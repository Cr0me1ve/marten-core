package hcore

import (
	"strings"
	"testing"

	"github.com/sagernet/sing-box/option"
)

func TestHasTunInbound(t *testing.T) {
	if hasTunInbound(nil) {
		t.Fatal("nil options reported a TUN inbound")
	}
	if hasTunInbound(&option.Options{Inbounds: []option.Inbound{{Type: "mixed"}}}) {
		t.Fatal("non-TUN inbound reported a TUN inbound")
	}
	if !hasTunInbound(&option.Options{Inbounds: []option.Inbound{{Type: "mixed"}, {Type: "tun"}}}) {
		t.Fatal("TUN inbound was not detected")
	}
}

func TestValidateAndroidTunPlatform(t *testing.T) {
	tunOptions := &option.Options{Inbounds: []option.Inbound{{Type: "tun"}}}
	nonTunOptions := &option.Options{Inbounds: []option.Inbound{{Type: "mixed"}}}

	t.Run("rejects Android TUN without platform", func(t *testing.T) {
		err := validateAndroidTunPlatform("android", tunOptions, nil)
		if err == nil || !strings.Contains(err.Error(), "platform interface unavailable") {
			t.Fatalf("validateAndroidTunPlatform() error = %v, want missing-platform failure", err)
		}
	})

	for _, test := range []struct {
		name     string
		goos     string
		options  *option.Options
		platform *MobilePlatformInterface
	}{
		{name: "non Android", goos: "linux", options: tunOptions},
		{name: "Android without TUN", goos: "android", options: nonTunOptions},
		{name: "Android TUN with platform", goos: "android", options: tunOptions, platform: &MobilePlatformInterface{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateAndroidTunPlatform(test.goos, test.options, test.platform); err != nil {
				t.Fatalf("validateAndroidTunPlatform() error = %v, want nil", err)
			}
		})
	}
}
