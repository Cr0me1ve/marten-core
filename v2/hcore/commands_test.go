package hcore

import (
	"context"
	"strings"
	"testing"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"google.golang.org/protobuf/proto"
)

func TestSelectOutboundWhenCoreStoppedReturnsFailure(t *testing.T) {
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

	response, err := SelectOutbound(&SelectOutboundRequest{
		GroupTag:    "select",
		OutboundTag: "example",
	})

	if err == nil {
		t.Fatal("SelectOutbound returned nil error, want failure")
	}
	if response == nil {
		t.Fatal("SelectOutbound returned nil response")
	}
	if response.Code != hcommon.ResponseCode_FAILED {
		t.Fatalf("response.Code = %v, want FAILED", response.Code)
	}
	if response.Message == "" {
		t.Fatal("response.Message is empty")
	}
}

func TestUrlTestWhenCoreStoppedReturnsFailure(t *testing.T) {
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

	response, err := UrlTest(&UrlTestRequest{GroupTag: "select"})

	if err == nil {
		t.Fatal("UrlTest returned nil error, want failure")
	}
	if response == nil {
		t.Fatal("UrlTest returned nil response")
	}
	if response.Code != hcommon.ResponseCode_FAILED {
		t.Fatalf("response.Code = %v, want FAILED", response.Code)
	}
	if response.Message == "" {
		t.Fatal("response.Message is empty")
	}
}

func TestProbeSelectedRouteRejectsNilRequest(t *testing.T) {
	response, err := ProbeSelectedRoute(context.Background(), nil)

	if err == nil {
		t.Fatal("ProbeSelectedRoute returned nil error, want failure")
	}
	if response != nil {
		t.Fatalf("ProbeSelectedRoute response = %#v, want nil", response)
	}
	if !strings.Contains(err.Error(), "request is nil") {
		t.Fatalf("ProbeSelectedRoute error = %q, want nil-request failure", err)
	}
}

func TestProbeSelectedRouteWhenCoreStoppedReturnsFailure(t *testing.T) {
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

	response, err := ProbeSelectedRoute(context.Background(), &UrlTestRequest{GroupTag: "select"})

	if err == nil {
		t.Fatal("ProbeSelectedRoute returned nil error, want failure")
	}
	if response != nil {
		t.Fatalf("ProbeSelectedRoute response = %#v, want nil", response)
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Fatalf("ProbeSelectedRoute error = %q, want stopped-core failure", err)
	}
}

func TestSelectOutboundRequestSkipProbeRoundTrip(t *testing.T) {
	want := &SelectOutboundRequest{
		GroupTag:    "select",
		OutboundTag: "exit",
		SkipProbe:   true,
	}

	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("marshal SelectOutboundRequest: %v", err)
	}
	var got SelectOutboundRequest
	if err := proto.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal SelectOutboundRequest: %v", err)
	}

	if got.GetGroupTag() != want.GetGroupTag() || got.GetOutboundTag() != want.GetOutboundTag() {
		t.Fatalf("request route = %q -> %q, want %q -> %q", got.GetGroupTag(), got.GetOutboundTag(), want.GetGroupTag(), want.GetOutboundTag())
	}
	if !got.GetSkipProbe() {
		t.Fatal("SkipProbe was lost after protobuf round trip")
	}
}
