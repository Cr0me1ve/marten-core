package config

import (
	"bytes"
	context "context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	sync "sync"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	dns "github.com/sagernet/sing-dns"
)

const (
	DNSRemoteTag       = "dns-remote"
	DNSLocalTag        = "dns-local"
	DNSDirectTag       = "dns-direct"
	DNSBlockTag        = "dns-block"
	DNSFakeTag         = "dns-fake"
	DNSTricksDirectTag = "dns-trick-direct"

	OutboundDirectTag         = "direct §hide§"
	OutboundBypassTag         = "bypass §hide§"
	OutboundBlockTag          = "block §hide§"
	OutboundSelectTag         = "select"
	OutboundURLTestTag        = "auto"
	OutboundDNSTag            = "dns-out §hide§"
	OutboundDirectFragmentTag = "direct-fragment §hide§"

	InboundTUNTag   = "tun-in"
	InboundMixedTag = "mixed-in"
	InboundDNSTag   = "dns-in"
)

const defaultAmneziaPersistentKeepaliveInterval uint16 = 25

var (
	OutboundMainProxyTag   = OutboundSelectTag
	PredefinedOutboundTags = []string{OutboundDirectTag, OutboundBypassTag, OutboundBlockTag, OutboundSelectTag, OutboundURLTestTag, OutboundDNSTag, OutboundDirectFragmentTag}
)

// PlatformEndpointResolver resolves proxy bootstrap hostnames through a
// platform-selected physical network. Android installs this hook because its
// UID-default resolver points at the fail-closed VPN as soon as the blocker is
// established; using net.DefaultResolver there can recursively send the
// endpoint lookup into the TUN that the endpoint itself is needed to start.
type PlatformEndpointResolver func(context.Context, string) ([]netip.Addr, error)

var (
	platformEndpointResolverMu sync.RWMutex
	platformEndpointResolver   PlatformEndpointResolver
)

// SetPlatformEndpointResolver replaces the process-wide endpoint bootstrap
// resolver. Passing nil restores the normal resolver used on non-Android
// platforms and by standalone core consumers.
func SetPlatformEndpointResolver(resolver PlatformEndpointResolver) {
	platformEndpointResolverMu.Lock()
	platformEndpointResolver = resolver
	platformEndpointResolverMu.Unlock()
}

func loadPlatformEndpointResolver() PlatformEndpointResolver {
	platformEndpointResolverMu.RLock()
	defer platformEndpointResolverMu.RUnlock()
	return platformEndpointResolver
}

