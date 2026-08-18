package config

import (
	"encoding/json"
	"testing"

	C "github.com/sagernet/sing-box/constant"
)

func TestParseConfigContent_ClashVLESSQuotedPortWithMetadata(t *testing.T) {
	raw := `proxies:
  - name: "Format-Test"
    type: vless
    server: 203.0.113.10
    port: "443"
    uuid: 00000000-1111-2222-3333-444455556666
    tls: true
    servername: example.test
    network: tcp
`

	canonical, err := ParseConfigContent(raw, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(canonical, &payload); err != nil {
		t.Fatalf("unmarshal canonical json: %v", err)
	}
	outboundsRaw, ok := payload["outbounds"].([]interface{})
	if !ok {
		t.Fatal("canonical json missing outbounds")
	}

	outbound := findOutboundByTag(outboundsRaw, "Format-Test")
	if outbound == nil {
		t.Fatal("vless outbound with tag 'Format-Test' not found")
	}

	if got := outbound["type"]; got != C.TypeVLESS {
		t.Fatalf("outbound type = %v, want %q", got, C.TypeVLESS)
	}
}

func TestParseConfigContent_ClashShadowsocksQuotedPort(t *testing.T) {
	raw := `proxies:
  - name: "SS Format"
    type: ss
    server: 198.51.100.30
    port: "8388"
    cipher: "aes-256-gcm"
    password: "synthetic-passphrase"
`

	canonical, err := ParseConfigContent(raw, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(canonical, &payload); err != nil {
		t.Fatalf("unmarshal canonical json: %v", err)
	}
	outboundsRaw, ok := payload["outbounds"].([]interface{})
	if !ok {
		t.Fatal("canonical json missing outbounds")
	}

	outbound := findOutboundByTag(outboundsRaw, "SS Format")
	if outbound == nil {
		t.Fatal("shadowsocks outbound with tag 'SS Format' not found")
	}

	if got := outbound["type"]; got != C.TypeShadowsocks {
		t.Fatalf("outbound type = %v, want %q", got, C.TypeShadowsocks)
	}
}

func findOutboundByTag(outbounds []interface{}, tag string) map[string]interface{} {
	for _, item := range outbounds {
		out, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if out["tag"] == tag {
			return out
		}
	}
	return nil
}
