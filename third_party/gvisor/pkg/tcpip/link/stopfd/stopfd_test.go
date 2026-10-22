//go:build linux

package stopfd

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopWakesPollAndCloseIsIdempotent(t *testing.T) {
	sf, err := New()
	if err != nil {
		t.Fatal(err)
	}
	fd := sf.EFD

	result := make(chan error, 1)
	go func() {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 5000)
		result <- err
	}()

	sf.Stop()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("poll failed after stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not wake the blocked poll")
	}

	sf.Close()
	sf.Close()
	if sf.EFD != -1 {
		t.Fatalf("Close left descriptor open: EFD=%d", sf.EFD)
	}
}

func TestLateStopCannotWriteToReusedDescriptor(t *testing.T) {
	sf, err := New()
	if err != nil {
		t.Fatal(err)
	}
	closedNumber := sf.EFD
	sf.Close()

	// Eventfd should be the lowest newly allocated descriptor, reusing the
	// just-closed number. A late Stop must see EFD=-1 rather than signal it.
	reused, err := unix.Eventfd(0, unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(reused)
	if reused != closedNumber {
		t.Skipf("eventfd number %d was not reused (got %d)", closedNumber, reused)
	}

	sf.Stop()
	var data [8]byte
	_, err = unix.Read(reused, data[:])
	if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
		t.Fatalf("late Stop wrote to a descriptor after close: read bytes=%v, err=%v", data, err)
	}
}

func TestConcurrentStopAndClose(t *testing.T) {
	sf, err := New()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sf.Stop()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sf.Close()
	}()
	wg.Wait()
	if sf.EFD != -1 {
		t.Fatalf("concurrent Close left descriptor open: EFD=%d", sf.EFD)
	}
}