func BuildConfigJson(configOpt MartenOptions, input option.Options) (string, error) {
	options, err := BuildConfig(configOpt, input)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	json.NewEncoder(&buffer)
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(options)
	if err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// TODO include selectors
func BuildConfig(opt MartenOptions, input option.Options) (*option.Options, error) {
	var options option.Options
	if opt.EnableFullConfig {
		options.Inbounds = input.Inbounds
		options.DNS = input.DNS
		options.Route = input.Route
	}
	options.DNS = &option.DNSOptions{
		StaticIPs: map[string][]string{},
	}
	OutboundMainProxyTag = OutboundSelectTag
	setClashAPI(&options, &opt)
	setLog(&options, &opt)
	setInbound(&options, &opt)
	if opt.EnableNTP {
		setNTP(&options)
	}
	setTurncoatProviders(&options, &input, &opt)
	err := setOutbounds(&options, &input, &opt)

	if err != nil {
		return nil, err
	}
	setDns(&options, &opt, options.Outbounds)
	setRoutingOptions(&options, &opt)
	setFakeDns(&options, &opt)
	addForceDirect(&options, &opt)
	return &options, nil
}

// setTurncoatProviders resolves the top-level TURNcoat auth-backend list that
// marten-sing-box uses as a fallback for outbounds with an empty per-outbound
// `providers`. Two sources, in order of precedence:
//
//  1. MartenOptions.Providers — explicitly set by Marten (e.g. from the app's
//     local providers config).
//  2. input.TurncoatProviders — preserved from the subscription JSON's
//     top-level `providers` field by ParseConfigContent.
//
// The first non-empty source wins.
func setTurncoatProviders(options *option.Options, input *option.Options, opt *MartenOptions) {
	switch {
	case len(opt.Providers) > 0:
		options.TurncoatProviders = opt.Providers
	case input != nil && len(input.TurncoatProviders) > 0:
		options.TurncoatProviders = input.TurncoatProviders
	}
}

func setNTP(options *option.Options) {
	options.NTP = &option.NTPOptions{
		Enabled:       true,
		ServerOptions: option.ServerOptions{ServerPort: 123, Server: "time.apple.com"},
		Interval:      option.Duration(12 * time.Hour),
	}
}

func getHostnameIfNotIP(inp string) (string, error) {
	if inp == "" {
		return "", fmt.Errorf("empty hostname: %s", inp)
	}
	if net.ParseIP(strings.Trim(inp, "[]")) == nil {
		return inp, nil
	}
	return "", fmt.Errorf("not a hostname: %s", inp)
}

func setOutbounds(options *option.Options, input *option.Options, opt *MartenOptions) error {
	var outbounds []option.Outbound
	var tags []string
	// Deduplicate by tag. sing-box rejects the whole config with
	// "outbound tag X duplicated" if two outbounds share a tag, and some
	// subscription providers emit the same outbound twice in a single
	// subscription. First occurrence wins; later duplicates are silently
	// dropped so the client still connects.
	seenTags := map[string]bool{}
	for _, out := range input.Outbounds {
		if contains(PredefinedOutboundTags, out.Tag) {
			continue
		}
		outbound, err := patchOutbound(out, *opt, options.DNS)
		if err != nil {
			return err
		}
		out = *outbound
		patchAmneziaWGKeepalive(&out)

		switch out.Type {
		case C.TypeBlock, C.TypeDNS:
			continue
		case C.TypeSelector, C.TypeURLTest:
			continue
		case C.TypeCustom:
			continue
		default:
			if contains([]string{"direct", "bypass", "block"}, out.Tag) {
				continue
			}
			if out.Tag != "" {
				if seenTags[out.Tag] {
					continue
				}
				seenTags[out.Tag] = true
			}
			if out.Type != C.TypeTurncoat && !strings.Contains(out.Tag, "§hide§") {
				tags = append(tags, out.Tag)
			}
			outbounds = append(outbounds, out)
		}
	}
	defaultSelect := ""
	if len(tags) > 0 {
		defaultSelect = tags[0]
	}

	for _, tag := range tags {
		if strings.Contains(tag, "§default§") {
			defaultSelect = "§default§"
		}
	}
	selector := option.Outbound{
		Type: C.TypeSelector,
		Tag:  OutboundSelectTag,
		SelectorOptions: option.SelectorOutboundOptions{
			Outbounds:                 tags,
			Default:                   defaultSelect,
			InterruptExistConnections: true,
		},
	}

	outbounds = append([]option.Outbound{selector}, outbounds...)

	options.Outbounds = append(
		outbounds,
		[]option.Outbound{
			{
				Tag:  OutboundDNSTag,
				Type: C.TypeDNS,
			},
			{
				Tag:  OutboundDirectTag,
				Type: C.TypeDirect,
			},
			{
				Tag:  OutboundDirectFragmentTag,
				Type: C.TypeDirect,
				DirectOptions: option.DirectOutboundOptions{
					DialerOptions: option.DialerOptions{
						TCPFastOpen: false,
						TLSFragment: option.TLSFragmentOptions{
							Enabled: true,
							Size:    opt.TLSTricks.FragmentSize,
							Sleep:   opt.TLSTricks.FragmentSleep,
						},
					},
				},
			},
			{
				Tag:  OutboundBypassTag,
				Type: C.TypeDirect,
			},
			{
				Tag:  OutboundBlockTag,
				Type: C.TypeBlock,
			},
		}...,
	)

	return nil
}

func patchAmneziaWGKeepalive(out *option.Outbound) {
	if out.Type != C.TypeAmneziaWG {
		return
	}
	if out.WireGuardOptions.PersistentKeepaliveInterval == 0 {
		out.WireGuardOptions.PersistentKeepaliveInterval = defaultAmneziaPersistentKeepaliveInterval
	}
	for i := range out.WireGuardOptions.Peers {
		if out.WireGuardOptions.Peers[i].PersistentKeepaliveInterval == 0 {
			out.WireGuardOptions.Peers[i].PersistentKeepaliveInterval = defaultAmneziaPersistentKeepaliveInterval
		}
	}
}

func isSelectableUserOutbound(out option.Outbound) bool {
	if contains(PredefinedOutboundTags, out.Tag) {
		return false
	}
	switch out.Type {
	case C.TypeBlock, C.TypeDNS, C.TypeSelector, C.TypeURLTest, C.TypeCustom, C.TypeTurncoat:
		return false
	}
	if contains([]string{"direct", "bypass", "block"}, out.Tag) {
		return false
	}
	return !strings.Contains(out.Tag, "§hide§")
}

func outboundSupportsUDPRemoteDNS(out option.Outbound) bool {
	switch out.Type {
	case C.TypeAmneziaWG, C.TypeWireGuard, "icmp":
		return true
	default:
		return false
	}
}

func isBlockedConnectionTestUrl(d string) bool {
	u, err := url.Parse(d)
	if err != nil {
		return false
	}
	return isBlockedDomain(u.Host)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func setClashAPI(options *option.Options, opt *MartenOptions) {
	if opt.EnableClashApi {
		if opt.ClashApiSecret == "" {
			opt.ClashApiSecret = generateRandomString(16)
		}
		options.Experimental = &option.ExperimentalOptions{
			ClashAPI: &option.ClashAPIOptions{
				ExternalController: fmt.Sprintf("%s:%d", "127.0.0.1", opt.ClashApiPort),
				Secret:             opt.ClashApiSecret,
			},

			CacheFile: &option.CacheFileOptions{
				Enabled: true,
				Path:    "data/clash.db",
			},
		}
	}
}

func setLog(options *option.Options, opt *MartenOptions) {
	options.Log = &option.LogOptions{
		Level:        opt.LogLevel,
		Output:       opt.LogFile,
		Disabled:     false,
		Timestamp:    false,
		DisableColor: true,
	}
}

func setInbound(options *option.Options, opt *MartenOptions) {
	setInboundForPlatform(options, opt, runtime.GOOS)
}

func setInboundForPlatform(options *option.Options, opt *MartenOptions, goos string) {
	var inboundDomainStrategy option.DomainStrategy
	if !opt.ResolveDestination {
		inboundDomainStrategy = option.DomainStrategy(dns.DomainStrategyAsIS)
	} else {
		inboundDomainStrategy = opt.IPv6Mode
	}
	if opt.EnableTun {
		tunInbound := option.Inbound{
			Type: C.TypeTun,
			Tag:  InboundTUNTag,

			TunOptions: option.TunInboundOptions{
				Stack:                  opt.TUNStack,
				MTU:                    opt.MTU,
				AutoRoute:              true,
				StrictRoute:            opt.StrictRoute,
				IncludePackage:         opt.IncludePackage,
				ExcludePackage:         opt.ExcludePackage,
				EndpointIndependentNat: true,
				// GSO:                    runtime.GOOS != "windows",
				InboundOptions: option.InboundOptions{
					SniffEnabled:             true,
					SniffOverrideDestination: false,
					DomainStrategy:           inboundDomainStrategy,
				},
			},
		}
		switch opt.IPv6Mode {
		case option.DomainStrategy(dns.DomainStrategyUseIPv4):
			tunInbound.TunOptions.Address = []netip.Prefix{
				netip.MustParsePrefix("172.19.0.1/28"),
			}
		case option.DomainStrategy(dns.DomainStrategyUseIPv6):
			tunInbound.TunOptions.Address = []netip.Prefix{
				netip.MustParsePrefix("fdfe:dcba:9876::1/126"),
			}
		default:
			tunInbound.TunOptions.Address = []netip.Prefix{
				netip.MustParsePrefix("172.19.0.1/28"),
				netip.MustParsePrefix("fdfe:dcba:9876::1/126"),
			}

		}
		options.Inbounds = append(options.Inbounds, tunInbound)

	}

	var bind string
	if opt.AllowConnectionFromLAN {
		bind = "0.0.0.0"
	} else {
		bind = "127.0.0.1"
	}

	options.Inbounds = append(
		options.Inbounds,
		option.Inbound{
			Type: C.TypeMixed,
			Tag:  InboundMixedTag,
			MixedOptions: option.HTTPMixedInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     option.NewListenAddress(netip.MustParseAddr(bind)),
					ListenPort: opt.MixedPort,
					InboundOptions: option.InboundOptions{
						SniffEnabled:             true,
						SniffOverrideDestination: true,
						DomainStrategy:           inboundDomainStrategy,
					},
				},
				SetSystemProxy: opt.SetSystemProxy,
			},
		},
	)

	// Android routes TUN DNS inside sing-box and resolves transport bootstrap
	// through PlatformEndpointResolver; no stable local DNS socket is part of
	// the mobile contract. Keep the direct inbound for config compatibility,
	// but let the kernel assign its private port so an old or foreign owner of
	// the legacy fixed port cannot block a new VPN generation.
	localDNSListenPort := opt.LocalDnsPort
	if goos == "android" {
		localDNSListenPort = 0
	}
	options.Inbounds = append(
		options.Inbounds,
		option.Inbound{
			Type: C.TypeDirect,
			Tag:  InboundDNSTag,
			DirectOptions: option.DirectInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     option.NewListenAddress(netip.MustParseAddr(bind)),
					ListenPort: localDNSListenPort,
				},
				// OverrideAddress: "1.1.1.1",
				// OverridePort:    53,
			},
		},
	)
}

