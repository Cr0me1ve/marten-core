package config

import (
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestBuildConfigKeepsICMPSelectableAndPreservesBackend(t *testing.T) {
	input := option.Options{Outbounds: []option.Outbound{{
		Type: C.TypeICMP,
		Tag:  "icmp-wg",
		ICMPOptions: option.ICMPOutboundOptions{
			Server:  "198.51.100.10",
			Servers: []string{"198.51.100.11"},
			Backend: option.ICMPWireGuardBackendOptions{
				Type: "wireguard",
				WireGuardOutboundOptions: option.WireGuardOutboundOptions{
					MTU:        1080,
					PrivateKey: "private",
					ServerOptions: option.ServerOptions{ServerPort: 51820},
					Peers: []option.WireGuardPeer{{PublicKey: "peer", AllowedIPs: []string{"0.0.0.0/0"}}},
				},
			},
			Carrier: option.ICMPCarrierOptions{CarrierKey: "carrier", Payload: 1200, Flows: 6},
		},
	}}}

	output, err := BuildConfig(*DefaultMartenOptions(), input)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	var icmp *option.Outbound
	for index := range output.Outbounds {
		if output.Outbounds[index].Tag == "icmp-wg" {
			icmp = &output.Outbounds[index]
		}
	}
	if icmp == nil || icmp.Type != C.TypeICMP {
		t.Fatalf("ICMP outbound missing from result: %#v", output.Outbounds)
	}
	if got := output.Outbounds[0].SelectorOptions.Outbounds; len(got) != 1 || got[0] != "icmp-wg" {
		t.Fatalf("selector outbounds = %#v, want icmp-wg selectable", got)
	}
	if icmp.ICMPOptions.Backend.Type != "wireguard" || icmp.ICMPOptions.Backend.MTU != 1080 || len(icmp.ICMPOptions.Backend.Peers) != 1 || icmp.ICMPOptions.Backend.Peers[0].PublicKey != "peer" {
		t.Fatalf("nested ICMP WireGuard backend lost: %#v", icmp.ICMPOptions.Backend)
	}
	if got := icmp.ICMPOptions.Carrier; got.CarrierKey != "carrier" || got.Payload != 1200 || got.Flows != 6 {
		t.Fatalf("carrier settings lost: %#v", got)
	}
}
