package config

import (
	"encoding/base64"
	"strings"
	"testing"

	C "github.com/sagernet/sing-box/constant"
)

func TestParseConfigContent_VLESSXHTTPURIStaysVLESS(t *testing.T) {
	link := "vless://00000000-0000-0000-0000-000000000000@203.0.113.20:443?security=tls&sni=de.example&fp=firefox&type=xhttp&host=de.example&path=%2Fassets%2Fapi%2Fevents&mode=stream-up&alpn=h2#Germany"

	options, err := ParseConfigContentToOptions(link, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	if len(options.Outbounds) != 1 {
		t.Fatalf("outbounds len = %d, want 1", len(options.Outbounds))
	}
	outbound := options.Outbounds[0]
	if outbound.Type != C.TypeVLESS {
		t.Fatalf("outbound type = %q, want vless", outbound.Type)
	}
	if outbound.VLESSOptions.Transport == nil {
		t.Fatal("expected VLESS transport")
	}
	if outbound.VLESSOptions.Transport.Type != C.V2RayTransportTypeXHTTP {
		t.Fatalf("transport type = %q, want xhttp", outbound.VLESSOptions.Transport.Type)
	}
	if got := outbound.VLESSOptions.Transport.XHTTPOptions.Mode; got != "stream-up" {
		t.Fatalf("xhttp mode = %q, want stream-up", got)
	}
	if outbound.VLESSOptions.TLS == nil || outbound.VLESSOptions.TLS.UTLS == nil {
		t.Fatal("expected TLS uTLS options")
	}
	if got := outbound.VLESSOptions.TLS.UTLS.Fingerprint; got != "firefox" {
		t.Fatalf("fingerprint = %q, want firefox", got)
	}
	if got := []string(outbound.VLESSOptions.TLS.ALPN); len(got) != 1 || got[0] != "h2" {
		t.Fatalf("alpn = %#v, want [h2]", got)
	}
}

func TestParseConfigContent_NativeVLESSXHTTPJSONWithExtra(t *testing.T) {
	raw := `{
		"outbounds": [
			{
				"type": "vless",
				"tag": "Germany",
				"server": "203.0.113.20",
				"server_port": 443,
				"uuid": "00000000-0000-0000-0000-000000000000",
				"packet_encoding": "xudp",
				"tls": {
					"enabled": true,
					"server_name": "de.example",
					"alpn": ["h2"],
					"utls": {
						"enabled": true,
						"fingerprint": "firefox"
					}
				},
				"transport": {
					"type": "xhttp",
					"host": "de.example",
					"path": "/assets/api/events",
					"mode": "stream-up",
					"extra": {
						"noSSEHeader": false,
						"xPaddingBytes": "100-1000"
					}
				}
			}
		]
	}`

	options, err := ParseConfigContentToOptions(raw, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	transport := options.Outbounds[0].VLESSOptions.Transport
	if transport == nil || transport.Type != C.V2RayTransportTypeXHTTP {
		t.Fatalf("transport = %#v, want xhttp", transport)
	}
	if got := transport.XHTTPOptions.Extra["xPaddingBytes"]; got != "100-1000" {
		t.Fatalf("xhttp extra = %#v, want xPaddingBytes", transport.XHTTPOptions.Extra)
	}
}

func TestParseConfigContent_VLESSSplitHTTPURIStaysSplitHTTP(t *testing.T) {
	link := "vless://00000000-0000-0000-0000-000000000000@example.com:443?security=tls&sni=front.example&fp=chrome&type=splithttp&host=front.example&path=%2Fsplithttp&mode=stream-one#Split"

	options, err := ParseConfigContentToOptions(link, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	transport := options.Outbounds[0].VLESSOptions.Transport
	if transport == nil || transport.Type != C.V2RayTransportTypeSplitHTTP {
		t.Fatalf("transport = %#v, want splithttp", transport)
	}
	if got := transport.XHTTPOptions.Mode; got != "stream-one" {
		t.Fatalf("mode = %q, want stream-one", got)
	}
}

func TestParseConfigContent_VLESSH2URIUsesNativeH2Alias(t *testing.T) {
	link := "vless://00000000-0000-0000-0000-000000000000@example.com:443?security=tls&sni=front.example&type=h2&host=front.example&path=%2Fh2#H2"

	options, err := ParseConfigContentToOptions(link, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	transport := options.Outbounds[0].VLESSOptions.Transport
	if transport == nil || transport.Type != C.V2RayTransportTypeHTTP2 {
		t.Fatalf("transport = %#v, want h2", transport)
	}
	if got := transport.HTTPOptions.Path; got != "/h2" {
		t.Fatalf("path = %q, want /h2", got)
	}
}

func TestParseConfigContent_VLESSURIErrorDoesNotFallbackToRay2Sing(t *testing.T) {
	link := "vless://00000000-0000-0000-0000-000000000000@example.com:443?security=tls&type=xhttp&extra=%7Bbad#Broken"

	_, err := ParseConfigContentToOptions(link, false, DefaultMartenOptions(), false)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "invalid xhttp extra") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseConfigContent_Base64SingleVLESSProducesSelectableTag(t *testing.T) {
	raw := "vless://00000000-1111-2222-3333-444455556666@example.com:443" +
		"?security=tls&sni=example.server&fp=chrome&alpn=h2#Germany"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))

	options, err := ParseConfigContentToOptions(encoded, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	if len(options.Outbounds) != 1 {
		t.Fatalf("outbounds len = %d, want 1", len(options.Outbounds))
	}
	if options.Outbounds[0].Type != C.TypeVLESS {
		t.Fatalf("outbound type = %q, want vless", options.Outbounds[0].Type)
	}
	if options.Outbounds[0].Tag != "Germany § 0" {
		t.Fatalf("outbound tag = %q, want Germany § 0", options.Outbounds[0].Tag)
	}

	built, err := BuildConfig(*DefaultMartenOptions(), *options)
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}

	var selectorFound bool
	for _, out := range built.Outbounds {
		if out.Tag == OutboundSelectTag {
			if len(out.SelectorOptions.Outbounds) != 1 || out.SelectorOptions.Outbounds[0] != options.Outbounds[0].Tag {
				t.Fatalf("selector outbounds = %#v, want [%q]", out.SelectorOptions.Outbounds, options.Outbounds[0].Tag)
			}
			selectorFound = true
		}
		if out.Type == C.TypeURLTest {
			t.Fatalf("unexpected urltest group for single outbound content")
		}
	}
	if !selectorFound {
		t.Fatal("selector outbound not found")
	}
}