// forceTcpDnsForRemote returns a DNS server address suitable for the
// `dns-remote` server, which routes through the active tunnel. Bare IPs and
// `udp://` addresses are rewritten to a TCP-capable transport because some
// backends (notably VLESS Vision and the TURNcoat helper) can only carry TCP
// traffic. Known public resolver defaults use IP-literal DoH on port 443 so
// tunnel DNS bootstrap does not depend on a direct resolver that a restricted
// network may block. Other addresses fall back to DNS-over-TCP. Already
// TCP-capable schemes (tcp, tls, https, h3, quic) are passed through.
//
// "local" stays untouched — it represents the system resolver, not a real DNS
// transport, and is only used for the DNSLocalTag server (which uses a direct
// detour anyway).
func forceTcpDnsForRemote(address string) string {
	if address == "" || address == "local" {
		return address
	}
	for _, scheme := range []string{"tcp://", "tls://", "https://", "h3://", "quic://", "rcode://"} {
		if strings.HasPrefix(address, scheme) {
			return address
		}
	}
	// Strip an explicit udp:// prefix if present; bare IPs default to udp.
	address = strings.TrimPrefix(address, "udp://")
	if doh := knownDoHResolverForAddress(address); doh != "" {
		return doh
	}
	return "tcp://" + address
}

