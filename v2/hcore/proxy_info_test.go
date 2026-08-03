package hcore

import (
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stoppedProxyInfoStream struct {
	grpc.ServerStream
}

func (stoppedProxyInfoStream) Send(*OutboundGroupList) error {
	return nil
}

func TestAllProxiesInfoStreamWhenCoreStoppedReturnsUnavailable(t *testing.T) {
	static.lock.Lock()
	previousBox := static.Box
	previousState := static.CoreState
	static.Box = nil
	static.CoreState = CoreStates_STOPPED
	static.lock.Unlock()
	t.Cleanup(func() {
		static.lock.Lock()
		static.Box = previousBox
		static.CoreState = previousState
		static.lock.Unlock()
	})

	err := AllProxiesInfoStream(stoppedProxyInfoStream{}, true)

	if status.Code(err) != codes.Unavailable {
		t.Fatalf("status.Code(err) = %v, want Unavailable; err = %v", status.Code(err), err)
	}
}

func TestGetAllProxiesInfoWhenCoreStoppedReturnsNil(t *testing.T) {
	static.lock.Lock()
	previousBox := static.Box
	previousState := static.CoreState
	static.Box = nil
	static.CoreState = CoreStates_STOPPED
	static.lock.Unlock()
	t.Cleanup(func() {
		static.lock.Lock()
		static.Box = previousBox
		static.CoreState = previousState
		static.lock.Unlock()
	})

	if groups := GetAllProxiesInfo(true); groups != nil {
		t.Fatalf("GetAllProxiesInfo returned %#v, want nil", groups)
	}
}
