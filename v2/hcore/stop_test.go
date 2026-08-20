package hcore

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"
)

func TestWaitForInFlightStartWaitsForCompletionSignal(t *testing.T) {
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- waitForInFlightStart(done, time.Second)
	}()

	select {
	case err := <-result:
		t.Fatalf("waitForInFlightStart returned before completion signal: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(done)
	if err := <-result; err != nil {
		t.Fatalf("waitForInFlightStart after completion signal: %v", err)
	}
}

func TestWaitForInFlightStartReturnsTimeoutError(t *testing.T) {
	err := waitForInFlightStart(make(chan struct{}), 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("waitForInFlightStart timeout error = %v, want completion timeout", err)
	}
}

func TestCloseWithGraceReportsTimeoutAndSuccessfulClose(t *testing.T) {
	blockClose := make(chan struct{})
	timeoutErr := closeWithGrace(func() error {
		<-blockClose
		return nil
	}, "test blocking close", 10*time.Millisecond)
	if timeoutErr == nil || !strings.Contains(timeoutErr.Error(), "did not finish") {
		t.Fatalf("closeWithGrace timeout error = %v, want non-nil timeout", timeoutErr)
	}
	close(blockClose)

	if err := closeWithGrace(func() error { return nil }, "test successful close", time.Second); err != nil {
		t.Fatalf("closeWithGrace successful close: %v", err)
	}
}

func TestNormalizeAlreadyClosedErrorAcceptsOnlyClosedErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "direct", err: os.ErrClosed},
		{name: "wrapped", err: fmt.Errorf("close: %w", os.ErrClosed)},
		{name: "only joined", err: errors.Join(os.ErrClosed, fmt.Errorf("duplicate close: %w", os.ErrClosed))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := normalizeAlreadyClosedError(tt.err); err != nil {
				t.Fatalf("normalizeAlreadyClosedError(%v) = %v, want nil", tt.err, err)
			}
		})
	}

	realErr := errors.New("native close failed")
	mixed := errors.Join(os.ErrClosed, realErr)
	if err := normalizeAlreadyClosedError(mixed); !errors.Is(err, realErr) {
		t.Fatalf("mixed close error = %v, want real error %v to remain visible", err, realErr)
	}
}