func knownDoHResolverForAddress(address string) string {
	host := address
	if parsedHost, _, err := net.SplitHostPort(address); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "8.8.8.8", "8.8.4.4":
		return "https://8.8.8.8/dns-query"
	case "1.1.1.1", "1.0.0.1":
		return "https://1.1.1.1/dns-query"
	case "9.9.9.9", "149.112.112.112":
		return "https://9.9.9.9/dns-query"
	default:
		return ""
	}
}

func addKnownDoHStaticIPs(staticIPs map[string][]string) {
	staticIPs["dns.google"] = []string{"8.8.8.8", "8.8.4.4"}
	staticIPs["cloudflare-dns.com"] = []string{"1.1.1.1", "1.0.0.1"}
	staticIPs["dns.quad9.net"] = []string{"9.9.9.9", "149.112.112.112"}
}

func preferNativeDnsTransportForRemote(address string) string {
	if strings.HasPrefix(address, "tcp://") {
		return strings.TrimPrefix(address, "tcp://")
	}
	if strings.HasPrefix(address, "udp://") {
		return strings.TrimPrefix(address, "udp://")
	}
	return address
}

func remoteDnsAddressForOutbounds(address string, outbounds []option.Outbound) string {
	selectableCount := 0
	for _, outbound := range outbounds {
		if !isSelectableUserOutbound(outbound) {
			continue
		}
		selectableCount++
		if !outboundSupportsUDPRemoteDNS(outbound) {
			return forceTcpDnsForRemote(address)
		}
	}
	if selectableCount == 0 {
		return forceTcpDnsForRemote(address)
	}
	return preferNativeDnsTransportForRemote(address)
}

func remoteDnsAddressForDetour(address string, outbounds []option.Outbound, detour string) string {
	for _, outbound := range outbounds {
		if outbound.Tag != detour {
			continue
		}
		if outboundSupportsUDPRemoteDNS(outbound) {
			return preferNativeDnsTransportForRemote(address)
		}
		break
	}
	return forceTcpDnsForRemote(address)
}

func dnsStrategyForIPv6Mode(strategy option.DomainStrategy, ipv6Mode option.DomainStrategy) option.DomainStrategy {
	if ipv6Mode == option.DomainStrategy(dns.DomainStrategyUseIPv4) {
		return option.DomainStrategy(dns.DomainStrategyUseIPv4)
	}
	return strategy
}

