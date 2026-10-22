package config

import (
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestSetInboundForPlatformAndroidUsesKernelAssignedLocalDnsPortAndKeepsMixedPort(t *testing.T) {
	opt := *DefaultMartenOptions()
	opt.MixedPort = 30001

	var options option.Options
	setInboundForPlatform(&options, &opt, "android")

	dnsInbound, ok := findInboundByTagAndType(options.Inbounds, InboundDNSTag, C.TypeDirect)
	if !ok {
		t.Fatalf("dns inbound with tag %q and type %q not found", InboundDNSTag, C.TypeDirect)
	}
	if got, want := dnsInbound.DirectOptions.ListenOptions.ListenPort, uint16(0); got != want {
		t.Fatalf("dns inbound listen port = %d, want %d", got, want)
	}

	mixedInbound, ok := findInboundByTagAndType(options.Inbounds, InboundMixedTag, C.TypeMixed)
	if !ok {
		t.Fatalf("mixed inbound with tag %q and type %q not found", InboundMixedTag, C.TypeMixed)
	}
	if got, want := mixedInbound.MixedOptions.ListenOptions.ListenPort, opt.MixedPort; got != want {
		t.Fatalf("mixed inbound listen port = %d, want fixed mixed port %d", got, want)
	}
}

func TestSetInboundForPlatformLinuxPreservesConfiguredLocalDnsPort(t *testing.T) {
	opt := *DefaultMartenOptions()
	opt.LocalDnsPort = 16450

	var options option.Options
	setInboundForPlatform(&options, &opt, "linux")

	dnsInbound, ok := findInboundByTagAndType(options.Inbounds, InboundDNSTag, C.TypeDirect)
	if !ok {
		t.Fatalf("dns inbound with tag %q and type %q not found", InboundDNSTag, C.TypeDirect)
	}
	if got, want := dnsInbound.DirectOptions.ListenOptions.ListenPort, opt.LocalDnsPort; got != want {
		t.Fatalf("dns inbound listen port = %d, want %d", got, want)
	}
}

func findInboundByTagAndType(inbounds []option.Inbound, tag string, inboundType string) (option.Inbound, bool) {
	for _, inbound := range inbounds {
		if inbound.Tag == tag && inbound.Type == inboundType {
			return inbound, true
		}
	}
	return option.Inbound{}, false
}
