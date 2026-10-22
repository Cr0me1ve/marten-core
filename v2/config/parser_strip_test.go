package config

import (
	"strings"
	"testing"
)

// TestParseConfigContent_StripsLeakedCredentialFields ensures the parser
// silently discards server-side credential bookkeeping (`credential_id`,
// `device_secret`, `credential_expires_at`) that older backend builds
// merged into per-outbound JSON. sing-box decodes outbound options with
// DisallowUnknownFields(), so without this strip a single legacy outbound
// fails the whole config with `unmarshal error: outbounds[0].credential_expires_at`.
//
// This guards both fresh subscription downloads (URL import) and any cached
// envelopes the client wrote to disk before the server fix landed.
func TestParseConfigContent_StripsLeakedCredentialFields(t *testing.T) {
	const sub = `{
		"outbounds": [
			{
				"type": "direct",
				"tag": "out",
				"credential_id": "cred-1",
				"device_secret": "shhh",
				"credential_expires_at": "2030-01-01T00:00:00Z"
			}
		]
	}`
	configOpt := DefaultMartenOptions()
	configOpt.EnableFullConfig = false
	out, err := ParseConfigContent(sub, false, configOpt, false)
	if err != nil {
		t.Fatalf("ParseConfigContent: %v", err)
	}
	for _, k := range martenOutboundUnknownLeakKeys {
		if strings.Contains(string(out), `"`+k+`"`) {
			t.Fatalf("expected %q stripped from outbound, got:\n%s", k, out)
		}
	}
}

// TestParseConfigContent_StripsLeakedCredentialFields_FullConfig covers the
// fullConfig=true branch where the parser keeps the entire input map (not
// just `outbounds`/`providers`). The strip must run there too.
func TestParseConfigContent_StripsLeakedCredentialFields_FullConfig(t *testing.T) {
	const sub = `{
		"log": {"level": "info"},
		"outbounds": [
			{"type": "direct", "tag": "a", "credential_id": "x"},
			{"type": "direct", "tag": "b", "device_secret": "y"}
		]
	}`
	configOpt := DefaultMartenOptions()
	configOpt.EnableFullConfig = true
	out, err := ParseConfigContent(sub, false, configOpt, true)
	if err != nil {
		t.Fatalf("ParseConfigContent (fullConfig): %v", err)
	}
	if strings.Contains(string(out), `"credential_id"`) || strings.Contains(string(out), `"device_secret"`) {
		t.Fatalf("expected credential leak keys stripped in fullConfig mode, got:\n%s", out)
	}
}
