package hcore

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	N "github.com/sagernet/sing/common/network"
)

func TestLocalListenEndpointsExtractsFixedEndpointsDeterministically(t *testing.T) {
	loopback := option.NewListenAddress(netip.MustParseAddr("127.0.0.1"))
	options := &option.Options{
		Inbounds: []option.Inbound{
			{
				Type: C.TypeMixed,
				MixedOptions: option.HTTPMixedInboundOptions{ListenOptions: option.ListenOptions{
					Listen: loopback, ListenPort: 1080,
				}},
			},
			{
				Type: C.TypeHTTP,
				HTTPOptions: option.HTTPMixedInboundOptions{ListenOptions: option.ListenOptions{
					Listen: loopback, ListenPort: 8080,
				}},
			},
			{
				Type: C.TypeSOCKS,
				SocksOptions: option.SocksInboundOptions{ListenOptions: option.ListenOptions{
					Listen: loopback, ListenPort: 1080,
				}},
			},
			{
				Type: C.TypeDirect,
				DirectOptions: option.DirectInboundOptions{ListenOptions: option.ListenOptions{
					Listen: loopback, ListenPort: 5353,
				}},
			},
			{
				Type: C.TypeDirect,
				DirectOptions: option.DirectInboundOptions{
					ListenOptions: option.ListenOptions{Listen: loopback, ListenPort: 5354},
					Network:       option.NetworkList(N.NetworkUDP),
				},
			},
		},
		Experimental: &option.ExperimentalOptions{ClashAPI: &option.ClashAPIOptions{
			ExternalController: "127.0.0.1:9090",
		}},
	}

	want := []localListenEndpoint{
		{network: N.NetworkTCP, address: "127.0.0.1:1080", port: 1080},
		{network: N.NetworkTCP, address: "127.0.0.1:5353", port: 5353},
		{network: N.NetworkUDP, address: "127.0.0.1:5353", port: 5353},
		{network: N.NetworkUDP, address: "127.0.0.1:5354", port: 5354},
		{network: N.NetworkTCP, address: "127.0.0.1:8080", port: 8080},
		{network: N.NetworkTCP, address: "127.0.0.1:9090", port: 9090},
	}
	if got := localListenEndpoints(options); !reflect.DeepEqual(got, want) {
		t.Fatalf("localListenEndpoints() = %#v, want %#v", got, want)
	}
}

