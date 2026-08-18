package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/reddts/ray2sing/ray2sing"
	C "github.com/sagernet/sing-box/constant"
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

// sanitizeMartenSubscriptionOutbounds makes mixed subscriptions
// forward-compatible without weakening normal sing-box config decoding. Each
// outbound is decoded independently through the option schema known to this
// build; ordinary encoding/json intentionally ignores fields added by newer
// servers, while an unknown type or an invalid value drops only that entry.
// Detour chains and selector/urltest references are then pruned to the tags
// that survived. The app performs the same graph cleanup before persistence,
// but keeping this boundary in core also protects headless/native importers.
func sanitizeMartenSubscriptionOutbounds(jsonObj map[string]interface{}) bool {
	rawOutbounds, ok := jsonObj["outbounds"].([]interface{})
	if !ok {
		return false
	}

	dropped := false
	seenTags := make(map[string]struct{}, len(rawOutbounds))
	outbounds := make([]map[string]interface{}, 0, len(rawOutbounds))
	for _, rawOutbound := range rawOutbounds {
		outboundMap, ok := rawOutbound.(map[string]interface{})
		if !ok {
			dropped = true
			continue
		}
		tag, ok := outboundMap["tag"].(string)
		if !ok || tag == "" {
			dropped = true
			continue
		}
		if _, duplicate := seenTags[tag]; duplicate {
			dropped = true
			continue
		}
		canonical, err := decodeMartenSubscriptionOutbound(outboundMap)
		if err != nil {
			dropped = true
			continue
		}
		seenTags[tag] = struct{}{}
		outbounds = append(outbounds, canonical)
	}

	for {
		availableTags := make(map[string]struct{}, len(outbounds))
		for _, outbound := range outbounds {
			availableTags[outbound["tag"].(string)] = struct{}{}
		}

		changed := false
		retained := make([]map[string]interface{}, 0, len(outbounds))
		for _, outbound := range outbounds {
			tag := outbound["tag"].(string)
			if detour, _ := outbound["detour"].(string); detour != "" {
				if _, exists := availableTags[detour]; !exists {
					changed = true
					continue
				}
			}

			typeName, _ := outbound["type"].(string)
			if typeName == "selector" || typeName == "urltest" {
				rawReferences, ok := outbound["outbounds"].([]interface{})
				if !ok {
					changed = true
					continue
				}
				references := make([]interface{}, 0, len(rawReferences))
				referenceTags := make(map[string]struct{}, len(rawReferences))
				for _, rawReference := range rawReferences {
					reference, ok := rawReference.(string)
					if !ok || reference == tag {
						changed = true
						continue
					}
					if _, exists := availableTags[reference]; !exists {
						changed = true
						continue
					}
					references = append(references, reference)
					referenceTags[reference] = struct{}{}
				}
				if len(references) == 0 {
					changed = true
					continue
				}
				outbound["outbounds"] = references
				if typeName == "selector" {
					if defaultTag, _ := outbound["default"].(string); defaultTag != "" {
						if _, exists := referenceTags[defaultTag]; !exists {
							delete(outbound, "default")
							changed = true
						}
					}
				}
			}
			retained = append(retained, outbound)
		}
		outbounds = retained
		dropped = dropped || changed
		if !changed {
			break
		}
	}

	jsonObj["outbounds"] = outbounds
	return dropped
}

func hasMartenSelectableOutbound(jsonObj map[string]interface{}) bool {
	rawOutbounds, ok := jsonObj["outbounds"].([]map[string]interface{})
	if !ok {
		return false
	}
	for _, outbound := range rawOutbounds {
		typeName, _ := outbound["type"].(string)
		tag, _ := outbound["tag"].(string)
		if strings.Contains(tag, "§hide§") {
			continue
		}
		switch typeName {
		case "", "direct", "block", "dns", "selector", "urltest", "custom", "turncoat":
			continue
		default:
			return true
		}
	}
	return false
}

