package hcore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"google.golang.org/grpc/metadata"
)

type recordingCoreInfoStream struct {
	ctx                context.Context
	lock               sync.Mutex
	sent               chan *CoreInfoResponse
	sendCount          int
	blockUpdates       bool
	updateSendBlocked  chan struct{}
	releaseUpdateSends chan struct{}
}

func newRecordingCoreInfoStream(ctx context.Context) *recordingCoreInfoStream {
	return &recordingCoreInfoStream{
		ctx:                ctx,
		sent:               make(chan *CoreInfoResponse, 16),
		updateSendBlocked:  make(chan struct{}, 1),
		releaseUpdateSends: make(chan struct{}),
	}
}

func (stream *recordingCoreInfoStream) Send(response *CoreInfoResponse) error {
	stream.lock.Lock()
	stream.sendCount++
	block := stream.blockUpdates && stream.sendCount > 1
	if block {
		select {
		case stream.updateSendBlocked <- struct{}{}:
		default:
		}
	}
	copy := coreInfoResponse(response.CoreState, response.MessageType, response.Message)
	stream.lock.Unlock()
	if block {
		<-stream.releaseUpdateSends
	}
	stream.sent <- copy
	return nil
}

func (stream *recordingCoreInfoStream) blockUpdateSends() {
	stream.lock.Lock()
	stream.blockUpdates = true
	stream.lock.Unlock()
}

func (stream *recordingCoreInfoStream) releaseBlockedUpdateSends() {
	stream.lock.Lock()
	stream.blockUpdates = false
	stream.lock.Unlock()
	close(stream.releaseUpdateSends)
}

func (stream *recordingCoreInfoStream) SetHeader(metadata.MD) error  { return nil }
func (stream *recordingCoreInfoStream) SendHeader(metadata.MD) error { return nil }
func (stream *recordingCoreInfoStream) SetTrailer(metadata.MD)       {}
func (stream *recordingCoreInfoStream) SendMsg(any) error            { return nil }
func (stream *recordingCoreInfoStream) RecvMsg(any) error            { return nil }
func (stream *recordingCoreInfoStream) Context() context.Context     { return stream.ctx }

func TestCoreInfoListenerSnapshotsAndConcurrentStatusPublicationStayLockSafe(t *testing.T) {
	source := readHcoreSource(t, "coreinfo.go")
	listener := sourceSection(t, source, "func (s *CoreService) CoreInfoListener", "\n}")
	if !strings.Contains(listener, "static.coreInfoObserver.Subscribe()") ||
		!strings.Contains(listener, "snapshot := currentCoreInfoEvent(MessageType_EMPTY, \"\")") ||
		!strings.Contains(listener, "stream.Send(snapshot.info)") {
		t.Fatal("CoreInfoListener must subscribe first and obtain its initial snapshot through the lock-safe helper")
	}
	if !strings.Contains(source, "type coreInfoEvent struct") ||
		!strings.Contains(source, "revision uint64") ||
		!strings.Contains(source, "static.coreInfoRevision++") ||
		!strings.Contains(source, "func currentCoreInfoEvent") ||
		!strings.Contains(source, "static.lock.Lock()") ||
		!strings.Contains(source, "revision: static.coreInfoRevision") {
		t.Fatal("initial CoreInfo snapshot must read lifecycle state while static.lock is held")
	}
	if !strings.Contains(source, "func latestCoreInfoEvent") ||
		!strings.Contains(source, "func sendLatestCoreInfo") ||
		!strings.Contains(source, "for {") ||
		!strings.Contains(listener, "case _, ok := <-coreSub:") ||
		!strings.Contains(listener, "sendLatestCoreInfo(stream, &lastRevision)") {
		t.Fatal("CoreInfoListener must treat observable events only as wake-ups and reconcile to the latest revision")
	}

	static.lock.Lock()
	originalState := static.CoreState
	originalRevision := static.coreInfoRevision
	originalLatest := static.coreInfoLatest
	static.lock.Unlock()
	t.Cleanup(func() {
		static.lock.Lock()
		static.CoreState = originalState
		static.coreInfoRevision = originalRevision
		static.coreInfoLatest = originalLatest
		static.lock.Unlock()
	})
	SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, "listener test initial")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newRecordingCoreInfoStream(ctx)
	finished := make(chan error, 1)
	go func() {
		finished <- (&CoreService{}).CoreInfoListener(&hcommon.Empty{}, stream)
	}()

	select {
	case initial := <-stream.sent:
		if initial.CoreState != CoreStates_STOPPED {
			t.Fatalf("initial listener snapshot = %s, want STOPPED", initial.CoreState)
		}
	case <-time.After(time.Second):
		t.Fatal("CoreInfoListener did not publish its initial snapshot")
	}

	published := make(chan struct{})
	go func() {
		SetCoreStatus(CoreStates_STARTING, MessageType_EMPTY, "concurrent listener publication")
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("concurrent lifecycle publication did not finish")
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case update := <-stream.sent:
			if update.CoreState == CoreStates_STARTING {
				goto receivedConcurrentPublication
			}
		case <-deadline.C:
			t.Fatal("CoreInfoListener did not receive concurrent lifecycle publication")
		}
	}