func TestLocalListenEndpointsCoversEveryFixedListenerInbound(t *testing.T) {
	loopback4 := option.NewListenAddress(netip.MustParseAddr("127.0.0.1"))
	loopback6 := option.NewListenAddress(netip.MustParseAddr("::1"))
	wildcard4 := option.NewListenAddress(netip.IPv4Unspecified())
	listen := func(port uint16) option.ListenOptions {
		return option.ListenOptions{Listen: loopback4, ListenPort: port}
	}
	endpoint := func(network, address string, port uint16) localListenEndpoint {
		return localListenEndpoint{network: network, address: net.JoinHostPort(address, strconv.Itoa(int(port))), port: port}
	}
	both := func(address string, port uint16) []localListenEndpoint {
		return []localListenEndpoint{
			endpoint(N.NetworkTCP, address, port),
			endpoint(N.NetworkUDP, address, port),
		}
	}

	tests := []struct {
		name     string
		inbounds []option.Inbound
		want     []localListenEndpoint
	}{
		{
			name: "redirect TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeRedirect, RedirectOptions: option.RedirectInboundOptions{ListenOptions: listen(11001)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11001)},
		},
		{
			name: "tproxy defaults to TCP and UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeTProxy, TProxyOptions: option.TProxyInboundOptions{ListenOptions: listen(11002)},
			}},
			want: both("127.0.0.1", 11002),
		},
		{
			name: "direct defaults to TCP and UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeDirect, DirectOptions: option.DirectInboundOptions{ListenOptions: listen(11003)},
			}},
			want: both("127.0.0.1", 11003),
		},
		{
			name: "socks TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeSOCKS, SocksOptions: option.SocksInboundOptions{ListenOptions: listen(11004)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11004)},
		},
		{
			name: "HTTP TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeHTTP, HTTPOptions: option.HTTPMixedInboundOptions{ListenOptions: listen(11005)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11005)},
		},
		{
			name: "mixed TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeMixed, MixedOptions: option.HTTPMixedInboundOptions{ListenOptions: listen(11006)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11006)},
		},
		{
			name: "shadowsocks defaults to TCP and UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeShadowsocks, ShadowsocksOptions: option.ShadowsocksInboundOptions{ListenOptions: listen(11007)},
			}},
			want: both("127.0.0.1", 11007),
		},
		{
			name: "VMess TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeVMess, VMessOptions: option.VMessInboundOptions{ListenOptions: listen(11008)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11008)},
		},
		{
			name: "VLESS TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeVLESS, VLESSOptions: option.VLESSInboundOptions{ListenOptions: listen(11009)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11009)},
		},
		{
			name: "trojan TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeTrojan, TrojanOptions: option.TrojanInboundOptions{ListenOptions: listen(11010)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11010)},
		},
		{
			name: "naive defaults to TCP and UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeNaive, NaiveOptions: option.NaiveInboundOptions{ListenOptions: listen(11011)},
			}},
			want: both("127.0.0.1", 11011),
		},
		{
			name: "hysteria UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeHysteria, HysteriaOptions: option.HysteriaInboundOptions{ListenOptions: listen(11012)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkUDP, "127.0.0.1", 11012)},
		},
		{
			name: "ShadowTLS TCP",
			inbounds: []option.Inbound{{
				Type: C.TypeShadowTLS, ShadowTLSOptions: option.ShadowTLSInboundOptions{ListenOptions: listen(11013)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11013)},
		},
		{
			name: "TUIC UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeTUIC, TUICOptions: option.TUICInboundOptions{ListenOptions: listen(11014)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkUDP, "127.0.0.1", 11014)},
		},
		{
			name: "hysteria2 UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeHysteria2, Hysteria2Options: option.Hysteria2InboundOptions{ListenOptions: listen(11015)},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkUDP, "127.0.0.1", 11015)},
		},
		{
			name: "VMess QUIC transport UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeVMess,
				VMessOptions: option.VMessInboundOptions{
					ListenOptions: listen(11019),
					Transport:     &option.V2RayTransportOptions{Type: C.V2RayTransportTypeQUIC},
				},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkUDP, "127.0.0.1", 11019)},
		},
		{
			name: "VLESS QUIC transport UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeVLESS,
				VLESSOptions: option.VLESSInboundOptions{
					ListenOptions: listen(11020),
					Transport:     &option.V2RayTransportOptions{Type: C.V2RayTransportTypeQUIC},
				},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkUDP, "127.0.0.1", 11020)},
		},
		{
			name: "trojan QUIC transport UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeTrojan,
				TrojanOptions: option.TrojanInboundOptions{
					ListenOptions: listen(11021),
					Transport:     &option.V2RayTransportOptions{Type: C.V2RayTransportTypeQUIC},
				},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkUDP, "127.0.0.1", 11021)},
		},
		{
			name: "IPv6",
			inbounds: []option.Inbound{{
				Type: C.TypeMixed,
				MixedOptions: option.HTTPMixedInboundOptions{ListenOptions: option.ListenOptions{
					Listen: loopback6, ListenPort: 11016,
				}},
			}},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "::1", 11016)},
		},
		{
			name: "wildcard direct preserves TCP and UDP",
			inbounds: []option.Inbound{{
				Type: C.TypeDirect,
				DirectOptions: option.DirectInboundOptions{ListenOptions: option.ListenOptions{
					Listen: wildcard4, ListenPort: 11017,
				}},
			}},
			want: both("0.0.0.0", 11017),
		},
		{
			name: "deduplicates matching protocol address and port",
			inbounds: []option.Inbound{
				{Type: C.TypeMixed, MixedOptions: option.HTTPMixedInboundOptions{ListenOptions: listen(11018)}},
				{Type: C.TypeHTTP, HTTPOptions: option.HTTPMixedInboundOptions{ListenOptions: listen(11018)}},
			},
			want: []localListenEndpoint{endpoint(N.NetworkTCP, "127.0.0.1", 11018)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localListenEndpoints(&option.Options{Inbounds: tt.inbounds})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("localListenEndpoints() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLocalListenerProbesDetectHeldTCPAndUDP(t *testing.T) {
	tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpEndpoint := endpointFromAddr(t, N.NetworkTCP, tcpListener.Addr())
	if err := probeLocalListener(context.Background(), tcpEndpoint); err == nil {
		t.Fatal("TCP probe succeeded while its endpoint was held")
	}
	if err := tcpListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probeLocalListener(context.Background(), tcpEndpoint); err != nil {
		t.Fatalf("TCP probe after holder close: %v", err)
	}

	udpListener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpListener.Close()
	udpEndpoint := endpointFromAddr(t, N.NetworkUDP, udpListener.LocalAddr())
	if err := probeLocalListener(context.Background(), udpEndpoint); err == nil {
		t.Fatal("UDP probe succeeded while its endpoint was held")
	}
	if err := udpListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probeLocalListener(context.Background(), udpEndpoint); err != nil {
		t.Fatalf("UDP probe after holder close: %v", err)
	}
}

func TestWaitForLocalListenersWaitsForTransientHolder(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFromAddr(t, N.NetworkTCP, listener.Addr())
	release := make(chan struct{})
	go func() {
		<-release
		_ = listener.Close()
	}()

	result := make(chan error, 1)
	go func() {
		result <- waitForLocalListeners(context.Background(), []localListenEndpoint{endpoint}, "test transient holder", time.Second)
	}()
	select {
	case err := <-result:
		t.Fatalf("wait returned while endpoint was held: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("wait after holder release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not finish after transient holder released")
	}
}

func TestWaitForLocalListenersTimesOutAndHonorsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	endpoint := endpointFromAddr(t, N.NetworkTCP, listener.Addr())

	if err := waitForLocalListeners(context.Background(), []localListenEndpoint{endpoint}, "test timeout", 40*time.Millisecond); err == nil {
		t.Fatal("wait succeeded while endpoint remained held")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = waitForLocalListeners(ctx, []localListenEndpoint{endpoint}, "test cancellation", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context cancellation", err)
	}
}

func endpointFromAddr(t *testing.T, network string, address net.Addr) localListenEndpoint {
	t.Helper()
	host, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		t.Fatalf("split listener address %q: %v", address, err)
	}
	port, err := netip.ParseAddrPort(net.JoinHostPort(host, portText))
	if err != nil {
		t.Fatalf("parse listener address %q: %v", address, err)
	}
	return localListenEndpoint{network: network, address: port.String(), port: port.Port()}
}