func setDns(options *option.Options, opt *MartenOptions, outbounds []option.Outbound) {
	remoteDNSAddress := remoteDnsAddressForOutbounds(opt.RemoteDnsAddress, outbounds)
	remoteDNSStrategy := dnsStrategyForIPv6Mode(opt.RemoteDnsDomainStrategy, opt.IPv6Mode)
	directDNSStrategy := dnsStrategyForIPv6Mode(opt.DirectDnsDomainStrategy, opt.IPv6Mode)
	options.DNS = &option.DNSOptions{
		StaticIPs: map[string][]string{},
		DNSClientOptions: option.DNSClientOptions{
			IndependentCache: opt.IndependentDNSCache,
		},
		Final: DNSRemoteTag,
		Servers: []option.DNSServerOptions{
			{
				Tag: DNSRemoteTag,
				// Keep UDP-capable tunnels on native DNS transport. Forcing DNS
				// to TCP makes the first resolver request after Android idle pay
				// an extra TCP handshake through WireGuard/AmneziaWG and can
				// exceed Android resolver deadlines. TCP-only backends are
				// still forced to a TCP-capable DNS transport above.
				Address:         remoteDNSAddress,
				AddressResolver: DNSDirectTag,
				Strategy:        remoteDNSStrategy,
				Detour:          OutboundMainProxyTag,
				// Detour: OutboundDirectTag,
			},
			{
				Tag:             DNSTricksDirectTag,
				Address:         "https://9.9.9.9/dns-query",
				AddressResolver: DNSDirectTag,
				Strategy:        directDNSStrategy,
				Detour:          OutboundDirectFragmentTag,
			},
			{
				Tag:             DNSDirectTag,
				Address:         opt.DirectDnsAddress,
				AddressResolver: DNSLocalTag,
				Strategy:        directDNSStrategy,
				Detour:          OutboundDirectFragmentTag,
			},
			{
				Tag:     DNSLocalTag,
				Address: "local",
				Detour:  OutboundDirectTag,
			},
			{
				Tag:     DNSBlockTag,
				Address: "rcode://success",
			},
		},
	}

	options.DNS.StaticIPs["time.apple.com"] = []string{"time.g.aaplimg.com", "time.apple.com"}
	addKnownDoHStaticIPs(options.DNS.StaticIPs)
	options.DNS.StaticIPs["ipinfo.io"] = []string{"ipinfo.io"}
	options.DNS.StaticIPs["ipwho.is"] = []string{"ipwho.is"}
	options.DNS.StaticIPs["api.my-ip.io"] = []string{"api.my-ip.io"}
	options.DNS.StaticIPs["myip.expert"] = []string{"myip.expert"}
	options.DNS.StaticIPs["ip-api.com"] = []string{"ip-api.com"}
	options.DNS.StaticIPs["freeipapi.com"] = []string{"freeipapi.com"}
	options.DNS.StaticIPs["reallyfreegeoip.org"] = []string{"reallyfreegeoip.org"}
	options.DNS.StaticIPs["ipapi.co"] = []string{"ipapi.co"}
	options.DNS.StaticIPs["api.ip.sb"] = []string{"api.ip.sb"}

}

func addForceDirect(options *option.Options, opt *MartenOptions) {
	dnsMap := make(map[string]string)
	endpointsByDetour := make(map[string]*forceDirectEndpointSet)

	for _, outbound := range options.Outbounds {
		outboundOptions, err := outbound.RawOptions()
		if err != nil {
			continue
		}
		if server, ok := outboundOptions.(option.ServerOptionsWrapper); ok {
			serverDomain := server.TakeServerOptions().Server
			detour := OutboundDirectTag
			if dialer, ok := outboundOptions.(option.DialerOptionsWrapper); ok {
				if server_detour := dialer.TakeDialerOptions().Detour; server_detour != "" {
					detour = server_detour
				}
			}

			if host, err := getHostnameIfNotIP(serverDomain); err == nil {
				if _, ok := dnsMap[host]; !ok || detour == OutboundDirectTag {
					dnsMap[host] = detour
				}
				endpointSetForDetour(endpointsByDetour, detour).domains = append(endpointSetForDetour(endpointsByDetour, detour).domains, host)
			} else if cidr := routeCIDRForEndpointIP(serverDomain); cidr != "" {
				endpointSetForDetour(endpointsByDetour, detour).ipCIDRs = append(endpointSetForDetour(endpointsByDetour, detour).ipCIDRs, cidr)
			}
		}
	}

	if len(dnsMap) > 0 {
		unique_dns_detours := make(map[string]bool)
		for _, detour := range dnsMap {
			unique_dns_detours[detour] = true
		}

		for detour := range unique_dns_detours {
			dns_detour := DNSLocalTag
			if detour != OutboundDirectTag {
				dns_detour = "dns-" + detour
				options.DNS.Servers = append(options.DNS.Servers, option.DNSServerOptions{
					Tag: dns_detour,
					// Per-outbound force-direct DNS server follows the detour's
					// transport capability: UDP for WireGuard/AmneziaWG detours,
					// TCP for TCP-only detours.
					Address:         remoteDnsAddressForDetour(opt.RemoteDnsAddress, options.Outbounds, detour),
					AddressResolver: DNSDirectTag,
					Strategy:        opt.RemoteDnsDomainStrategy,
					Detour:          detour,
				})
			}

			domains := []string{}
			for domain, d := range dnsMap {
				if d == detour {
					domains = append(domains, domain)
				}
			}

			if len(domains) == 0 {
				continue
			}
			options.DNS.Rules = append(
				[]option.DNSRule{
					{
						Type: C.RuleTypeDefault,
						DefaultOptions: option.DefaultDNSRule{
							Server: dns_detour,
							Domain: domains,
						},
					},
				},
				options.DNS.Rules...,
			)
		}

	}
	addForceDirectRouteRules(options, endpointsByDetour)

}

