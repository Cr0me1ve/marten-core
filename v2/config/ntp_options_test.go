package config

import (
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestBuildConfigDoesNotStartNTPWhenDisabled(t *testing.T) {
	opt := *DefaultMartenOptions()
	if opt.EnableNTP {
		t.Fatal("default Marten options must not block startup on NTP")
	}

	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{{Type: C.TypeVLESS, Tag: "vless"}},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	if out.NTP != nil {
		t.Fatalf("NTP = %#v, want nil when enable-ntp is false", out.NTP)
	}
}

func TestBuildConfigStartsNTPOnlyWhenEnabled(t *testing.T) {
	opt := *DefaultMartenOptions()
	opt.EnableNTP = true

	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{{Type: C.TypeVLESS, Tag: "vless"}},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	if out.NTP == nil || !out.NTP.Enabled {
		t.Fatalf("NTP = %#v, want enabled NTP options", out.NTP)
	}
	if out.NTP.Server != "time.apple.com" || out.NTP.ServerPort != 123 {
		t.Fatalf("NTP server = %s:%d, want time.apple.com:123", out.NTP.Server, out.NTP.ServerPort)
	}
}
