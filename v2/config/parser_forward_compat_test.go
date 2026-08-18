package config

import (
	"encoding/json"
	"testing"
)

func TestParseConfigContentForwardCompatibleSubscriptionDropsOnlyUnsupportedBranches(t *testing.T) {
	const subscription = `{
		"outbounds": [
			{"type":"vless","tag":"first","server":"first.example.test","server_port":443,"uuid":"00000000-0000-0000-0000-000000000001","transport":{"type":"ws","path":"/edge","headers":{"Host":"first.example.test"},"future_websocket_extension":{"nested":true}}},
			{
				"type":"icmp",
				"tag":"icmp-wg",
				"server":"198.51.100.10",
				"backend":{"type":"wireguard","server_port":51820,"local_address":["10.0.0.2/32"],"private_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","peer_public_key":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="},
				"carrier":{"carrier_key":"carrier-key","payload":1200,"flows":6}
			},
			{"type":"socks","tag":"socks-uot","server":"socks.example.test","server_port":1080,"udp_over_tcp":{"enabled":true,"version":2,"future_uot_extension":{"nested":true}}},
			{"type":"future-protocol","tag":"future","nested_extension":{"revision":2}},
			{"type":"vless","tag":"malformed","server":"malformed.example.test","server_port":443},
			{"type":"vless","tag":"dependent","server":"dependent.example.test","server_port":443,"uuid":"00000000-0000-0000-0000-000000000002","detour":"future"},
			{"type":"selector","tag":"choose","outbounds":["future","first"],"default":"future"},
			{"type":"selector","tag":"future-group","outbounds":["future"]},
			{"type":"vless","tag":"second","server":"second.example.test","server_port":443,"uuid":"00000000-0000-0000-0000-000000000003","tls":{"enabled":true,"future_tls_extension":{"nested":true}}}
		]
	}`

	canonical, err := ParseConfigContent(subscription, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("forward-compatible mixed subscription must retain usable servers: %v", err)
	}
	var decoded struct {
		Outbounds []struct {
			Type      string   `json:"type"`
			Tag       string   `json:"tag"`
			Outbounds []string `json:"outbounds"`
			Default   *string  `json:"default"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		t.Fatalf("decode canonical config: %v", err)
	}
	if len(decoded.Outbounds) != 5 || decoded.Outbounds[0].Tag != "first" || decoded.Outbounds[1].Tag != "icmp-wg" || decoded.Outbounds[2].Tag != "socks-uot" || decoded.Outbounds[3].Tag != "choose" || decoded.Outbounds[4].Tag != "second" {
		t.Fatalf("usable outbounds must remain in source order, got %#v", decoded.Outbounds)
	}
	selector := decoded.Outbounds[3]
	if len(selector.Outbounds) != 1 || selector.Outbounds[0] != "first" || selector.Default != nil {
		t.Fatalf("selector must retain only surviving reference and drop stale default, got %#v", selector)
	}
}

func TestParseConfigContentForwardCompatibleSubscriptionFailsClosedWithoutSelectableOutbound(t *testing.T) {
	const subscription = `{
		"outbounds": [
			{"type":"future-protocol","tag":"future"},
			{"type":"vless","tag":"malformed","server":"malformed.example.test","server_port":443},
			{"type":"selector","tag":"future-group","outbounds":["future"]}
		]
	}`

	if _, err := ParseConfigContent(subscription, false, DefaultMartenOptions(), false); err == nil {
		t.Fatal("subscription without a selectable supported outbound must fail closed")
	}
}

func TestParseConfigContentForwardCompatibleSubscriptionArrayDropsUnsupportedEntries(t *testing.T) {
	const subscription = `[
		{"type":"vless","tag":"array-first","server":"first.example.test","server_port":443,"uuid":"00000000-0000-0000-0000-000000000001"},
		{"type":"future-protocol","tag":"future"},
		{"type":"vless","tag":"array-malformed","server":"malformed.example.test","server_port":443},
		"malformed array item",
		{"type":"socks","tag":"array-second","server":"second.example.test","server_port":1080}
	]`

	canonical, err := ParseConfigContent(subscription, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("forward-compatible array subscription must retain usable servers: %v", err)
	}
	var decoded struct {
		Outbounds []struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		t.Fatalf("decode canonical config: %v", err)
	}
	if len(decoded.Outbounds) != 2 || decoded.Outbounds[0].Type != "vless" || decoded.Outbounds[0].Tag != "array-first" || decoded.Outbounds[1].Type != "socks" || decoded.Outbounds[1].Tag != "array-second" {
		t.Fatalf("usable array outbounds must remain in source order, got %#v", decoded.Outbounds)
	}
}

func TestParseConfigContentFullConfigKeepsKnownOutboundFieldsStrict(t *testing.T) {
	const config = `{
		"outbounds": [
			{
				"type":"vless",
				"tag":"strict-vless",
				"server":"strict.example.test",
				"server_port":443,
				"uuid":"00000000-0000-0000-0000-000000000001",
				"future_extension":{"nested":true}
			}
		]
	}`

	if _, err := ParseConfigContent(config, false, DefaultMartenOptions(), true); err == nil {
		t.Fatal("full config must reject an unknown field on a known outbound")
	}
}
