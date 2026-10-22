package hcore

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	N "github.com/sagernet/sing/common/network"
)

const (
	localListenPollPeriod    = 200 * time.Millisecond
	localListenWarningPeriod = 12 * time.Second
)

// localListenEndpoint is a fixed process-local listener that must be free
// before a new Box is allowed to start. Address is kept so the probe uses the
// same IPv4/IPv6 and wildcard semantics as sing-box instead of assuming that a
// successful 127.0.0.1 TCP dial proves ownership or availability.
type localListenEndpoint struct {
	network string
	address string
	port    uint16
}

func cloneLocalListenEndpoints(endpoints []localListenEndpoint) []localListenEndpoint {
	return append([]localListenEndpoint(nil), endpoints...)
}

func localListenEndpoints(options *option.Options) []localListenEndpoint {
	if options == nil {
		return nil
	}

	seen := make(map[string]localListenEndpoint)
	add := func(network string, listenOptions option.ListenOptions) {
		if network != N.NetworkTCP && network != N.NetworkUDP {
			return
		}
		if listenOptions.ListenPort == 0 {
			return
		}
		address := listenOptions.Listen.Build()
		if !address.IsValid() {
			return
		}
		endpoint := localListenEndpoint{
			network: network,
			address: net.JoinHostPort(address.String(), strconv.Itoa(int(listenOptions.ListenPort))),
			port:    listenOptions.ListenPort,
		}
		seen[endpoint.network+"\x00"+endpoint.address] = endpoint
	}
	addNetworks := func(networks []string, listenOptions option.ListenOptions) {
		for _, network := range networks {
			add(network, listenOptions)
		}
	}

	for _, inbound := range options.Inbounds {
		switch inbound.Type {
		case C.TypeMixed:
			add(N.NetworkTCP, inbound.MixedOptions.ListenOptions)
		case C.TypeHTTP:
			add(N.NetworkTCP, inbound.HTTPOptions.ListenOptions)
		case C.TypeSOCKS:
			add(N.NetworkTCP, inbound.SocksOptions.ListenOptions)
		case C.TypeRedirect:
			add(N.NetworkTCP, inbound.RedirectOptions.ListenOptions)
		case C.TypeTProxy:
			addNetworks(inbound.TProxyOptions.Network.Build(), inbound.TProxyOptions.ListenOptions)
		case C.TypeDirect:
			addNetworks(inbound.DirectOptions.Network.Build(), inbound.DirectOptions.ListenOptions)
		case C.TypeShadowsocks:
			addNetworks(inbound.ShadowsocksOptions.Network.Build(), inbound.ShadowsocksOptions.ListenOptions)
		case C.TypeVMess:
			addNetworks(v2rayInboundNetworks(inbound.VMessOptions.Transport), inbound.VMessOptions.ListenOptions)
		case C.TypeVLESS:
			addNetworks(v2rayInboundNetworks(inbound.VLESSOptions.Transport), inbound.VLESSOptions.ListenOptions)
		case C.TypeTrojan:
			addNetworks(v2rayInboundNetworks(inbound.TrojanOptions.Transport), inbound.TrojanOptions.ListenOptions)
		case C.TypeNaive:
			addNetworks(inbound.NaiveOptions.Network.Build(), inbound.NaiveOptions.ListenOptions)
		case C.TypeHysteria:
			add(N.NetworkUDP, inbound.HysteriaOptions.ListenOptions)
		case C.TypeShadowTLS:
			add(N.NetworkTCP, inbound.ShadowTLSOptions.ListenOptions)
		case C.TypeTUIC:
			add(N.NetworkUDP, inbound.TUICOptions.ListenOptions)
		case C.TypeHysteria2:
			add(N.NetworkUDP, inbound.Hysteria2Options.ListenOptions)
		}
	}

	if options.Experimental != nil && options.Experimental.ClashAPI != nil {
		if endpoint, ok := localTCPListenEndpoint(options.Experimental.ClashAPI.ExternalController); ok {
			seen[endpoint.network+"\x00"+endpoint.address] = endpoint
		}
	}

	endpoints := make([]localListenEndpoint, 0, len(seen))
	for _, endpoint := range seen {
		endpoints = append(endpoints, endpoint)
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].port != endpoints[j].port {
			return endpoints[i].port < endpoints[j].port
		}
		if endpoints[i].network != endpoints[j].network {
			return endpoints[i].network < endpoints[j].network
		}
		return endpoints[i].address < endpoints[j].address
	})
	return endpoints
}