type forceDirectEndpointSet struct {
	domains []string
	ipCIDRs []string
}

func endpointSetForDetour(sets map[string]*forceDirectEndpointSet, detour string) *forceDirectEndpointSet {
	set := sets[detour]
	if set == nil {
		set = &forceDirectEndpointSet{}
		sets[detour] = set
	}
	return set
}

func addForceDirectRouteRules(options *option.Options, endpointsByDetour map[string]*forceDirectEndpointSet) {
	if options.Route == nil || len(endpointsByDetour) == 0 {
		return
	}
	for detour, endpoints := range endpointsByDetour {
		domains := removeDuplicateStr(endpoints.domains)
		ipCIDRs := removeDuplicateStr(endpoints.ipCIDRs)
		if len(domains) > 0 {
			for _, domain := range domains {
				resolvedIPs := getIPs(domain)
				ipCIDRs = append(ipCIDRs, routeCIDRsForEndpointIPs(resolvedIPs)...)
				if len(resolvedIPs) > 0 {
					options.DNS.StaticIPs[domain] = removeDuplicateStr(append(options.DNS.StaticIPs[domain], resolvedIPs...))
				}
			}
		}
		ipCIDRs = removeDuplicateStr(ipCIDRs)
		if len(domains) == 0 && len(ipCIDRs) == 0 {
			continue
		}
		options.Route.Rules = append(
			[]option.Rule{
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						Domain:   domains,
						IPCIDR:   ipCIDRs,
						Outbound: detour,
					},
				},
			},
			options.Route.Rules...,
		)
	}
}

func routeCIDRsForEndpointIPs(ips []string) []string {
	cidrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		if cidr := routeCIDRForEndpointIP(ip); cidr != "" {
			cidrs = append(cidrs, cidr)
		}
	}
	return cidrs
}

func routeCIDRForEndpointIP(ip string) string {
	addr, err := netip.ParseAddr(strings.Trim(ip, "[]"))
	if err != nil {
		return ""
	}
	if addr.Is4() {
		return addr.String() + "/32"
	}
	return addr.String() + "/128"
}

func setFakeDns(options *option.Options, opt *MartenOptions) {
	if opt.EnableFakeDNS {
		inet4Range := netip.MustParsePrefix("198.18.0.0/15")
		inet6Range := netip.MustParsePrefix("fc00::/18")
		options.DNS.FakeIP = &option.DNSFakeIPOptions{
			Enabled:    true,
			Inet4Range: &inet4Range,
			Inet6Range: &inet6Range,
		}
		options.DNS.Servers = append(
			options.DNS.Servers,
			option.DNSServerOptions{
				Tag:      DNSFakeTag,
				Address:  "fakeip",
				Strategy: option.DomainStrategy(dns.DomainStrategyUseIPv4),
			},
		)
		options.DNS.Rules = append(
			options.DNS.Rules,
			option.DNSRule{
				Type: C.RuleTypeDefault,
				DefaultOptions: option.DefaultDNSRule{
					Inbound:      []string{InboundTUNTag},
					Server:       DNSFakeTag,
					DisableCache: true,
				},
			},
		)

	}
}

