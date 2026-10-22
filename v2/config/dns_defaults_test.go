package config

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
)

func TestPlatformEndpointResolverDoesNotReuseProcessCacheForEmptyOrErrorResult(t *testing.T) {
	endpoint := "authoritative-physical-resolver.example"
	cases := []struct {
		name     string
		resolver PlatformEndpointResolver
	}{
		{
			name: "empty",
			resolver: func(_ context.Context, _ string) ([]netip.Addr, error) {
				return nil, nil
			},
		},
		{
			name: "error",
			resolver: func(_ context.Context, _ string) ([]netip.Addr, error) {
				return nil, errors.New("physical resolver unavailable")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ipMapsMutex.Lock()
			previous, existed := ipMaps[endpoint]
			ipMaps[endpoint] = []string{"198.51.100.99"}
			ipMapsMutex.Unlock()
			t.Cleanup(func() {
				ipMapsMutex.Lock()
				defer ipMapsMutex.Unlock()
				if existed {
					ipMaps[endpoint] = previous
				} else {
					delete(ipMaps, endpoint)
				}
			})
			SetPlatformEndpointResolver(tc.resolver)
			t.Cleanup(func() { SetPlatformEndpointResolver(nil) })

			if got := getIPs(endpoint); len(got) != 0 {
				t.Fatalf("getIPs(%q) = %v, want no reuse of the stale process cache", endpoint, got)
			}
		})
	}
}

func TestPlatformEndpointResolverCancellationBoundsStalledWorkersAndAllowsNextLookup(t *testing.T) {
	var started atomic.Int32
	finished := make(chan struct{}, 8)
	SetPlatformEndpointResolver(func(ctx context.Context, _ string) ([]netip.Addr, error) {
		started.Add(1)
		<-ctx.Done()
		finished <- struct{}{}
		return nil, ctx.Err()
	})
	t.Cleanup(func() { SetPlatformEndpointResolver(nil) })

	domains := []string{
		"stalled-1.example.test", "stalled-2.example.test", "stalled-3.example.test", "stalled-4.example.test",
		"stalled-5.example.test", "stalled-6.example.test", "stalled-7.example.test", "stalled-8.example.test",
	}
	startedAt := time.Now()
	if got := getIPs(domains...); len(got) != 0 {
		t.Fatalf("stalled resolver returned unexpected addresses: %v", got)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("stalled endpoint resolution took %s, want bounded near its 500 ms deadline", elapsed)
	}
	if got := started.Load(); got == 0 || int(got) > cap(endpointLookupSlots) {
		t.Fatalf("concurrent physical lookups = %d, want 1..%d", got, cap(endpointLookupSlots))
	}
	for index := int32(0); index < started.Load(); index++ {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("cancelled endpoint resolver worker did not exit")
		}
	}

	SetPlatformEndpointResolver(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.51")}, nil
	})
	if got := getIPs("after-stall.example.test"); len(got) != 1 || got[0] != "192.0.2.51" {
		t.Fatalf("lookup after stalled resolvers = %v, want [192.0.2.51]", got)
	}
}

