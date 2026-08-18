package config

import (
	"encoding/json"
	"fmt"
	"testing"
)

func syntheticXrayProfile(tag string, index int) map[string]interface{} {
	return map[string]interface{}{
		"remarks": tag,
		"routing": map[string]interface{}{
			"domain_strategy": "AsIs",
		},
		"outbounds": []any{
			map[string]interface{}{
				"protocol": "vless",
				"tag":      tag,
				"settings": map[string]interface{}{
					"vnext": []any{
						map[string]interface{}{
							"address": fmt.Sprintf("203.0.113.%d", index+10),
							"port":    443,
							"users": []any{
								map[string]interface{}{
									"id":         "00000000-1111-2222-3333-444455556666",
									"encryption": "none",
								},
							},
						},
					},
				},
				"streamSettings": map[string]interface{}{
					"network":  "tcp",
					"security": "tls",
					"tlsSettings": map[string]interface{}{
						"serverName": "203.0.113.1",
					},
				},
			},
		},
		"dns": map[string]interface{}{
			"servers": []any{
				map[string]interface{}{"address": fmt.Sprintf("https://dns%d.example.test:443/dns-query", index)},
			},
		},
	}
}

func TestParseConfigContent_XraySubscriptionArrayCreatesSeparateXrayOutbounds(t *testing.T) {
	names := []string{
		"🇩🇪 Berlin #0",
		"🇫🇷 Paris #1",
		"🇮🇹 Milan #2",
		"🇪🇸 Madrid #3",
		"🇬🇧 London #4",
		"🇳🇱 Amsterdam #5",
		"🇵🇱 Warsaw #6",
		"🇷🇺 Kyiv #7",
		"🇨🇭 Zurich #8",
	}
	profiles := make([]any, 0, len(names))
	for index, tag := range names {
		profiles = append(profiles, syntheticXrayProfile(tag, index))
	}
	raw, err := json.Marshal(profiles)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	converted, err := ParseConfigContent(string(raw), false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(converted, &decoded); err != nil {
		t.Fatalf("unmarshal converted config: %v", err)
	}
	parsedOutbounds := decoded["outbounds"]
	outboundList, ok := parsedOutbounds.([]interface{})
	if !ok {
		t.Fatalf("outbounds is %T, want []interface{}", parsedOutbounds)
	}
	if got := len(outboundList); got != 9 {
		t.Fatalf("outbound count = %d, want 9", got)
	}

	found := map[string]bool{}
	for index, item := range outboundList {
		outbound, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("outbound[%d] is %T, want map[string]interface{}", index, item)
		}
		if got := outbound["type"]; got != "xray" {
			t.Fatalf("outbound[%d].type = %v, want xray", index, got)
		}
		tag, ok := outbound["tag"].(string)
		if !ok {
			t.Fatalf("outbound[%d].tag is %T, want string", index, outbound["tag"])
		}
		if tag == "" {
			t.Fatalf("outbound[%d].tag is empty", index)
		}
		found[tag] = true
		xconfig, ok := outbound["xconfig"].(map[string]interface{})
		if !ok {
			t.Fatalf("outbound[%d].xconfig is %T, want map[string]interface{}", index, outbound["xconfig"])
		}
		routing, ok := xconfig["routing"].(map[string]interface{})
		if !ok {
			t.Fatalf("xconfig for outbound[%d] is missing routing", index)
		}
		if _, ok := xconfig["dns"]; !ok {
			t.Fatalf("xconfig for outbound[%d] is missing dns", index)
		}
		if routing["domain_strategy"] != "AsIs" {
			t.Fatalf("xconfig[%d] routing.domain_strategy = %v, want AsIs", index, routing["domain_strategy"])
		}
	}
	for _, name := range names {
		if !found[name] {
			t.Fatalf("missing preserved outbound tag %q", name)
		}
	}
}

