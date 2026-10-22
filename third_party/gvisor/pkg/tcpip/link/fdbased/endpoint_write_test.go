//go:build linux

package fdbased

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/rawfile"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
	"golang.org/x/sys/unix"
)

func writeTestPacket(reserve, pushed int, fragments ...[]byte) *stack.PacketBuffer {
	var payload buffer.Buffer
	for _, fragment := range fragments {
		payload.Append(buffer.NewViewWithData(fragment))
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{ReserveHeaderBytes: reserve, Payload: payload})
	if pushed != 0 {
		copy(pkt.NetworkHeader().Push(pushed), bytes.Repeat([]byte{0x45}, pushed))
	}
	return pkt
}

func TestWritePacketFragments(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		reserve, pushed, limit, fragments int
		gso                               bool
	}{
		{name: "contiguous", limit: rawfile.MaxIovs, fragments: 1},
		{name: "partial-reserve", reserve: 80, pushed: 20, limit: rawfile.MaxIovs, fragments: 2},
		{name: "skip-reserve", reserve: 80, limit: rawfile.MaxIovs, fragments: 3},
		{name: "header-only", reserve: 80, pushed: 20, limit: rawfile.MaxIovs},
		{name: "heap-iovecs", reserve: 80, pushed: 40, limit: rawfile.MaxIovs, fragments: 12},
		{name: "coalesce", reserve: 80, pushed: 20, limit: 2, fragments: 12},
		{name: "one-iovec", reserve: 80, pushed: 20, limit: 1, fragments: 3},
		{name: "virtio", reserve: 80, pushed: 20, limit: rawfile.MaxIovs, fragments: 3, gso: true},
		{name: "virtio-coalesce", reserve: 80, pushed: 20, limit: 1, fragments: 3, gso: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[0])
			defer unix.Close(fds[1])
			parts := make([][]byte, tc.fragments)
			for i := range parts {
				parts[i] = bytes.Repeat([]byte{byte(i + 1)}, 73+i)
			}
			pkt := writeTestPacket(tc.reserve, tc.pushed, parts...)
			defer pkt.DecRef()
			original := bytes.Join(pkt.AsSlices(), nil)
			e := endpoint{fds: []fdInfo{{fd: fds[0]}}, writevMaxIovs: tc.limit}
			want := append([]byte(nil), original...)
			if tc.gso {
				e.gsoKind = stack.HostGSOSupported
				pkt.GSOOptions = stack.GSO{Type: stack.GSOTCPv4, MSS: 100, NeedsCsum: true, L3HdrLen: 20, CsumOffset: 16}
				hdr := virtioNetHdr{flags: _VIRTIO_NET_HDR_F_NEEDS_CSUM, hdrLen: uint16(tc.pushed), gsoType: _VIRTIO_NET_HDR_GSO_TCPV4, gsoSize: 100, csumStart: 20, csumOffset: 16}
				want = append(hdr.marshal(), want...)
			}
			for repeat := 0; repeat < 2; repeat++ {
				if err := e.writePacket(pkt); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(want)+16)
				n, err := unix.Read(fds[1], got)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got[:n], want) {
					t.Fatalf("packet differs: got %x, want %x", got[:n], want)
				}
				if !bytes.Equal(bytes.Join(pkt.AsSlices(), nil), original) {
					t.Fatal("write mutated packet")
				}
			}
		})
	}
}

func TestNonSocketBatchKeepsPacketBoundaries(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	e := endpoint{fds: []fdInfo{{fd: fds[0], isSocket: false}}, writevMaxIovs: rawfile.MaxIovs}
	var packets stack.PacketBufferList
	for i := 0; i < 4; i++ {
		packets.PushBack(writeTestPacket(64, 20, bytes.Repeat([]byte{byte(i)}, 101+i)))
	}
	defer packets.Reset()
	if n, err := e.WritePackets(packets); n != 4 || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	for _, pkt := range packets.AsSlice() {
		got := make([]byte, 2048)
		n, err := unix.Read(fds[1], got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:n], bytes.Join(pkt.AsSlices(), nil)) {
			t.Fatal("packet boundary or contents changed")
		}
	}
	if _, err := unix.Read(fds[1], make([]byte, 1)); err != unix.EAGAIN {
		t.Fatalf("unexpected extra packet: %v", err)
	}
	// A closed descriptor must report failure immediately, without retry loops.
	e.fds[0].fd = -1
	if n, err := e.WritePackets(packets); n != 0 || err == nil {
		t.Fatalf("closed descriptor = %d, %v", n, err)
	}
}