func TestBuildConfigUsesRegisteredPlatformEndpointResolverForStaticIPsAndDirectRoutes(t *testing.T) {
	var resolvedDomains []string
	SetPlatformEndpointResolver(func(_ context.Context, domain string) ([]netip.Addr, error) {
		resolvedDomains = append(resolvedDomains, domain)
		return []netip.Addr{
			netip.MustParseAddr("198.51.100.44"),
			netip.MustParseAddr("2001:db8::44"),
			netip.MustParseAddr("10.0.0.44"), // blocked bootstrap answer
			netip.Addr{},                     // invalid, must never materialize
		}, nil
	})
	t.Cleanup(func() { SetPlatformEndpointResolver(nil) })

	opt := *DefaultMartenOptions()
	endpoint := "platform-resolver.example"
	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{{
			Type: C.TypeVLESS,
			Tag:  "platform-endpoint",
			VLESSOptions: option.VLESSOutboundOptions{
				ServerOptions: option.ServerOptions{Server: endpoint, ServerPort: 443},
				UUID:          "372f66bc-d81b-5392-9ce3-3658ee631ab7",
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	if len(resolvedDomains) != 1 || resolvedDomains[0] != endpoint {
		t.Fatalf("platform resolver domains = %v, want [%s]", resolvedDomains, endpoint)
	}

	gotStaticIPs := out.DNS.StaticIPs[endpoint]
	wantStaticIPs := []string{"198.51.100.44", "2001:db8::44"}
	if len(gotStaticIPs) != len(wantStaticIPs) {
		t.Fatalf("static IPs = %v, want %v", gotStaticIPs, wantStaticIPs)
	}
	for index, want := range wantStaticIPs {
		if gotStaticIPs[index] != want {
			t.Fatalf("static IPs = %v, want %v", gotStaticIPs, wantStaticIPs)
		}
	}

	var gotCIDRs []string
	for _, rule := range out.Route.Rules {
		if rule.DefaultOptions.Outbound == OutboundDirectTag && containsString(rule.DefaultOptions.Domain, endpoint) {
			gotCIDRs = rule.DefaultOptions.IPCIDR
			break
		}
	}
	if !containsString(gotCIDRs, "198.51.100.44/32") || !containsString(gotCIDRs, "2001:db8::44/128") {
		t.Fatalf("direct endpoint CIDRs = %v, want resolved IPv4 and IPv6 CIDRs", gotCIDRs)
	}
	if containsString(gotCIDRs, "10.0.0.44/32") {
		t.Fatalf("direct endpoint CIDRs = %v, must not contain blocked answer", gotCIDRs)
	}
}

func TestBuildConfigPlatformEndpointResolverAcceptsAnEmptyAnswerWithoutFailing(t *testing.T) {
	SetPlatformEndpointResolver(func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{}, nil
	})
	t.Cleanup(func() { SetPlatformEndpointResolver(nil) })

	opt := *DefaultMartenOptions()
	endpoint := "empty-platform-resolver.example"
	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{{
			Type: C.TypeVLESS,
			Tag:  "empty-platform-endpoint",
			VLESSOptions: option.VLESSOutboundOptions{
				ServerOptions: option.ServerOptions{Server: endpoint, ServerPort: 443},
				UUID:          "372f66bc-d81b-5392-9ce3-3658ee631ab7",
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildConfig with an empty usable platform result returned error: %v", err)
	}
	if got := out.DNS.StaticIPs[endpoint]; len(got) != 0 {
		t.Fatalf("static IPs = %v, want no answer materialized from an empty resolver result", got)
	}
	for _, rule := range out.Route.Rules {
		if rule.DefaultOptions.Outbound == OutboundDirectTag && containsString(rule.DefaultOptions.Domain, endpoint) && len(rule.DefaultOptions.IPCIDR) != 0 {
			t.Fatalf("direct endpoint CIDRs = %v, want no CIDRs from an empty resolver result", rule.DefaultOptions.IPCIDR)
		}
	}
}

func TestDefaultRemoteDNSUsesIPLiteralDoHWithoutResolverBootstrap(t *testing.T) {
	opt := *DefaultMartenOptions()
	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{{Type: C.TypeVLESS, Tag: "vless"}},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	for _, server := range out.DNS.Servers {
		if server.Tag != DNSRemoteTag {
			continue
		}
		if server.Address != "https://8.8.8.8/dns-query" {
			t.Fatalf("default dns-remote address = %q, want IP-literal DoH", server.Address)
		}
		parsed, err := url.Parse(server.Address)
		if err != nil {
			t.Fatalf("parse default dns-remote address: %v", err)
		}
		if host := parsed.Hostname(); host != "8.8.8.8" {
			t.Fatalf("default dns-remote hostname = %q, want IP literal to avoid bootstrap DNS", host)
		}
		return
	}
	t.Fatal("dns-remote server not found")
}

// TestForceTcpDnsForRemote covers the rewrite that prevents sing-box DNS from
// trying UDP through TCP-only backends (notably VLESS Vision and the TURNcoat
// helper). Known public resolver defaults move to DoH on 443, unknown bare IPs
// and explicit udp:// addresses must become tcp://; already TCP-capable schemes
// must pass through; "local" must be left alone.
func TestForceTcpDnsForRemote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"9.9.9.9", "https://9.9.9.9/dns-query"},
		{"8.8.8.8", "https://8.8.8.8/dns-query"},
		{"8.8.8.8:53", "https://8.8.8.8/dns-query"},
		{"1.1.1.1", "https://1.1.1.1/dns-query"},
		{"udp://9.9.9.9", "https://9.9.9.9/dns-query"},
		{"udp://9.9.9.9:53", "https://9.9.9.9/dns-query"},
		{"203.0.113.8", "tcp://203.0.113.8"},
		{"udp://203.0.113.8:53", "tcp://203.0.113.8:53"},
		{"tcp://9.9.9.9", "tcp://9.9.9.9"},
		{"tls://9.9.9.9", "tls://9.9.9.9"},
		// Explicit transports belong to the imported config and must not be
		// rewritten, even if their host is a name rather than an IP literal.
		{"https://dns.quad9.net/dns-query", "https://dns.quad9.net/dns-query"},
		{"https://8.8.8.8/dns-query", "https://8.8.8.8/dns-query"},
		{"h3://9.9.9.9/dns-query", "h3://9.9.9.9/dns-query"},
		{"quic://9.9.9.9", "quic://9.9.9.9"},
		{"rcode://success", "rcode://success"},
		{"local", "local"},
		{"", ""},
	}
	for _, c := range cases {
		got := forceTcpDnsForRemote(c.in)
		if got != c.want {
			t.Errorf("forceTcpDnsForRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRemoteDNSUsesNativeTransportForAmneziaWGOnlyProfiles(t *testing.T) {
	got := remoteDnsAddressForOutbounds("9.9.9.9", []option.Outbound{
		{Type: C.TypeAmneziaWG, Tag: "awg"},
	})
	if got != "9.9.9.9" {
		t.Fatalf("remote DNS address = %q, want native DNS for AWG-only profile", got)
	}
}

func TestRemoteDNSKeepsTCPForTCPOnlyOrMixedProfiles(t *testing.T) {
	cases := []struct {
		name      string
		outbounds []option.Outbound
	}{
		{
			name: "tcp only",
			outbounds: []option.Outbound{
				{Type: C.TypeVLESS, Tag: "vless"},
			},
		},
		{
			name: "mixed awg and tcp",
			outbounds: []option.Outbound{
				{Type: C.TypeAmneziaWG, Tag: "awg"},
				{Type: C.TypeVLESS, Tag: "vless"},
			},
		},
	}
	for _, c := range cases {
		got := remoteDnsAddressForOutbounds("8.8.8.8", c.outbounds)
		if got != "https://8.8.8.8/dns-query" {
			t.Fatalf("%s: remote DNS address = %q, want forced DoH", c.name, got)
		}
	}
}

func TestBuildConfigAddsStaticIPsForKnownDoHResolvers(t *testing.T) {
	opt := *DefaultMartenOptions()
	opt.RemoteDnsAddress = "8.8.8.8"

	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{
			{Type: C.TypeVLESS, Tag: "vless"},
		},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	for host, want := range map[string][]string{
		"dns.google":         {"8.8.8.8", "8.8.4.4"},
		"cloudflare-dns.com": {"1.1.1.1", "1.0.0.1"},
		"dns.quad9.net":      {"9.9.9.9", "149.112.112.112"},
	} {
		got := out.DNS.StaticIPs[host]
		if len(got) != len(want) {
			t.Fatalf("static IPs for %s = %v, want %v", host, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("static IPs for %s = %v, want %v", host, got, want)
			}
		}
	}
}

func TestBuildConfigUsesLocalDNSForDirectOutboundServerDomains(t *testing.T) {
	opt := *DefaultMartenOptions()
	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{
			{
				Type: C.TypeVLESS,
				Tag:  "germany",
				VLESSOptions: option.VLESSOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "de-01.example.net",
						ServerPort: 443,
					},
					UUID: "372f66bc-d81b-5392-9ce3-3658ee631ab7",
				},
			},
			{
				Type: C.TypeVLESS,
				Tag:  "us",
				VLESSOptions: option.VLESSOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "203.0.113.10",
						ServerPort: 443,
					},
					UUID: "93f70d0c-7868-54f5-afe0-1dab894a5841",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}

	for _, rule := range out.DNS.Rules {
		defaultRule := rule.DefaultOptions
		if !containsString(defaultRule.Domain, "de-01.example.net") {
			continue
		}
		if defaultRule.Server != DNSLocalTag {
			t.Fatalf("server-domain DNS rule server = %q, want %q", defaultRule.Server, DNSLocalTag)
		}
		return
	}
	t.Fatal("missing DNS rule for direct outbound server domain")
}

func TestBuildConfigRoutesOutboundServerEndpointsDirect(t *testing.T) {
	opt := *DefaultMartenOptions()
	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{
			{
				Type: C.TypeVLESS,
				Tag:  "germany",
				VLESSOptions: option.VLESSOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "de-01.example.net",
						ServerPort: 443,
					},
					UUID: "372f66bc-d81b-5392-9ce3-3658ee631ab7",
				},
			},
			{
				Type: C.TypeVLESS,
				Tag:  "us",
				VLESSOptions: option.VLESSOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "203.0.113.10",
						ServerPort: 443,
					},
					UUID: "93f70d0c-7868-54f5-afe0-1dab894a5841",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}

	var hasDomain, hasIP bool
	var hasDomainStrategy bool
	for _, rule := range out.Route.Rules {
		defaultRule := rule.DefaultOptions
		if defaultRule.Outbound != OutboundDirectTag {
			continue
		}
		if containsString(defaultRule.Domain, "de-01.example.net") {
			hasDomain = true
		}
		if containsString(defaultRule.IPCIDR, "203.0.113.10/32") {
			hasIP = true
		}
	}
	for _, outbound := range out.Outbounds {
		if outbound.Tag != "germany" {
			continue
		}
		if outbound.VLESSOptions.Server != "de-01.example.net" {
			t.Fatalf("germany server = %q, want domain endpoint", outbound.VLESSOptions.Server)
		}
		if outbound.VLESSOptions.DomainStrategy == option.DomainStrategy(dns.DomainStrategyUseIPv4) {
			hasDomainStrategy = true
		}
	}
	if !hasDomain {
		t.Fatal("missing direct route rule for outbound server domain")
	}
	if !hasIP {
		t.Fatal("missing direct route rule for outbound server IP")
	}
	if !hasDomainStrategy {
		t.Fatal("missing ipv4_only domain strategy for outbound server domain")
	}
}