func setRoutingOptions(options *option.Options, opt *MartenOptions) {
	dnsRules := []option.DefaultDNSRule{}
	routeRules := []option.Rule{}
	rulesets := []option.RuleSet{}

	if opt.EnableTun && runtime.GOOS == "android" {
		// routeRules = append(
		// 	routeRules,
		// 	option.Rule{
		// 		Type: C.RuleTypeDefault,

		// 		DefaultOptions: option.DefaultRule{
		// 			Inbound:     []string{InboundTUNTag},
		// 			PackageName: []string{"app.marten.client"},
		// 			Outbound:    OutboundBypassTag,
		// 		},
		// 	},
		// )
	}
	if opt.EnableTun && runtime.GOOS == "windows" {
		// routeRules = append(
		// 	routeRules,
		// 	option.Rule{
		// 		Type: C.RuleTypeDefault,
		// 		DefaultOptions: option.DefaultRule{
		// 			ProcessName: []string{"Marten", "Marten.exe", "MartenCli", "MartenCli.exe"},
		// 			Outbound:    OutboundBypassTag,
		// 		},
		// 	},
		// )
	}
	routeRules = append(routeRules, option.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			Inbound:  []string{InboundDNSTag},
			Outbound: OutboundDNSTag,
		},
	})
	if opt.EnableTun {
		routeRules = append(routeRules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				Inbound:  []string{InboundTUNTag},
				Network:  []string{"tcp"},
				IPCIDR:   []string{"172.19.0.2/32", "fdfe:dcba:9876::2/128"},
				Port:     []uint16{853},
				Outbound: OutboundBlockTag,
			},
		})
	}
	routeRules = append(routeRules, option.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			Port:     []uint16{53},
			Outbound: OutboundDNSTag,
		},
	})
	routeRules = append(routeRules, option.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			IPCIDR:   []string{"10.10.34.0/24"},
			Outbound: OutboundMainProxyTag,
		},
	})
	// {
	// 	Type: C.RuleTypeDefault,
	// 	DefaultOptions: option.DefaultRule{
	// 		ClashMode: "Direct",
	// 		Outbound:  OutboundDirectTag,
	// 	},
	// },
	// {
	// 	Type: C.RuleTypeDefault,
	// 	DefaultOptions: option.DefaultRule{
	// 		ClashMode: "Global",
	// 		Outbound:  OutboundMainProxyTag,
	// 	},
	// },	}

	if opt.BypassLAN {
		routeRules = append(
			routeRules,
			option.Rule{
				Type: C.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					// GeoIP:    []string{"private"},
					IPIsPrivate: true,
					Outbound:    OutboundBypassTag,
				},
			},
		)
	}

	// for _, rule := range opt.Rules {
	// 	routeRule := rule.MakeRule()
	// 	switch rule.Outbound {
	// 	case "bypass":
	// 		routeRule.Outbound = OutboundBypassTag
	// 	case "block":
	// 		routeRule.Outbound = OutboundBlockTag
	// 	case "proxy":
	// 		routeRule.Outbound = OutboundMainProxyTag
	// 	}

	// 	if routeRule.IsValid() {
	// 		routeRules = append(
	// 			routeRules,
	// 			option.Rule{
	// 				Type:           C.RuleTypeDefault,
	// 				DefaultOptions: routeRule,
	// 			},
	// 		)
	// 	}

	// 	dnsRule := rule.MakeDNSRule()
	// 	switch rule.Outbound {
	// 	case "bypass":
	// 		dnsRule.Server = DNSDirectTag
	// 	case "block":
	// 		dnsRule.Server = DNSBlockTag
	// 		dnsRule.DisableCache = true
	// 	case "proxy":
	// 		if opt.EnableFakeDNS {
	// 			fakeDnsRule := dnsRule
	// 			fakeDnsRule.Server = DNSFakeTag
	// 			fakeDnsRule.Inbound = []string{InboundTUNTag, InboundMixedTag}
	// 			dnsRules = append(dnsRules, fakeDnsRule)
	// 		}
	// 		dnsRule.Server = DNSRemoteTag
	// 	}
	// 	dnsRules = append(dnsRules, dnsRule)
	// }

	parsedURL, err := url.Parse(opt.ConnectionTestUrl)
	var dnsCPttl uint32 = 30000
	if err == nil {
		dnsRules = append(dnsRules, option.DefaultDNSRule{
			Domain:       []string{parsedURL.Host},
			Server:       DNSRemoteTag,
			RewriteTTL:   &dnsCPttl,
			DisableCache: false,
		})
	}
	if options.NTP != nil && options.NTP.Enabled && options.NTP.Server != "" {
		dnsRules = append(dnsRules, option.DefaultDNSRule{
			Domain:       []string{options.NTP.Server},
			Server:       DNSDirectTag,
			RewriteTTL:   &dnsCPttl,
			DisableCache: false,
		})

		routeRules = append(routeRules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				Domain:   []string{options.NTP.Server},
				Outbound: OutboundDirectTag,
			},
		})
	}

	if opt.BlockAds {
		rulesets = append(rulesets, option.RuleSet{
			Type:   C.RuleSetTypeRemote,
			Tag:    "geosite-ads",
			Format: C.RuleSetFormatBinary,
			RemoteOptions: option.RemoteRuleSet{
				URL:            "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-category-ads-all.srs",
				UpdateInterval: option.Duration(5 * time.Hour * 24),
			},
		})

		routeRules = append(routeRules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				RuleSet: []string{
					"geosite-ads",
				},
				Outbound: OutboundBlockTag,
			},
		})
		dnsRules = append(dnsRules, option.DefaultDNSRule{
			RuleSet: []string{
				"geosite-ads",
			},
			Server: DNSBlockTag,
			//		DisableCache: true,
		})

	}
	if opt.Region != "other" {
		dnsRules = append(dnsRules, option.DefaultDNSRule{
			DomainSuffix: []string{"." + opt.Region},
			Server:       DNSDirectTag,
		})
		routeRules = append(routeRules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				DomainSuffix: []string{"." + opt.Region},
				Outbound:     OutboundDirectTag,
			},
		})
	}
	if opt.RouteOptions.BlockQuic {
		routeRules = append(routeRules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				Port:     []uint16{443},
				Network:  []string{"udp"},
				Outbound: OutboundBlockTag,
			},
		})
	}
	options.Route = &option.RouteOptions{
		Rules:               routeRules,
		Final:               OutboundMainProxyTag,
		AutoDetectInterface: autoDetectInterfaceForRuntime(runtime.GOOS, opt.EnableTun),
		OverrideAndroidVPN:  overrideAndroidVPNForRuntime(runtime.GOOS, opt.EnableTun),
		RuleSet:             rulesets,
		// GeoIP: &option.GeoIPOptions{
		// 	Path: opt.GeoIPPath,
		// },
		// Geosite: &option.GeositeOptions{
		// 	Path: opt.GeoSitePath,
		// },
	}
	if opt.EnableDNSRouting {
		for _, dnsRule := range dnsRules {
			if dnsRule.IsValid() {
				options.DNS.Rules = append(
					options.DNS.Rules,
					option.DNSRule{
						Type:           C.RuleTypeDefault,
						DefaultOptions: dnsRule,
					},
				)
			}
		}
	}

}