func TestParseConfigContent_XraySubscriptionSingleProfileSupported(t *testing.T) {
	raw, err := json.Marshal(syntheticXrayProfile("🇩🇪 Berlin", 11))
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	converted, err := ParseConfigContent(string(raw), false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(converted, &decoded); err != nil {
		t.Fatalf("unmarshal converted config: %v", err)
	}
	outboundList, ok := decoded["outbounds"].([]interface{})
	if !ok || len(outboundList) != 1 {
		t.Fatalf("outbounds = %v, want one item", decoded["outbounds"])
	}
	outbound := outboundList[0].(map[string]interface{})
	if outbound["type"] != "xray" {
		t.Fatalf("outbound type = %v, want xray", outbound["type"])
	}
	if outbound["tag"] != "🇩🇪 Berlin" {
		t.Fatalf("outbound tag = %v, want 🇩🇪 Berlin", outbound["tag"])
	}
}

func TestParseConfigContent_SingboxJSONIsNotInterceptedByXrayAdapter(t *testing.T) {
	raw := `{
		"outbounds": [{
			"type": "vless",
			"tag": "direct",
			"server": "203.0.113.10",
			"server_port": 443,
			"uuid": "00000000-1111-2222-3333-444455556666"
		}]
	}`
	converted, err := ParseConfigContent(raw, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(converted, &decoded); err != nil {
		t.Fatalf("unmarshal converted config: %v", err)
	}
	outboundList, ok := decoded["outbounds"].([]interface{})
	if !ok || len(outboundList) != 1 {
		t.Fatalf("outbounds = %v, want one item", decoded["outbounds"])
	}
	outbound := outboundList[0].(map[string]interface{})
	if outbound["type"] != "vless" {
		t.Fatalf("outbound type = %v, want vless", outbound["type"])
	}
	if _, ok := outbound["xconfig"]; ok {
		t.Fatalf("non-xray subscription produced unexpected xconfig: %v", outbound)
	}
}

func TestParseConfigContent_XraySubscriptionMixedArraySkipsForeignAndMalformedProfiles(t *testing.T) {
	mixed := []any{
		syntheticXrayProfile("🇩🇪 Berlin", 1),
		map[string]interface{}{
			"outbounds": []any{
				map[string]interface{}{"type": "direct", "tag": "invalid"},
			},
		},
		map[string]interface{}{
			"type":        "vless",
			"tag":         "mixed-vless",
			"server":      "vless.example.test",
			"server_port": 443,
			"uuid":        "00000000-1111-2222-3333-444455556666",
		},
		map[string]interface{}{"outbounds": "not an array"},
		"malformed profile",
		syntheticXrayProfile("🇫🇷 Paris", 2),
	}
	raw, err := json.Marshal(mixed)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	converted, err := ParseConfigContent(string(raw), false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	var decoded struct {
		Outbounds []struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(converted, &decoded); err != nil {
		t.Fatalf("decode converted config: %v", err)
	}
	if len(decoded.Outbounds) != 3 || decoded.Outbounds[0].Type != "xray" || decoded.Outbounds[0].Tag != "🇩🇪 Berlin" || decoded.Outbounds[1].Type != "vless" || decoded.Outbounds[1].Tag != "mixed-vless" || decoded.Outbounds[2].Type != "xray" || decoded.Outbounds[2].Tag != "🇫🇷 Paris" {
		t.Fatalf("usable Xray profiles must remain in source order, got %#v", decoded.Outbounds)
	}
}

func xrayProfileWithUnusableInboundsAndLeastLoad(tag string, index int) map[string]interface{} {
	serverAddress := fmt.Sprintf("203.0.113.%d", index+10)
	proxyTag := fmt.Sprintf("proxy-%d", index)
	singleProxyTag := fmt.Sprintf("single-%d", index)
	loopbackTag := fmt.Sprintf("loopback-%d", index)
	serviceTag := fmt.Sprintf("service-%d", index)

	return map[string]interface{}{
		"remarks": tag,
		"inbounds": []interface{}{
			map[string]interface{}{
				"tag":      "incoming",
				"protocol": "dokodemo-door",
				"port":     1080,
			},
			map[string]interface{}{
				"tag":      loopbackTag,
				"protocol": "loopback",
			},
			map[string]interface{}{
				"tag":      serviceTag,
				"protocol": "service",
			},
		},
		"outbounds": []interface{}{
			map[string]interface{}{
				"protocol": "vless",
				"tag":      proxyTag,
				"settings": map[string]interface{}{
					"vnext": []interface{}{
						map[string]interface{}{
							"address": serverAddress,
							"port":    443,
							"users": []interface{}{
								map[string]interface{}{
									"id":         "00000000-1111-2222-3333-444455556666",
									"encryption": "none",
								},
							},
						},
					},
				},
			},
			map[string]interface{}{
				"protocol": "vless",
				"tag":      singleProxyTag,
				"settings": map[string]interface{}{
					"vnext": []interface{}{
						map[string]interface{}{
							"address": serverAddress,
							"port":    443,
							"users": []interface{}{
								map[string]interface{}{
									"id":         "00000000-1111-2222-3333-444455556666",
									"encryption": "none",
								},
							},
						},
					},
				},
			},
			map[string]interface{}{
				"protocol": "loopback",
				"tag":      loopbackTag,
				"settings": map[string]interface{}{
					"network": "tcp",
				},
			},
		},
		"routing": map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{
					"type":        "field",
					"outboundTag": proxyTag,
				},
				map[string]interface{}{
					"type":        "field",
					"inboundTag":  []interface{}{loopbackTag},
					"outboundTag": proxyTag,
				},
			},
			"balancers": []interface{}{
				map[string]interface{}{
					"tag":         "lb-single",
					"selector":    []interface{}{singleProxyTag},
					"fallbackTag": singleProxyTag,
				},
				map[string]interface{}{
					"tag":         "lb-multi",
					"selector":    []interface{}{proxyTag, singleProxyTag},
					"fallbackTag": proxyTag,
				},
			},
		},
	}
}

func TestParseConfigContent_XraySubscriptionArrayFiltersUnusableInboundsAndNormalizesLeastLoadFallback(t *testing.T) {
	const (
		proxyTag       = "proxy-21"
		singleProxyTag = "single-21"
		loopbackTag    = "loopback-21"
	)
	raw, err := json.Marshal([]map[string]interface{}{xrayProfileWithUnusableInboundsAndLeastLoad("🇩🇪 Berlin 0", 21)})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	converted, err := ParseConfigContent(string(raw), false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(converted, &decoded); err != nil {
		t.Fatalf("unmarshal converted config: %v", err)
	}

	outbounds, ok := decoded["outbounds"].([]interface{})
	if !ok || len(outbounds) != 1 {
		t.Fatalf("outbounds = %v, want one item", decoded["outbounds"])
	}
	xray, ok := outbounds[0].(map[string]interface{})
	if !ok {
		t.Fatalf("xray outbound is %T, want map[string]interface{}", outbounds[0])
	}
	if got := xray["tag"]; got != "🇩🇪 Berlin 0" {
		t.Fatalf("xray profile tag = %v, want 🇩🇪 Berlin 0", got)
	}
	config, ok := xray["xconfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("xray profile xconfig = %T, want map[string]interface{}", xray["xconfig"])
	}

	if _, hasInbounds := config["inbounds"]; hasInbounds {
		t.Fatalf("xconfig.inbounds should be removed by normalization")
	}

	routing, ok := config["routing"].(map[string]interface{})
	if !ok {
		t.Fatalf("xconfig.routing = %T, want map[string]interface{}", config["routing"])
	}
	rules, ok := routing["rules"].([]interface{})
	if !ok {
		t.Fatalf("xconfig.routing.rules = %T, want []interface{}", routing["rules"])
	}
	for _, rule := range rules {
		ruleMap, ok := rule.(map[string]interface{})
		if !ok {
			t.Fatalf("routing rule is %T, want map[string]interface{}", rule)
		}
		rawInbound := ruleMap["inboundTag"]
		rawOutboundTag := fmt.Sprint(ruleMap["outboundTag"])
		if rawOutboundTag == loopbackTag {
			t.Fatalf("unusable outbound tag should be removed from routing rules")
		}
		if rawInbound == nil {
			continue
		}
		inboundTags, ok := rawInbound.([]interface{})
		if !ok {
			t.Fatalf("routing rule inboundTag is %T, want []interface{}", rawInbound)
		}
		for _, inbound := range inboundTags {
			if inbound == "loopback-21" || inbound == "service-21" {
				t.Fatalf("unusable inbound tags should be removed from routing rule: %v", inboundTags)
			}
		}
	}

	if rawBalancers, ok := routing["balancers"].([]interface{}); ok {
		for _, item := range rawBalancers {
			balancer, ok := item.(map[string]interface{})
			if !ok {
				t.Fatalf("balancer is %T, want map[string]interface{}", item)
			}
			tag, _ := balancer["tag"].(string)
			fallback := fmt.Sprint(balancer["fallbackTag"])
			if tag == "lb-single" && fallback != singleProxyTag {
				t.Fatalf("lb-single fallbackTag %q, want %q", fallback, singleProxyTag)
			}
			if tag == "lb-multi" && fallback != proxyTag && fallback != singleProxyTag {
				t.Fatalf("lb-multi fallbackTag %q, want either %q or %q", fallback, proxyTag, singleProxyTag)
			}
		}
	}

	xrayOutbounds, ok := config["outbounds"].([]interface{})
	if !ok {
		t.Fatalf("xconfig.outbounds = %T, want []interface{}", config["outbounds"])
	}
	knownProxies := map[string]struct{}{}
	for _, item := range xrayOutbounds {
		outboundMap, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("xconfig outbound is %T, want map[string]interface{}", item)
		}
		protocol, _ := outboundMap["protocol"].(string)
		tag := fmt.Sprint(outboundMap["tag"])
		if protocol == "loopback" {
			t.Fatalf("loopback outbound %q should be removed during normalization", tag)
		}
		if protocol != "" {
			knownProxies[tag] = struct{}{}
		}
	}
	if _, ok := knownProxies[proxyTag]; !ok {
		t.Fatalf("proxy outbound %q is missing after normalization", proxyTag)
	}
	if _, ok := knownProxies[singleProxyTag]; !ok {
		t.Fatalf("proxy outbound %q is missing after normalization", singleProxyTag)
	}
}