func BenchmarkWritePacket(b *testing.B) {
	for _, fragments := range []int{1, 2, 4, 12} {
		b.Run(fmt.Sprintf("fragments=%d", fragments), func(b *testing.B) {
			f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			parts := make([][]byte, fragments)
			for i := range parts {
				parts[i] = bytes.Repeat([]byte{byte(i)}, 1400/fragments)
			}
			pkt := writeTestPacket(80, 40, parts...)
			defer pkt.DecRef()
			e := endpoint{fds: []fdInfo{{fd: int(f.Fd())}}, writevMaxIovs: rawfile.MaxIovs}
			b.ReportAllocs()
			b.SetBytes(int64(pkt.Size()))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.writePacket(pkt); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Include kernel packet copying and draining without WAN/server variance.
// This preserves datagram boundaries but does not model the TUN IP stack.
func BenchmarkWritePacketDatagram(b *testing.B) {
	for _, fragments := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("fragments=%d", fragments), func(b *testing.B) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer unix.Close(fds[0])
			defer unix.Close(fds[1])
			parts := make([][]byte, fragments)
			for i := range parts {
				parts[i] = make([]byte, 1400/fragments)
			}
			pkt := writeTestPacket(80, 40, parts...)
			defer pkt.DecRef()
			e := endpoint{fds: []fdInfo{{fd: fds[0]}}, writevMaxIovs: rawfile.MaxIovs}
			received := make([]byte, pkt.Size())
			b.SetBytes(int64(pkt.Size()))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.writePacket(pkt); err != nil {
					b.Fatal(err)
				}
				if n, err := unix.Read(fds[1], received); err != nil || n != pkt.Size() {
					b.Fatalf("read = %d, %v", n, err)
				}
			}
		})
	}
}

func TestWritePacketsStopsAtFailingFD(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e := endpoint{fds: []fdInfo{{fd: int(f.Fd())}, {fd: -1}}, writevMaxIovs: rawfile.MaxIovs}
	var packets stack.PacketBufferList
	for i := 0; i < 3; i++ {
		pkt := writeTestPacket(80, 40, make([]byte, 100))
		pkt.Hash = uint32(i % 2)
		packets.PushBack(pkt)
	}
	defer packets.Reset()
	if n, err := e.WritePackets(packets); n != 1 || err == nil {
		t.Fatalf("partial batch = %d, %v", n, err)
	}
}

func TestWritePacketConcurrentSharedViews(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pkt := writeTestPacket(80, 40, make([]byte, 700), make([]byte, 700))
	defer pkt.DecRef()
	e := endpoint{fds: []fdInfo{{fd: int(f.Fd())}}, writevMaxIovs: rawfile.MaxIovs}
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			clone := pkt.Clone()
			defer clone.DecRef()
			for j := 0; j < 1000; j++ {
				if err := e.writePacket(clone); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	workers.Wait()
}

func TestWritePacketBackpressureReturnsWithoutWaiting(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	e := endpoint{fds: []fdInfo{{fd: fds[0]}}, writevMaxIovs: rawfile.MaxIovs}
	pkt := writeTestPacket(80, 40, make([]byte, 1400))
	defer pkt.DecRef()
	for i := 0; i < 1024; i++ {
		if err := e.writePacket(pkt); err != nil {
			if _, readErr := unix.Read(fds[1], make([]byte, 1600)); readErr != nil {
				t.Fatal(readErr)
			}
			if err := e.writePacket(pkt); err != nil {
				t.Fatalf("write after draining: %v", err)
			}
			return
		}
	}
	t.Fatal("failed to exercise backpressure within bounded writes")
}

func TestWritePacketCommonPathDoesNotAllocate(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pkt := writeTestPacket(80, 40, make([]byte, 700), make([]byte, 700))
	defer pkt.DecRef()
	e := endpoint{fds: []fdInfo{{fd: int(f.Fd())}}, writevMaxIovs: rawfile.MaxIovs}
	if got := testing.AllocsPerRun(100, func() {
		if err := e.writePacket(pkt); err != nil {
			t.Fatal(err)
		}
	}); got != 0 {
		t.Fatalf("common packet write allocated %g times", got)
	}
}
