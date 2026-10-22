//go:build linux

package fdbased

import (
	"os"
	"testing"

	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
	"golang.org/x/sys/unix"
)

type lifetimeDispatcher struct{ stack.NetworkDispatcher }

func (lifetimeDispatcher) DeliverNetworkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}
func (lifetimeDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer)    {}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

func TestAttachNilStopsReaderAndReleasesStopFD(t *testing.T) {
	baseline := openFDCount(t)
	for cycle := 0; cycle < 20; cycle++ {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("socketpair cycle %d: %v", cycle, err)
		}

		endpoint, err := New(&Options{FDs: []int{fds[0]}, MTU: 1500, PacketDispatchMode: Readv})
		if err != nil {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
			t.Fatalf("create endpoint cycle %d: %v", cycle, err)
		}
		endpoint.Attach(lifetimeDispatcher{})
		// The reader is blocked waiting for input. Detaching must signal its
		// eventfd, wait for the read loop to exit, and release the eventfd.
		endpoint.Attach(nil)
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])

		if got := openFDCount(t); got != baseline {
			t.Fatalf("file descriptors after detach cycle %d: got %d, want baseline %d", cycle, got, baseline)
		}
	}
}