func TestParseConfigAcceptsNativeVLESSReality(t *testing.T) {
	raw := `{
  "outbounds": [
    {
      "type": "vless",
      "tag": "germany",
      "server": "de-01.wewe-jncjwb.gu.cc",
      "server_port": 443,
      "uuid": "372f66bc-d81b-5392-9ce3-3658ee631ab7",
      "flow": "xtls-rprx-vision",
      "packet_encoding": "xudp",
      "tls": {
        "enabled": true,
        "server_name": "de-01.wewe-jncjwb.gu.cc",
        "alpn": ["h2", "http/1.1"],
        "utls": {
          "enabled": true,
          "fingerprint": "chrome"
        },
        "reality": {
          "enabled": true,
          "public_key": "Xb4oXmQP7nlSRedupNZeEEC7E-mMaXL5rCQAksF5mVw",
          "short_id": "e210b8bca29ccc38"
        }
      }
    }
  ],
  "servers": [
    {"tag": "germany", "server": "de-01.wewe-jncjwb.gu.cc", "server_port": 443}
  ]
}`

	options, err := ParseConfigContentToOptions(raw, false, DefaultMartenOptions(), false)
	if err != nil {
		t.Fatalf("ParseConfigContentToOptions returned error: %v", err)
	}
	if len(options.Outbounds) == 0 {
		t.Fatal("missing outbounds")
	}
	outbound := options.Outbounds[0]
	if outbound.Type != C.TypeVLESS {
		t.Fatalf("outbound type = %q, want vless", outbound.Type)
	}
	tls := outbound.VLESSOptions.OutboundTLSOptionsContainer.TLS
	if tls == nil || tls.Reality == nil || !tls.Reality.Enabled {
		t.Fatalf("vless reality tls missing: %#v", tls)
	}
	if outbound.VLESSOptions.ServerOptions.Server != "de-01.wewe-jncjwb.gu.cc" ||
		outbound.VLESSOptions.ServerOptions.ServerPort != 443 {
		t.Fatalf("endpoint = %s:%d, want de domain:443", outbound.VLESSOptions.ServerOptions.Server, outbound.VLESSOptions.ServerOptions.ServerPort)
	}
}