receivedConcurrentPublication:

	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("CoreInfoListener returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CoreInfoListener did not exit after stream cancellation")
	}
}

func TestCoreInfoListenerReconcilesLatestTerminalStateAfterBlockedSendAndWakeSaturation(t *testing.T) {
	static.lock.Lock()
	originalState := static.CoreState
	originalRevision := static.coreInfoRevision
	originalLatest := static.coreInfoLatest
	static.lock.Unlock()
	t.Cleanup(func() {
		static.lock.Lock()
		static.CoreState = originalState
		static.coreInfoRevision = originalRevision
		static.coreInfoLatest = originalLatest
		static.lock.Unlock()
	})
	SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, "latest-wins test initial")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newRecordingCoreInfoStream(ctx)
	finished := make(chan error, 1)
	go func() {
		finished <- (&CoreService{}).CoreInfoListener(&hcommon.Empty{}, stream)
	}()
	select {
	case <-stream.sent:
	case <-time.After(time.Second):
		t.Fatal("CoreInfoListener did not send its initial snapshot")
	}

	stream.blockUpdateSends()
	SetCoreStatus(CoreStates_STARTING, MessageType_EMPTY, "queued transition")
	select {
	case <-stream.updateSendBlocked:
	case <-time.After(time.Second):
		t.Fatal("listener did not block while sending the queued transition")
	}
	SetCoreStatus(CoreStates_STOPPING, MessageType_EMPTY, "buffer saturation transition")
	SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, "latest terminal")
	static.lock.Lock()
	terminalRevision := static.coreInfoRevision
	static.lock.Unlock()
	stream.releaseBlockedUpdateSends()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case update := <-stream.sent:
			if update.CoreState == CoreStates_STOPPED && update.Message == "latest terminal" {
				goto deliveredLatestTerminal
			}
		case <-deadline.C:
			t.Fatal("listener did not reconcile a blocked/saturated stream to the latest terminal revision")
		}
	}

deliveredLatestTerminal:
	// A stale observable wake-up is deliberately not the source of truth. The
	// listener must re-read the current terminal revision instead of replaying it.
	static.coreInfoObserver.Emit(&coreInfoEvent{
		revision: terminalRevision - 1,
		info:     coreInfoResponse(CoreStates_STARTING, MessageType_EMPTY, "stale wake"),
	})
	select {
	case update := <-stream.sent:
		t.Fatalf("stale wake regressed delivered terminal state to %s: %s", update.CoreState, update.Message)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("CoreInfoListener returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CoreInfoListener did not exit after stream cancellation")
	}
}