func TestBoxCloseCoordinatorJoinsTimedOutCloseAndAllowsNextSuccessfulOperation(t *testing.T) {
	var coordinator boxCloseCoordinator
	box := new(libbox.BoxService)
	var closeCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	firstErr := coordinator.closeWithAction(box, "first", 10*time.Millisecond, func() error {
		closeCalls.Add(1)
		close(entered)
		<-release
		return nil
	})
	if firstErr == nil || !strings.Contains(firstErr.Error(), "did not finish") {
		t.Fatalf("first timed-out close = %v, want timeout", firstErr)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first close action did not start")
	}

	joined := make(chan error, 1)
	go func() {
		joined <- coordinator.closeWithAction(box, "join", time.Second, func() error {
			closeCalls.Add(1)
			return errors.New("second close action must not run")
		})
	}()
	select {
	case err := <-joined:
		t.Fatalf("joining close returned before original action completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-joined; err != nil {
		t.Fatalf("joining caller returned %v, want nil from original close", err)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close actions after timeout and join = %d, want exactly 1", got)
	}

	if err := coordinator.closeWithAction(box, "next", time.Second, func() error {
		closeCalls.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("new idempotent close after success = %v, want nil", err)
	}
	if got := closeCalls.Load(); got != 2 {
		t.Fatalf("close actions after successful completed operation = %d, want 2", got)
	}
}

func TestBoxCloseCoordinatorKeepsTerminalCloseErrorSticky(t *testing.T) {
	var coordinator boxCloseCoordinator
	box := new(libbox.BoxService)
	terminalErr := errors.New("native shutdown failed")
	var closeCalls atomic.Int32

	if err := coordinator.closeWithAction(box, "first", time.Second, func() error {
		closeCalls.Add(1)
		return terminalErr
	}); !errors.Is(err, terminalErr) {
		t.Fatalf("first terminal close = %v, want %v", err, terminalErr)
	}
	if err := coordinator.closeWithAction(box, "later", time.Second, func() error {
		closeCalls.Add(1)
		return nil
	}); !errors.Is(err, terminalErr) {
		t.Fatalf("sticky terminal close = %v, want %v", err, terminalErr)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("terminal close actions = %d, want one sticky action", got)
	}
}

func TestCoreStopCoordinatorCoalescesOverlappingCallersAndAllowsNextOperation(t *testing.T) {
	var coordinator coreStopCoordinator
	var closeCalls atomic.Int32
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	wantResponse := &CoreInfoResponse{CoreState: CoreStates_STOPPED}
	wantErr := errors.New("close failed")
	type stopResult struct {
		response *CoreInfoResponse
		err      error
	}

	stop := func() (*CoreInfoResponse, error) {
		closeCalls.Add(1)
		close(closeEntered)
		<-releaseClose
		return wantResponse, wantErr
	}
	firstResult := make(chan stopResult, 1)
	go func() {
		response, err := coordinator.run(stop)
		firstResult <- stopResult{response: response, err: err}
	}()
	select {
	case <-closeEntered:
	case <-time.After(time.Second):
		t.Fatal("first stop operation did not begin")
	}

	secondResult := make(chan stopResult, 1)
	go func() {
		response, err := coordinator.run(stop)
		secondResult <- stopResult{response: response, err: err}
	}()
	select {
	case result := <-secondResult:
		t.Fatalf("overlapping caller returned before shared close finished: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseClose)
	first := <-firstResult
	second := <-secondResult
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close operation calls = %d, want 1", got)
	}
	if first.response != wantResponse || second.response != wantResponse {
		t.Fatal("coalesced callers did not receive the shared response")
	}
	if !errors.Is(first.err, wantErr) || !errors.Is(second.err, wantErr) {
		t.Fatalf("coalesced caller errors = (%v, %v), want shared %v", first.err, second.err, wantErr)
	}

	nextResponse := &CoreInfoResponse{CoreState: CoreStates_STOPPED}
	next, err := coordinator.run(func() (*CoreInfoResponse, error) {
		closeCalls.Add(1)
		return nextResponse, nil
	})
	if err != nil || next != nextResponse {
		t.Fatalf("next stop operation = (%#v, %v), want (%#v, nil)", next, err, nextResponse)
	}
	if got := closeCalls.Load(); got != 2 {
		t.Fatalf("close operation calls after a completed operation = %d, want 2", got)
	}
}

func TestCoreStopCoordinatorWaitsAndTimesOut(t *testing.T) {
	var coordinator coreStopCoordinator
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		coordinator.run(func() (*CoreInfoResponse, error) {
			close(entered)
			<-release
			return nil, nil
		})
	}()
	<-entered

	if err := coordinator.wait(10 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "still finishing") {
		t.Fatalf("wait timeout error = %v, want in-flight stop timeout", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- coordinator.wait(time.Second) }()
	select {
	case err := <-waited:
		t.Fatalf("wait returned before stop completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-waited; err != nil {
		t.Fatalf("wait after stop completion: %v", err)
	}
	<-finished
}

func TestCoreStopCoordinatorFollowerTimesOutWithoutStartingSecondStop(t *testing.T) {
	var coordinator coreStopCoordinator
	var stopCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	leader := make(chan struct{})
	go func() {
		defer close(leader)
		_, _ = coordinator.runWithTimeout(func() (*CoreInfoResponse, error) {
			stopCalls.Add(1)
			close(entered)
			<-release
			return &CoreInfoResponse{CoreState: CoreStates_STOPPED}, nil
		}, time.Second)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("leader stop did not begin")
	}

	startedAt := time.Now()
	response, err := coordinator.runWithTimeout(func() (*CoreInfoResponse, error) {
		stopCalls.Add(1)
		return nil, errors.New("follower must not start a second stop")
	}, 10*time.Millisecond)
	if response != nil {
		t.Fatalf("timed-out follower response = %#v, want nil", response)
	}
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("timed-out follower error = %v, want bounded in-flight stop timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("follower waited too long: %s", elapsed)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop actions after timed-out follower = %d, want exactly 1", got)
	}

	close(release)
	select {
	case <-leader:
	case <-time.After(time.Second):
		t.Fatal("leader stop did not finish after release")
	}
}

func TestCoreStopCoordinatorExactFollowerWaitsPastLeaderBoundedTimeout(t *testing.T) {
	var coordinator coreStopCoordinator
	var stopCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	wantResponse := &CoreInfoResponse{CoreState: CoreStates_STOPPED}
	wantErr := errors.New("leader close result")
	type stopResult struct {
		response *CoreInfoResponse
		err      error
	}

	leaderResult := make(chan stopResult, 1)
	go func() {
		response, err := coordinator.runWithTimeout(func() (*CoreInfoResponse, error) {
			stopCalls.Add(1)
			close(entered)
			<-release
			return wantResponse, wantErr
		}, 10*time.Millisecond)
		leaderResult <- stopResult{response: response, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("leader stop did not begin")
	}
	select {
	case result := <-leaderResult:
		if result.response != nil || result.err == nil || !strings.Contains(result.err.Error(), "did not finish") {
			t.Fatalf("bounded leader result = (%#v, %v), want (nil, timeout)", result.response, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not return at its bounded timeout")
	}

	exactStarted := make(chan struct{})
	exactResult := make(chan stopResult, 1)
	go func() {
		close(exactStarted)
		response, err := coordinator.runExact(func() (*CoreInfoResponse, error) {
			stopCalls.Add(1)
			return nil, errors.New("exact follower must not start another stop")
		})
		exactResult <- stopResult{response: response, err: err}
	}()
	<-exactStarted
	select {
	case result := <-exactResult:
		t.Fatalf("exact follower returned before leader release: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}

	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop callbacks before leader release = %d, want exactly 1", got)
	}
	close(release)

	select {
	case result := <-exactResult:
		if result.response != wantResponse || !errors.Is(result.err, wantErr) {
			t.Fatalf("exact follower result = (%#v, %v), want shared (%#v, %v)", result.response, result.err, wantResponse, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("exact follower did not finish after leader release")
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop callbacks after exact join = %d, want exactly 1", got)
	}
}

func TestBoxCloseCoordinatorExactJoinWaitsPastBoundedCallerTimeout(t *testing.T) {
	var coordinator boxCloseCoordinator
	box := new(libbox.BoxService)
	var closeCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	wantErr := errors.New("original Box.Close result")

	boundedResult := make(chan error, 1)
	go func() {
		boundedResult <- coordinator.closeWithAction(box, "bounded", 10*time.Millisecond, func() error {
			closeCalls.Add(1)
			close(entered)
			<-release
			return wantErr
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bounded Box.Close action did not begin")
	}
	select {
	case err := <-boundedResult:
		if err == nil || !strings.Contains(err.Error(), "did not finish") {
			t.Fatalf("bounded Box.Close result = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded Box.Close caller did not return at timeout")
	}

	exactResult := make(chan error, 1)
	go func() {
		exactResult <- coordinator.closeExactWithAction(box, func() error {
			closeCalls.Add(1)
			return errors.New("exact join must not start another action")
		})
	}()
	select {
	case err := <-exactResult:
		t.Fatalf("exact Box.Close join returned before original action released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("Box.Close actions before release = %d, want exactly 1", got)
	}

	close(release)
	select {
	case err := <-exactResult:
		if !errors.Is(err, wantErr) {
			t.Fatalf("exact Box.Close result = %v, want shared %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("exact Box.Close join did not return after original action released")
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("Box.Close actions after exact join = %d, want exactly 1", got)
	}
}
