package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/reddts/ray2sing/ray2sing"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	SJ "github.com/sagernet/sing/common/json"
	"github.com/xmdhs/clash2singbox/convert"
	"github.com/xmdhs/clash2singbox/model/clash"
	"gopkg.in/yaml.v3"
)

//go:embed config.json.template
var configByte []byte

// martenTopLevelPassThrough lists Marten extension keys that must survive the
// non-fullConfig stripping pass in ParseConfigContent. Add a key here when
// marten-sing-box's option.Options gains a new top-level field that
// subscriptions are allowed to set.
var martenTopLevelPassThrough = []string{
	"providers", // marten-sing-box: top-level TurncoatProviders fallback
}

// martenOutboundUnknownLeakKeys are keys that older or buggy versions of
// marten-server accidentally merged into per-outbound JSON (server-side
// bookkeeping for the device credential). sing-box decodes outbound options
// with DisallowUnknownFields(), so any of these in an outbound aborts the
// whole config with `unmarshal error: outbounds[N].<key>: unknown field`.
// We strip them defensively before strict unmarshal so previously cached
// subscriptions and old servers don't break the client.
var martenOutboundUnknownLeakKeys = []string{
	"credential_id",
	"device_secret",
	"credential_expires_at",
}

// stripMartenOutboundLeakKeys removes known server-side leak keys from each
// element of the top-level `outbounds` array of a sing-box config. It is a
// no-op if `outbounds` is missing or not an array. Only the explicit allowlist
// in martenOutboundUnknownLeakKeys is touched — anything else still surfaces
// as a strict-decode error so real config bugs aren't silently swallowed.
func stripMartenOutboundLeakKeys(jsonObj map[string]interface{}) {
	raw, ok := jsonObj["outbounds"]
	if !ok {
		return
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return
	}
	for _, item := range arr {
		out, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range martenOutboundUnknownLeakKeys {
			delete(out, k)
		}
	}
}

func ParseConfig(path string, debug bool) ([]byte, error) {
	content, err := os.ReadFile(path)
	os.Chdir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return ParseConfigContent(string(content), debug, nil, false)
}

func ParseConfigContentToOptions(contentstr string, debug bool, configOpt *MartenOptions, fullConfig bool) (*option.Options, error) {
	content, err := ParseConfigContent(contentstr, debug, configOpt, fullConfig)
	if err != nil {
		return nil, err
	}
	var options option.Options
	err = json.Unmarshal(content, &options)
	if err != nil {
		return nil, err
	}
	return &options, nil
}

func ParseConfigContent(contentstr string, debug bool, configOpt *MartenOptions, fullConfig bool) ([]byte, error) {
	if configOpt == nil {
		configOpt = DefaultMartenOptions()
	}
	content := []byte(contentstr)
	var jsonObj map[string]interface{} = make(map[string]interface{})

	fmt.Printf("Convert using json\n")
	var tmpJsonResult any
	jsonDecoder := json.NewDecoder(SJ.NewCommentFilter(bytes.NewReader(content)))
	if err := jsonDecoder.Decode(&tmpJsonResult); err == nil {
		if tmpJsonObj, ok := tmpJsonResult.(map[string]interface{}); ok {
			if tmpJsonObj["outbounds"] == nil {
				jsonObj["outbounds"] = []interface{}{jsonObj}
			} else {
				if fullConfig || (configOpt != nil && configOpt.EnableFullConfig) {
					jsonObj = tmpJsonObj
				} else {
					jsonObj["outbounds"] = tmpJsonObj["outbounds"]
					// Marten extension fields: preserve top-level keys that
					// marten-sing-box understands (e.g. `providers` as a
					// shared TURNcoat auth-backend fallback list). Keep this
					// list small and explicit — we don't want to leak full
					// experimental/route configs in non-fullConfig mode.
					for _, k := range martenTopLevelPassThrough {
						if v, ok := tmpJsonObj[k]; ok {
							jsonObj[k] = v
						}
					}
				}
			}
		} else if jsonArray, ok := tmpJsonResult.([]map[string]interface{}); ok {
			jsonObj["outbounds"] = jsonArray
		} else {
			return nil, fmt.Errorf("[SingboxParser] Incorrect Json Format")
		}

		stripMartenOutboundLeakKeys(jsonObj)
		newContent, _ := json.MarshalIndent(jsonObj, "", "  ")

		return patchConfig(newContent, "SingboxParser", configOpt)
	}

	// Marten extension: a subscription whose every line is a `turncoat://`
	// URI is parsed natively before falling through to ray2sing — ray2sing
	// has no notion of the turncoat scheme and would reject such content.
	// On any error (mixed protocols, bad payload, no turncoat URIs at all)
	// we silently fall through; ray2sing then gets a fair chance.
	if turncoatStr, tcErr := Turncoat2Sing(string(content)); tcErr == nil {
		return patchConfig([]byte(turncoatStr), "TurncoatParser", configOpt)
	}

	if vlessStr, handled, vlessErr := shareLinks2SingNoAutoVLESS(string(content)); handled {
		if vlessErr != nil {
			return nil, vlessErr
		}
		return patchConfig([]byte(vlessStr), "VLESSParser", configOpt)
	}

	v2rayStr, err := ray2sing.Ray2Singbox(string(content), configOpt.UseXrayCoreWhenPossible)
	if err == nil {
		return patchConfig([]byte(v2rayStr), "V2rayParser", configOpt)
	}
	fmt.Printf("Convert using clash\n")
	clashObj := clash.Clash{}
	if err := yaml.Unmarshal(content, &clashObj); err == nil && clashObj.Proxies != nil {
		if len(clashObj.Proxies) == 0 {
			return nil, fmt.Errorf("[ClashParser] no outbounds found")
		}
		converted, err := convert.Clash2sing(clashObj)
		if err != nil {
			return nil, fmt.Errorf("[ClashParser] converting clash to sing-box error: %w", err)
		}
		output := configByte
		output, err = convert.Patch(output, converted, "", "", nil)
		if err != nil {
			return nil, fmt.Errorf("[ClashParser] patching clash config error: %w", err)
		}
		return patchConfig(output, "ClashParser", configOpt)
	}

	return nil, fmt.Errorf("unable to determine config format")
}

func patchConfig(content []byte, name string, configOpt *MartenOptions) ([]byte, error) {
	options := option.Options{}
	err := json.Unmarshal(content, &options)
	if err != nil {
		return nil, fmt.Errorf("[SingboxParser] unmarshal error: %w", err)
	}
	for i := range options.Outbounds {
		patchAmneziaWGKeepalive(&options.Outbounds[i])
	}

	content, _ = json.MarshalIndent(options, "", "  ")

	// fmt.Printf("%s\n", content)
	return validateResult(content, name)
}

func validateResult(content []byte, name string) ([]byte, error) {
	err := libbox.CheckConfig(string(content))
	if err != nil {
		return nil, fmt.Errorf("[%s] invalid sing-box config: %w", name, err)
	}
	return content, nil
}
