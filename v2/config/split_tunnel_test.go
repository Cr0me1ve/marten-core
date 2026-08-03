package config

import (
	"testing"

	"github.com/sagernet/sing-box/option"
)

func TestBuildConfigAppliesTunExcludePackage(t *testing.T) {
	opt := *DefaultMartenOptions()
	opt.EnableTun = true
	opt.ExcludePackage = []string{"ru.sberbankmobile", "com.example.app"}

	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{
			{Type: "direct", Tag: "direct"},
		},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	if len(out.Inbounds) == 0 || out.Inbounds[0].Type != "tun" {
		t.Fatalf("first inbound = %#v, want tun", out.Inbounds)
	}
	got := out.Inbounds[0].TunOptions.ExcludePackage
	if len(got) != 2 || got[0] != "ru.sberbankmobile" || got[1] != "com.example.app" {
		t.Fatalf("ExcludePackage = %#v", got)
	}
}
