package config

import "testing"

func TestParseConfigContent_AcceptsAmneziaWG20Outbound(t *testing.T) {
	const sub = `{
		"outbounds": [
			{
				"type": "amneziawg",
				"tag": "awg",
				"server": "example.net",
				"server_port": 51820,
				"local_address": "10.8.0.2/32",
				"private_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"peer_public_key": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				"jc": 4,
				"jmin": 64,
				"jmax": 256,
				"s1": 16,
				"s2": 24,
				"s3": 12,
				"s4": 8,
				"h1": "1100-1110",
				"h2": "2200-2210",
				"h3": "3300-3310",
				"h4": "4400-4410",
				"i1": "<b 0xaabbccdd><t><r 8>",
				"i2": "<r 12>",
				"i3": "<rc 10>",
				"i4": "<rd 10>",
				"i5": "<b 0x01020304><r 4>"
			}
		]
	}`
	options, err := ParseConfigContentToOptions(sub, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	for _, outbound := range options.Outbounds {
		if outbound.Tag != "awg" {
			continue
		}
		awg := outbound.WireGuardOptions
		if awg.S3 != 12 || awg.S4 != 8 {
			t.Fatalf("S3/S4 were not preserved: %#v", awg)
		}
		if string(awg.H1) != "1100-1110" || string(awg.H4) != "4400-4410" {
			t.Fatalf("H ranges were not preserved: %#v", awg)
		}
		if awg.I1 == "" || awg.I5 == "" {
			t.Fatalf("I packets were not preserved: %#v", awg)
		}
		if awg.PersistentKeepaliveInterval != defaultAmneziaPersistentKeepaliveInterval {
			t.Fatalf("AWG keepalive = %d, want %d", awg.PersistentKeepaliveInterval, defaultAmneziaPersistentKeepaliveInterval)
		}
		return
	}
	t.Fatalf("AWG outbound not found in parsed config: %#v", options.Outbounds)
}

func TestParseConfigContent_PreservesExplicitAmneziaWGKeepalive(t *testing.T) {
	const sub = `{
		"outbounds": [
			{
				"type": "amneziawg",
				"tag": "awg",
				"server": "example.net",
				"server_port": 51820,
				"local_address": "10.8.0.2/32",
				"private_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"peer_public_key": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				"persistent_keepalive_interval": 17
			}
		]
	}`
	options, err := ParseConfigContentToOptions(sub, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	for _, outbound := range options.Outbounds {
		if outbound.Tag == "awg" {
			if outbound.WireGuardOptions.PersistentKeepaliveInterval != 17 {
				t.Fatalf("AWG keepalive = %d, want 17", outbound.WireGuardOptions.PersistentKeepaliveInterval)
			}
			return
		}
	}
	t.Fatalf("AWG outbound not found in parsed config: %#v", options.Outbounds)
}
