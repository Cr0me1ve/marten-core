package config

import (
	"encoding/json"
	"strings"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestParseConfigContentSalvagesCompatibleProvidersIndependently(t *testing.T) {
	const sub = `{
		"providers": [
			{"hosts":[" provider.example.test "],"credentials":[{"client_id":"placeholder-id","client_secret":"placeholder-secret","future_credential_field":true}],"future_provider_field":{"revision":2}},
			{"hosts":"wrong-type","credentials":[{"client_id":"placeholder-id","client_secret":"placeholder-secret"}]},
			{"hosts":[""],"credentials":[{"client_id":"placeholder-id","client_secret":"placeholder-secret"}]},
			{"hosts":["empty-credentials.example.test"],"credentials":[{"client_id":"","client_secret":"placeholder-secret"}]}
		],
		"outbounds": [
			{"type":"future-protocol","tag":"future"},
			{"type":"vless","tag":"usable","server":"usable.example.test","server_port":443,"uuid":"00000000-0000-0000-0000-000000000001","future_outbound_field":true},
			{"type":"vless","tag":"malformed","server":"malformed.example.test","server_port":443}
		]
	}`

	canonical, err := ParseConfigContent(sub, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("mixed subscription must keep compatible providers and selectable outbounds: %v", err)
	}

	var decoded struct {
		Providers []struct {
			Hosts       []string `json:"hosts"`
			Credentials []struct {
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
			} `json:"credentials"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		t.Fatalf("decode canonical config: %v", err)
	}
	if len(decoded.Providers) != 1 {
		t.Fatalf("want exactly one surviving provider, got %#v", decoded.Providers)
	}
	provider := decoded.Providers[0]
	if len(provider.Hosts) != 1 || provider.Hosts[0] != "provider.example.test" {
		t.Fatalf("provider hosts were not canonicalized: %#v", provider.Hosts)
	}
	if len(provider.Credentials) != 1 || provider.Credentials[0].ClientID != "placeholder-id" || provider.Credentials[0].ClientSecret != "placeholder-secret" {
		t.Fatalf("provider credentials were not retained: %#v", provider.Credentials)
	}
}

// TestParser_PreservesTopLevelProviders verifies that a top-level `providers`
// field in a sing-box subscription survives the non-fullConfig parser pass.
func TestParser_PreservesTopLevelProviders(t *testing.T) {
	const sub = `{
		"outbounds": [{"type": "direct", "tag": "out"}],
		"providers": [
			{"hosts": ["provider.example"], "credentials": [{"client_id": "x", "client_secret": "y"}]}
		]
	}`
	configOpt := DefaultMartenOptions()
	configOpt.EnableFullConfig = false
	out, err := ParseConfigContent(sub, false, configOpt, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(string(out), `"providers"`) {
		t.Fatalf("expected providers preserved in non-fullConfig output, got:\n%s", out)
	}
	if !strings.Contains(string(out), `"provider.example"`) {
		t.Fatalf("expected provider entry preserved, got:\n%s", out)
	}
}

// TestSetTurncoatProviders_MartenOptionsWins documents the precedence:
// MartenOptions.Providers (set by the app) takes precedence over the
// subscription's top-level `providers` (parsed into input.TurncoatProviders).
func TestSetTurncoatProviders_MartenOptionsWins(t *testing.T) {
	opt := &MartenOptions{
		Providers: []option.TurncoatProvider{{Hosts: []string{"app.example"}}},
	}
	input := &option.Options{}
	input.TurncoatProviders = []option.TurncoatProvider{{Hosts: []string{"sub.example"}}}
	out := &option.Options{}
	setTurncoatProviders(out, input, opt)
	if len(out.TurncoatProviders) != 1 || out.TurncoatProviders[0].Hosts[0] != "app.example" {
		t.Fatalf("expected MartenOptions.Providers to win, got %+v", out.TurncoatProviders)
	}
}

func TestSetTurncoatProviders_FallsBackToSubscription(t *testing.T) {
	opt := &MartenOptions{}
	input := &option.Options{}
	input.TurncoatProviders = []option.TurncoatProvider{{Hosts: []string{"sub.example"}}}
	out := &option.Options{}
	setTurncoatProviders(out, input, opt)
	if len(out.TurncoatProviders) != 1 || out.TurncoatProviders[0].Hosts[0] != "sub.example" {
		t.Fatalf("expected subscription providers, got %+v", out.TurncoatProviders)
	}
}

func TestSetTurncoatProviders_BothEmpty(t *testing.T) {
	out := &option.Options{}
	setTurncoatProviders(out, &option.Options{}, &MartenOptions{})
	if len(out.TurncoatProviders) != 0 {
		t.Fatal("expected empty when neither source set")
	}
}

func TestBuildConfig_HidesTurncoatTransportFromSelector(t *testing.T) {
	input := option.Options{Outbounds: []option.Outbound{
		{
			Type: C.TypeTurncoat,
			Tag:  "node-a-turncoat",
			TurncoatOptions: option.TurncoatOutboundOptions{
				ServerOptions:  option.ServerOptions{Server: "relay.example.net", ServerPort: 56000},
				CallURLs:       []string{"https://call.example.com/join/abc"},
				PeerType:       "proxy_v2",
				Sessions:       4,
				StreamsPerCred: 2,
				UseUDP:         true,
			},
		},
		{
			Type: C.TypeHysteria2,
			Tag:  "node-a",
			Hysteria2Options: option.Hysteria2OutboundOptions{
				DialerOptions: option.DialerOptions{Detour: "node-a-turncoat"},
			},
		},
	}}
	out, err := BuildConfig(*DefaultMartenOptions(), input)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	var selector *option.Outbound
	var foundTurncoat bool
	for i := range out.Outbounds {
		if out.Outbounds[i].Tag == OutboundSelectTag {
			selector = &out.Outbounds[i]
		}
		if out.Outbounds[i].Tag == "node-a-turncoat" {
			foundTurncoat = true
		}
	}
	if !foundTurncoat {
		t.Fatal("turncoat transport outbound should remain in final config")
	}
	if selector == nil {
		t.Fatal("selector not found")
	}
	for _, tag := range selector.SelectorOptions.Outbounds {
		if tag == "node-a-turncoat" {
			t.Fatalf("turncoat helper leaked into selector: %#v", selector.SelectorOptions.Outbounds)
		}
	}
	if len(selector.SelectorOptions.Outbounds) != 1 || selector.SelectorOptions.Outbounds[0] != "node-a" {
		t.Fatalf("backend outbound missing from selector: %#v", selector.SelectorOptions.Outbounds)
	}
	if selector.SelectorOptions.Default != "node-a" {
		t.Fatalf("selector default = %q, want node-a", selector.SelectorOptions.Default)
	}
	for _, outbound := range out.Outbounds {
		if outbound.Tag == OutboundURLTestTag {
			t.Fatal("single visible outbound should not create an auto urltest group")
		}
	}
}
