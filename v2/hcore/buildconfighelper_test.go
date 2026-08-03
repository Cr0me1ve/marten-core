package hcore

import (
	"encoding/json"
	"testing"

	"github.com/Cr0me1ve/marten-core/v2/config"
)

func TestNormalizeLegacyMartenSettings(t *testing.T) {
	options := config.DefaultMartenOptions()
	options.MTU = 9000
	options.RemoteDnsAddress = "tcp://9.9.9.9"

	normalizeLegacyMartenSettings(options)

	if options.MTU != 1280 {
		t.Fatalf("MTU = %d, want 1280", options.MTU)
	}
	if options.RemoteDnsAddress != "8.8.8.8" {
		t.Fatalf("RemoteDnsAddress = %q, want 8.8.8.8", options.RemoteDnsAddress)
	}
}

func TestNormalizeLegacyMartenSettingsJSONPreservesExplicitFalse(t *testing.T) {
	input := `{"enable-clash-api":false,"strict-route":false,"mtu":9000,"remote-dns-address":"tcp://9.9.9.9"}`

	normalized, err := normalizeLegacyMartenSettingsJSON(input)
	if err != nil {
		t.Fatal(err)
	}

	var settings map[string]any
	if err := json.Unmarshal([]byte(normalized), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["enable-clash-api"] != false {
		t.Fatalf("enable-clash-api = %v, want false", settings["enable-clash-api"])
	}
	if settings["strict-route"] != false {
		t.Fatalf("strict-route = %v, want false", settings["strict-route"])
	}
	if settings["mtu"] != float64(1280) {
		t.Fatalf("mtu = %v, want 1280", settings["mtu"])
	}
	if settings["remote-dns-address"] != "8.8.8.8" {
		t.Fatalf("remote-dns-address = %v, want 8.8.8.8", settings["remote-dns-address"])
	}
}

func TestChangeMartenSettingsInvalidJSONDoesNotPublishDefaults(t *testing.T) {
	previousOptions := static.MartenOptions
	previousReady := static.settingsReady.Load()
	t.Cleanup(func() {
		static.MartenOptions = previousOptions
		static.settingsReady.Store(previousReady)
	})

	current := config.DefaultMartenOptions()
	current.EnableTun = true
	current.MTU = 1420
	static.MartenOptions = current
	static.settingsReady.Store(true)

	_, err := ChangeMartenSettings(&ChangeMartenSettingsRequest{
		MartenSettingsJson: `{"enable-tun":`,
	}, false)
	if err == nil {
		t.Fatal("invalid settings JSON was accepted")
	}
	if static.MartenOptions != current {
		t.Fatal("invalid settings JSON replaced the published options")
	}
	if !static.MartenOptions.EnableTun || static.MartenOptions.MTU != 1420 {
		t.Fatalf("published options changed after invalid JSON: %#v", static.MartenOptions)
	}
	if !static.settingsReady.Load() {
		t.Fatal("invalid settings JSON cleared readiness for the prior valid snapshot")
	}
}

func TestChangeMartenSettingsEmptySnapshotClearsReadiness(t *testing.T) {
	previousOptions := static.MartenOptions
	previousReady := static.settingsReady.Load()
	t.Cleanup(func() {
		static.MartenOptions = previousOptions
		static.settingsReady.Store(previousReady)
	})

	static.MartenOptions = config.DefaultMartenOptions()
	static.MartenOptions.EnableTun = true
	static.settingsReady.Store(true)

	if _, err := ChangeMartenSettings(&ChangeMartenSettingsRequest{}, true); err != nil {
		t.Fatalf("clear settings snapshot: %v", err)
	}
	if static.settingsReady.Load() {
		t.Fatal("empty settings snapshot left settings readiness true")
	}
	if static.MartenOptions == nil {
		t.Fatal("empty settings snapshot did not publish defaults")
	}
}