func autoDetectInterfaceForRuntime(goos string, enableTun bool) bool {
	return true
}

func overrideAndroidVPNForRuntime(goos string, enableTun bool) bool {
	return true
}

var (
	ipMaps      = map[string][]string{}
	ipMapsMutex sync.Mutex
)

func lookupEndpointIPs(ctx context.Context, domain string) []string {
	if resolver := loadPlatformEndpointResolver(); resolver != nil {
		addresses, err := resolver(ctx, domain)
		if err != nil {
			return nil
		}
		resolved := make([]string, 0, len(addresses))
		for _, address := range addresses {
			if !address.IsValid() {
				continue
			}
			resolved = append(resolved, address.Unmap().String())
		}
		return resolved
	}

	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", domain)
	if err != nil {
		return nil
	}
	resolved := make([]string, 0, len(addresses))
	for _, address := range addresses {
		resolved = append(resolved, address.String())
	}
	return resolved
}

var endpointLookupSlots = make(chan struct{}, 4)

func getIPs(domains ...string) []string {
	if len(domains) == 0 {
		return nil
	}
	platformResolverRequired := loadPlatformEndpointResolver() != nil
	resChan := make(chan []string, len(domains))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	for _, d := range domains {
		go func(domain string) {
			select {
			case endpointLookupSlots <- struct{}{}:
				defer func() { <-endpointLookupSlots }()
			case <-ctx.Done():
				return
			}
			ips := lookupEndpointIPs(ctx, domain)
			select {
			case resChan <- ips:
			case <-ctx.Done():
			}
		}(d)
	}
	var res []string
collect:
	for remaining := len(domains); remaining > 0; remaining-- {
		select {
		case ips := <-resChan:
			for _, ip := range ips {
				if !isBlockedIP(ip) {
					res = append(res, ip)
				}
			}
		case <-ctx.Done():
			break collect
		}
	}
	if len(res) == 0 {
		// Android's physical resolver is authoritative while installed. A
		// previous network's process cache must not revive an endpoint that the
		// current physical Network could not resolve.
		if platformResolverRequired {
			return nil
		}
		ipMapsMutex.Lock()
		cached := append([]string(nil), ipMaps[domains[0]]...)
		ipMapsMutex.Unlock()
		return cached
	}
	ipMapsMutex.Lock()
	ipMaps[domains[0]] = append([]string(nil), res...)
	ipMapsMutex.Unlock()

	return res
}

func isBlockedDomain(domain string) bool {
	if strings.HasPrefix("full:", domain) {
		return false
	}
	ips := getIPs(domain)
	if len(ips) == 0 {
		// fmt.Println(err)
		return true
	}

	// // Print the IP addresses associated with the domain
	// fmt.Printf("IP addresses for %s:\n", domain)
	// for _, ip := range ips {
	// 	if isBlockedIP(ip) {
	// 		return true
	// 	}
	// }
	return false
}

func isBlockedIP(ip string) bool {
	if strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "2001:4188:2:600:10") {
		return true
	}
	return false
}

func removeDuplicateStr(strSlice []string) []string {
	allKeys := make(map[string]bool)
	list := []string{}
	for _, item := range strSlice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}

func generateRandomString(length int) string {
	// Determine the number of bytes needed
	bytesNeeded := (length*6 + 7) / 8

	// Generate random bytes
	randomBytes := make([]byte, bytesNeeded)
	_, err := rand.Read(randomBytes)
	if err != nil {
		return "marten"
	}

	// Encode random bytes to base64
	randomString := base64.URLEncoding.EncodeToString(randomBytes)

	// Trim padding characters and return the string
	return randomString[:length]
}