// salvageMartenSubscriptionOutbounds is the bounded fallback for values that
// decode structurally but fail deeper sing-box validation (for example a
// supported protocol entry missing required credentials). It validates each
// non-group outbound together with its detour chain, keeps the successful
// chains in source order, and rebuilds selector/urltest groups from surviving
// references. The normal path pays none of this per-outbound validation cost.
func salvageMartenSubscriptionOutbounds(jsonObj map[string]interface{}, configOpt *MartenOptions) bool {
	outbounds, ok := jsonObj["outbounds"].([]map[string]interface{})
	if !ok || len(outbounds) == 0 {
		return false
	}

	byTag := make(map[string]map[string]interface{}, len(outbounds))
	for _, outbound := range outbounds {
		byTag[outbound["tag"].(string)] = outbound
	}
	validTags := make(map[string]struct{}, len(outbounds))
	for _, outbound := range outbounds {
		typeName, _ := outbound["type"].(string)
		if typeName == "selector" || typeName == "urltest" {
			continue
		}
		chain, ok := martenOutboundDetourChain(outbound, byTag)
		if !ok || !validateMartenOutboundChain(jsonObj, outbounds, chain, configOpt) {
			continue
		}
		for tag := range chain {
			validTags[tag] = struct{}{}
		}
	}

	for {
		added := false
		for _, outbound := range outbounds {
			typeName, _ := outbound["type"].(string)
			if typeName != "selector" && typeName != "urltest" {
				continue
			}
			tag := outbound["tag"].(string)
			if _, exists := validTags[tag]; exists {
				continue
			}
			for _, rawReference := range outbound["outbounds"].([]interface{}) {
				reference, _ := rawReference.(string)
				if _, exists := validTags[reference]; exists && reference != tag {
					validTags[tag] = struct{}{}
					added = true
					break
				}
			}
		}
		if !added {
			break
		}
	}

	retained := make([]map[string]interface{}, 0, len(validTags))
	for _, outbound := range outbounds {
		tag := outbound["tag"].(string)
		if _, exists := validTags[tag]; !exists {
			continue
		}
		typeName, _ := outbound["type"].(string)
		if typeName == "selector" || typeName == "urltest" {
			references := make([]interface{}, 0)
			referenceTags := make(map[string]struct{})
			for _, rawReference := range outbound["outbounds"].([]interface{}) {
				reference, _ := rawReference.(string)
				if _, exists := validTags[reference]; exists && reference != tag {
					references = append(references, reference)
					referenceTags[reference] = struct{}{}
				}
			}
			if len(references) == 0 {
				continue
			}
			outbound["outbounds"] = references
			if typeName == "selector" {
				if defaultTag, _ := outbound["default"].(string); defaultTag != "" {
					if _, exists := referenceTags[defaultTag]; !exists {
						delete(outbound, "default")
					}
				}
			}
		}
		retained = append(retained, outbound)
	}
	jsonObj["outbounds"] = retained
	return len(retained) != len(outbounds)
}

func martenOutboundDetourChain(
	outbound map[string]interface{},
	byTag map[string]map[string]interface{},
) (map[string]struct{}, bool) {
	chain := make(map[string]struct{})
	current := outbound
	for {
		tag := current["tag"].(string)
		if _, cycle := chain[tag]; cycle {
			return nil, false
		}
		chain[tag] = struct{}{}
		detour, _ := current["detour"].(string)
		if detour == "" {
			return chain, true
		}
		next, exists := byTag[detour]
		if !exists {
			return nil, false
		}
		current = next
	}
}

func validateMartenOutboundChain(
	jsonObj map[string]interface{},
	outbounds []map[string]interface{},
	chain map[string]struct{},
	configOpt *MartenOptions,
) bool {
	candidate := make(map[string]interface{}, len(jsonObj))
	for key, value := range jsonObj {
		if key != "outbounds" {
			candidate[key] = value
		}
	}
	candidateOutbounds := make([]map[string]interface{}, 0, len(chain))
	for _, outbound := range outbounds {
		if _, exists := chain[outbound["tag"].(string)]; exists {
			candidateOutbounds = append(candidateOutbounds, outbound)
		}
	}
	candidate["outbounds"] = candidateOutbounds
	content, err := json.Marshal(candidate)
	if err != nil {
		return false
	}
	_, err = patchConfig(content, "SingboxSubscriptionEntry", configOpt)
	return err == nil
}