// VMess, VLESS, and Trojan use a TCP listener without a V2Ray transport. All
// supported transports keep that listener except QUIC, whose server binds UDP.
func v2rayInboundNetworks(transport *option.V2RayTransportOptions) []string {
	if transport != nil && transport.Type == C.V2RayTransportTypeQUIC {
		return []string{N.NetworkUDP}
	}
	return []string{N.NetworkTCP}
}

func localTCPListenEndpoint(address string) (localListenEndpoint, bool) {
	address = strings.TrimSpace(address)
	if address == "" {
		return localListenEndpoint{}, false
	}

	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		port, parseErr := strconv.ParseUint(address, 10, 16)
		if parseErr != nil || port == 0 {
			return localListenEndpoint{}, false
		}
		host = netip.MustParseAddr("127.0.0.1").String()
		portString = address
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil || port == 0 {
		return localListenEndpoint{}, false
	}
	return localListenEndpoint{
		network: N.NetworkTCP,
		address: net.JoinHostPort(host, portString),
		port:    uint16(port),
	}, true
}

func waitForLocalListeners(
	ctx context.Context,
	endpoints []localListenEndpoint,
	reason string,
	timeout time.Duration,
) error {
	if len(endpoints) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	pollTicker := time.NewTicker(localListenPollPeriod)
	defer pollTicker.Stop()
	warningTicker := time.NewTicker(localListenWarningPeriod)
	defer warningTicker.Stop()

	var timeoutTimer *time.Timer
	var timeoutChannel <-chan time.Time
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		timeoutChannel = timeoutTimer.C
		defer timeoutTimer.Stop()
	}

	waiting := false
	for {
		busy := unavailableLocalListeners(ctx, endpoints)
		if len(busy) == 0 {
			if waiting {
				Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("local listeners released during %s", reason))
			}
			return nil
		}
		if !waiting {
			waiting = true
			Log(
				LogLevel_DEBUG,
				LogType_CORE,
				fmt.Sprintf("waiting for local listeners during %s: %s", reason, summarizeLocalListeners(busy)),
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("local listener wait cancelled during %s: %w", reason, ctx.Err())
		case <-timeoutChannel:
			return fmt.Errorf(
				"local listener admission did not become ready within %s during %s; busy: %s",
				timeout,
				reason,
				summarizeLocalListeners(busy),
			)
		case <-warningTicker.C:
			Log(
				LogLevel_WARNING,
				LogType_CORE,
				fmt.Sprintf("local listeners are still busy during %s: %s", reason, summarizeLocalListeners(busy)),
			)
		case <-pollTicker.C:
		}
	}
}

func unavailableLocalListeners(
	ctx context.Context,
	endpoints []localListenEndpoint,
) []localListenEndpoint {
	busy := make([]localListenEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if err := probeLocalListener(ctx, endpoint); err != nil {
			busy = append(busy, endpoint)
		}
	}
	return busy
}

func probeLocalListener(ctx context.Context, endpoint localListenEndpoint) error {
	network := endpoint.network
	if host, _, err := net.SplitHostPort(endpoint.address); err == nil && host != "" {
		if address, parseErr := netip.ParseAddr(host); parseErr == nil {
			if address.Is4() {
				network += "4"
			} else if address.Is6() {
				network += "6"
			}
		}
	}

	var listenConfig net.ListenConfig
	switch endpoint.network {
	case N.NetworkTCP:
		listener, err := listenConfig.Listen(ctx, network, endpoint.address)
		if err != nil {
			return err
		}
		_ = listener.Close()
		return nil
	case N.NetworkUDP:
		packetConn, err := listenConfig.ListenPacket(ctx, network, endpoint.address)
		if err != nil {
			return err
		}
		_ = packetConn.Close()
		return nil
	default:
		return fmt.Errorf("unsupported local listener network %q", endpoint.network)
	}
}

func summarizeLocalListeners(endpoints []localListenEndpoint) string {
	summaries := make([]string, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		summary := endpoint.network + "/" + strconv.Itoa(int(endpoint.port))
		if _, exists := seen[summary]; exists {
			continue
		}
		seen[summary] = struct{}{}
		summaries = append(summaries, summary)
	}
	sort.Strings(summaries)
	return strings.Join(summaries, ",")
}
