//go:build with_gvisor

package tun

import (
	"net/netip"
	"testing"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
)

type attachRecorder struct {
	stack.LinkEndpoint
	attached stack.NetworkDispatcher
	called   bool
}

func (e *attachRecorder) Attach(dispatcher stack.NetworkDispatcher) {
	e.called = true
	e.attached = dispatcher
}

type deliveryRecorder struct {
	stack.NetworkDispatcher
	delivered int
}

func (d *deliveryRecorder) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, _ *stack.PacketBuffer) {
	d.delivered++
}

func TestAttachNilPropagatesDetachWithoutWrapping(t *testing.T) {
	endpoint := &attachRecorder{}
	filter := &LinkEndpointFilter{LinkEndpoint: endpoint}

	filter.Attach(nil)

	if !endpoint.called {
		t.Fatal("underlying endpoint Attach was not called")
	}
	if endpoint.attached != nil {
		t.Fatalf("nil detach was wrapped instead of propagated: %T", endpoint.attached)
	}
}

func TestAttachNonNilWrapsAndDeliversUnfilteredPackets(t *testing.T) {
	endpoint := &attachRecorder{}
	dispatcher := &deliveryRecorder{}
	filter := &LinkEndpointFilter{
		LinkEndpoint:     endpoint,
		BroadcastAddress: netip.MustParseAddr("192.0.2.255"),
	}

	filter.Attach(dispatcher)

	wrapped, ok := endpoint.attached.(*networkDispatcherFilter)
	if !ok {
		t.Fatalf("non-nil dispatcher was not wrapped: got %T", endpoint.attached)
	}
	if wrapped.NetworkDispatcher != dispatcher {
		t.Fatal("wrapper does not retain the attached dispatcher")
	}

	// A short packet cannot contain an IP header, so the filter should preserve
	// the established fallback behavior and deliver it to the network stack.
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData([]byte{1})})
	defer pkt.DecRef()
	wrapped.DeliverNetworkPacket(0x0800, pkt)
	if dispatcher.delivered != 1 {
		t.Fatalf("wrapped dispatcher received %d packets, want 1", dispatcher.delivered)
	}
}