func decodeMartenSubscriptionOutbound(raw map[string]interface{}) (map[string]interface{}, error) {
	typeName, ok := raw["type"].(string)
	if !ok || strings.TrimSpace(typeName) == "" {
		return nil, fmt.Errorf("missing outbound type")
	}
	tag, ok := raw["tag"].(string)
	if !ok || tag == "" {
		return nil, fmt.Errorf("missing outbound tag")
	}

	outbound := option.Outbound{Type: strings.ToLower(typeName), Tag: tag}
	rawOptions, err := outbound.RawOptions()
	if err != nil {
		return nil, err
	}
	if rawOptions != nil {
		optionsMap := make(map[string]interface{}, len(raw))
		for key, value := range raw {
			if key == "type" || key == "tag" {
				continue
			}
			optionsMap[key] = value
		}
		if err = canonicalizeMartenNestedOutboundOptions(outbound.Type, optionsMap); err != nil {
			return nil, err
		}
		optionsJSON, err := json.Marshal(optionsMap)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(optionsJSON, rawOptions); err != nil {
			return nil, err
		}
		if err = validateMartenSubscriptionOutbound(outbound.Type, rawOptions); err != nil {
			return nil, err
		}
	}

	canonicalJSON, err := json.Marshal(&outbound)
	if err != nil {
		return nil, err
	}
	var canonical map[string]interface{}
	if err = json.Unmarshal(canonicalJSON, &canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

// A small number of option unions implement their own strict UnmarshalJSON.
// Canonicalize those unions separately with ordinary encoding/json so future
// fields nested inside an otherwise supported outbound are ignored just like
// future fields at the outbound's top level. Unknown union variants still
// fail and therefore discard only their containing outbound.
func canonicalizeMartenNestedOutboundOptions(typeName string, options map[string]interface{}) error {
	switch typeName {
	case "vless", "vmess", "trojan":
		if rawTransport, exists := options["transport"]; exists {
			canonical, err := canonicalizeMartenV2RayTransport(rawTransport)
			if err != nil {
				return err
			}
			options["transport"] = canonical
		}
	}
	switch typeName {
	case "socks", "shadowsocks", "ssh", "xray":
		if rawUDPOverTCP, exists := options["udp_over_tcp"]; exists {
			canonical, err := canonicalizeMartenUDPOverTCP(rawUDPOverTCP)
			if err != nil {
				return err
			}
			options["udp_over_tcp"] = canonical
		}
	}
	return nil
}

func canonicalizeMartenV2RayTransport(raw interface{}) (interface{}, error) {
	content, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var header struct {
		Type string `json:"type"`
	}
	if err = json.Unmarshal(content, &header); err != nil {
		return nil, err
	}

	transport := option.V2RayTransportOptions{Type: header.Type}
	var selectedOptions interface{}
	switch header.Type {
	case C.V2RayTransportTypeHTTP, C.V2RayTransportTypeHTTP2:
		selectedOptions = &transport.HTTPOptions
	case C.V2RayTransportTypeWebsocket:
		selectedOptions = &transport.WebsocketOptions
	case C.V2RayTransportTypeQUIC:
		selectedOptions = &transport.QUICOptions
	case C.V2RayTransportTypeGRPC:
		selectedOptions = &transport.GRPCOptions
	case C.V2RayTransportTypeHTTPUpgrade:
		selectedOptions = &transport.HTTPUpgradeOptions
	case C.V2RayTransportTypeXHTTP, C.V2RayTransportTypeSplitHTTP:
		selectedOptions = &transport.XHTTPOptions
	default:
		return nil, fmt.Errorf("unknown transport type: %s", header.Type)
	}
	if err = json.Unmarshal(content, selectedOptions); err != nil {
		return nil, err
	}
	return marshalMartenCanonicalJSON(transport)
}

func canonicalizeMartenUDPOverTCP(raw interface{}) (interface{}, error) {
	if _, ok := raw.(bool); ok {
		return raw, nil
	}
	content, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Enabled bool  `json:"enabled,omitempty"`
		Version uint8 `json:"version,omitempty"`
	}
	if err = json.Unmarshal(content, &decoded); err != nil {
		return nil, err
	}
	options := option.UDPOverTCPOptions{
		Enabled: decoded.Enabled,
		Version: decoded.Version,
	}
	return marshalMartenCanonicalJSON(options)
}

func marshalMartenCanonicalJSON(value interface{}) (interface{}, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var canonical interface{}
	if err = json.Unmarshal(content, &canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

// validateMartenSubscriptionOutbound rejects zero values for fields that are
// required by the supported wire protocols but are not enforced by
// encoding/json (and, for some outbounds, by libbox.CheckConfig). Keeping this
// check beside the tolerant per-entry decoder lets a malformed known server be
// discarded without turning the whole mixed subscription into an error.
func validateMartenSubscriptionOutbound(typeName string, rawOptions any) error {
	if serverOptions, ok := rawOptions.(option.ServerOptionsWrapper); ok {
		switch typeName {
		case "wireguard", "amneziawg":
			// The modern WireGuard schema may put endpoints in peers instead of
			// the deprecated top-level ServerOptions fields.
		default:
			server := serverOptions.TakeServerOptions()
			if strings.TrimSpace(server.Server) == "" || server.ServerPort == 0 {
				return fmt.Errorf("missing outbound server endpoint")
			}
		}
	}

	switch typeName {
	case "shadowsocks":
		options := rawOptions.(*option.ShadowsocksOutboundOptions)
		if strings.TrimSpace(options.Method) == "" || options.Password == "" {
			return fmt.Errorf("missing shadowsocks credentials")
		}
	case "shadowsocksr":
		options := rawOptions.(*option.ShadowsocksROutboundOptions)
		if strings.TrimSpace(options.Method) == "" || options.Password == "" {
			return fmt.Errorf("missing shadowsocksr credentials")
		}
	case "vmess":
		options := rawOptions.(*option.VMessOutboundOptions)
		if _, err := uuid.Parse(options.UUID); err != nil {
			return fmt.Errorf("invalid vmess uuid")
		}
	case "vless":
		options := rawOptions.(*option.VLESSOutboundOptions)
		if _, err := uuid.Parse(options.UUID); err != nil {
			return fmt.Errorf("invalid vless uuid")
		}
	case "trojan":
		if rawOptions.(*option.TrojanOutboundOptions).Password == "" {
			return fmt.Errorf("missing trojan password")
		}
	case "wireguard", "amneziawg":
		options := rawOptions.(*option.WireGuardOutboundOptions)
		if options.PrivateKey == "" || len(options.LocalAddress) == 0 || !hasMartenWireGuardEndpoint(options) {
			return fmt.Errorf("incomplete wireguard outbound")
		}
	case "xray":
		options := rawOptions.(*option.XrayOutboundOptions)
		if options.XConfig == nil && options.DeprecatedXrayOutboundJson == nil {
			return fmt.Errorf("missing xray outbound config")
		}
	case "turncoat":
		if len(rawOptions.(*option.TurncoatOutboundOptions).CallURLs) == 0 {
			return fmt.Errorf("missing turncoat call url")
		}
	case "icmp":
		options := rawOptions.(*option.ICMPOutboundOptions)
		if !hasMartenICMPServer(options) || options.Backend.Type != "wireguard" ||
			options.Backend.PrivateKey == "" || len(options.Backend.LocalAddress) == 0 ||
			!hasMartenICMPWireGuardPeer(&options.Backend.WireGuardOutboundOptions) || options.Carrier.CarrierKey == "" {
			return fmt.Errorf("incomplete icmp outbound")
		}
	}
	return nil
}

func hasMartenWireGuardEndpoint(options *option.WireGuardOutboundOptions) bool {
	if strings.TrimSpace(options.Server) != "" && options.ServerPort != 0 && options.PeerPublicKey != "" {
		return true
	}
	for _, peer := range options.Peers {
		if strings.TrimSpace(peer.Server) != "" && peer.ServerPort != 0 && peer.PublicKey != "" {
			return true
		}
	}
	return false
}

func hasMartenICMPWireGuardPeer(options *option.WireGuardOutboundOptions) bool {
	// The ICMP carrier supplies the endpoint address from the outer `server`
	// field, so its embedded WireGuard config intentionally has only the port
	// and peer key at the top level.
	if options.ServerPort != 0 && options.PeerPublicKey != "" {
		return true
	}
	for _, peer := range options.Peers {
		if peer.ServerPort != 0 && peer.PublicKey != "" {
			return true
		}
	}
	return false
}

func hasMartenICMPServer(options *option.ICMPOutboundOptions) bool {
	if strings.TrimSpace(options.Server) != "" {
		return true
	}
	for _, server := range options.Servers {
		if strings.TrimSpace(server) != "" {
			return true
		}
	}
	return false
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
		if converted, handled, convertErr := xraySubscriptionToSingbox(tmpJsonResult); handled {
			if convertErr != nil {
				return nil, convertErr
			}
			if fullConfig || configOpt.EnableFullConfig {
				return patchConfig(converted, "XraySubscriptionParser", configOpt)
			}
			// Feed the converted entries through the same independent
			// subscription sanitizer and bounded salvage path. One malformed
			// embedded Xray profile must not discard its valid siblings.
			return ParseConfigContent(string(converted), debug, configOpt, false)
		}
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
		} else if jsonArray, ok := tmpJsonResult.([]interface{}); ok {
			jsonObj["outbounds"] = jsonArray
		} else {
			return nil, fmt.Errorf("[SingboxParser] Incorrect Json Format")
		}

		stripMartenOutboundLeakKeys(jsonObj)
		isFullConfig := fullConfig || configOpt.EnableFullConfig
		if !isFullConfig {
			if dropped := sanitizeMartenSubscriptionOutbounds(jsonObj); dropped && !hasMartenSelectableOutbound(jsonObj) {
				return nil, fmt.Errorf("[SingboxParser] no supported selectable outbounds")
			}
		}
		newContent, _ := json.MarshalIndent(jsonObj, "", "  ")
		patchedContent, patchErr := patchConfig(newContent, "SingboxParser", configOpt)
		if patchErr == nil || isFullConfig {
			return patchedContent, patchErr
		}
		if !salvageMartenSubscriptionOutbounds(jsonObj, configOpt) || !hasMartenSelectableOutbound(jsonObj) {
			return nil, patchErr
		}
		newContent, _ = json.MarshalIndent(jsonObj, "", "  ")
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