func TestBuildConfigDoesNotAddImplicitTLSTricksToVLESSReality(t *testing.T) {
	base := option.Outbound{
		Type: C.TypeVLESS,
		Tag:  "germany",
		VLESSOptions: option.VLESSOutboundOptions{
			ServerOptions: option.ServerOptions{
				Server:     "de-01.example.net",
				ServerPort: 443,
			},
			UUID: "372f66bc-d81b-5392-9ce3-3658ee631ab7",
			OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{
				TLS: &option.OutboundTLSOptions{
					Enabled:    true,
					ServerName: "de-01.example.net",
					UTLS: &option.OutboundUTLSOptions{
						Enabled:     true,
						Fingerprint: "chrome",
					},
					Reality: &option.OutboundRealityOptions{
						Enabled:   true,
						PublicKey: "Xb4oXmQP7nlSRedupNZeEEC7E-mMaXL5rCQAksF5mVw",
						ShortID:   "e210b8bca29ccc38",
					},
				},
			},
		},
	}

	configOpt := *DefaultMartenOptions()
	configOpt.TLSTricks.PaddingSize = "1200-1400"
	out, err := BuildConfig(configOpt, option.Options{
		Outbounds: []option.Outbound{base},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}

	var outbound *option.Outbound
	for i := range out.Outbounds {
		if out.Outbounds[i].Tag == "germany" {
			outbound = &out.Outbounds[i]
			break
		}
	}
	if outbound == nil {
		t.Fatal("missing germany outbound")
	}

	tls := outbound.VLESSOptions.OutboundTLSOptionsContainer.TLS
	if tls == nil || tls.UTLS == nil {
		t.Fatalf("missing tls.utls: %#v", tls)
	}
	if tls.UTLS.Fingerprint != "chrome" {
		t.Fatalf("utls fingerprint = %q, want chrome", tls.UTLS.Fingerprint)
	}
	if tls.TLSTricks != nil {
		t.Fatalf("unexpected implicit tls_tricks: %#v", tls.TLSTricks)
	}
	if tls.Reality == nil || !tls.Reality.Enabled {
		t.Fatalf("missing reality options: %#v", tls)
	}
	if outbound.VLESSOptions.ServerOptions.Server != "de-01.example.net" || outbound.VLESSOptions.ServerOptions.ServerPort != 443 {
		t.Fatalf("endpoint = %s:%d, want de-01.example.net:443", outbound.VLESSOptions.ServerOptions.Server, outbound.VLESSOptions.ServerOptions.ServerPort)
	}
}

func TestRemoteDNSStripsTCPForAmneziaWG(t *testing.T) {
	got := remoteDnsAddressForOutbounds("tcp://9.9.9.9", []option.Outbound{
		{Type: C.TypeAmneziaWG, Tag: "awg"},
	})
	if got != "9.9.9.9" {
		t.Fatalf("remote DNS address = %q, want native DNS", got)
	}
}

func TestBuildConfigUsesNativeRemoteDNSForAmneziaWGProfile(t *testing.T) {
	opt := *DefaultMartenOptions()
	opt.RemoteDnsAddress = "8.8.8.8"

	out, err := BuildConfig(opt, option.Options{
		Outbounds: []option.Outbound{
			{
				Type: C.TypeAmneziaWG,
				Tag:  "awg",
				WireGuardOptions: option.WireGuardOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "198.51.100.1",
						ServerPort: 51820,
					},
					LocalAddress:  []netip.Prefix{netip.MustParsePrefix("10.0.0.2/32")},
					PrivateKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
					PeerPublicKey: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildConfig returned error: %v", err)
	}
	for _, server := range out.DNS.Servers {
		if server.Tag == DNSRemoteTag {
			if server.Address != "8.8.8.8" {
				t.Fatalf("dns-remote address = %q, want native DNS", server.Address)
			}
			return
		}
	}
	t.Fatal("dns-remote server not found")
}

func TestIPv4OnlyModeForcesDNSStrategyToIPv4(t *testing.T) {
	opt := DefaultMartenOptions()
	opt.EnableTun = true
	opt.RemoteDnsDomainStrategy = option.DomainStrategy(dns.DomainStrategyAsIS)
	opt.DirectDnsDomainStrategy = option.DomainStrategy(dns.DomainStrategyAsIS)
	var options option.Options

	setDns(&options, opt, nil)

	for _, server := range options.DNS.Servers {
		switch server.Tag {
		case DNSRemoteTag, DNSDirectTag, DNSTricksDirectTag:
			if server.Strategy != option.DomainStrategy(dns.DomainStrategyUseIPv4) {
				t.Fatalf("%s strategy = %v, want ipv4_only", server.Tag, server.Strategy)
			}
		}
	}
}

func TestRoutingBlocksAndroidPrivateDNSProbeToTunDNS(t *testing.T) {
	opt := DefaultMartenOptions()
	opt.EnableTun = true
	var options option.Options
	setNTP(&options)

	setRoutingOptions(&options, opt)

	for _, rule := range options.Route.Rules {
		defaultRule := rule.DefaultOptions
		if defaultRule.Outbound != OutboundBlockTag {
			continue
		}
		if !containsString(defaultRule.Inbound, InboundTUNTag) {
			continue
		}
		if !containsString(defaultRule.Network, "tcp") {
			continue
		}
		if !containsUint16(defaultRule.Port, 853) {
			continue
		}
		if containsString(defaultRule.IPCIDR, "172.19.0.2/32") &&
			containsString(defaultRule.IPCIDR, "fdfe:dcba:9876::2/128") {
			return
		}
	}
	t.Fatal("missing block rule for Android Private DNS probes to the TUN DNS address")
}

func TestAndroidTunProtectsCoreDialsWithAutoDetectInterface(t *testing.T) {
	if !autoDetectInterfaceForRuntime("android", true) {
		t.Fatal("Android TUN must keep auto_detect_interface so platform protect/bind hooks guard outbound dials")
	}
	if !autoDetectInterfaceForRuntime("android", false) {
		t.Fatal("Android non-TUN route should keep auto_detect_interface")
	}
	if !autoDetectInterfaceForRuntime("darwin", true) {
		t.Fatal("non-Android route should keep auto_detect_interface")
	}
	if !overrideAndroidVPNForRuntime("android", true) {
		t.Fatal("Android TUN must protect core outbound dials from the app VPN")
	}
	if !overrideAndroidVPNForRuntime("android", false) {
		t.Fatal("Android non-TUN route should keep the default behavior")
	}
	if !overrideAndroidVPNForRuntime("darwin", true) {
		t.Fatal("non-Android route should keep the default behavior")
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsUint16(values []uint16, target uint16) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
