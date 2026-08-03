package hcore

import (
	"errors"
	"testing"
	"time"
)

func TestLifecycleCoordinatorSerializesConcurrentOperations(t *testing.T) {
	var coordinator lifecycleCoordinator
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	go func() {
		firstDone <- coordinator.run(func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	go func() {
		secondDone <- coordinator.run(func() error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second lifecycle operation entered while first operation still held the gate")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first operation returned error: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second operation returned error: %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second lifecycle operation did not enter after the first released the gate")
	}
}

func TestLifecycleCoordinatorReleasesGateAfterOperationError(t *testing.T) {
	var coordinator lifecycleCoordinator
	want := errors.New("setup failed")

	if err := coordinator.run(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("failed operation error = %v, want %v", err, want)
	}

	ranNextOperation := false
	if err := coordinator.run(func() error {
		ranNextOperation = true
		return nil
	}); err != nil {
		t.Fatalf("operation after error returned error: %v", err)
	}
	if !ranNextOperation {
		t.Fatal("operation after error did not acquire lifecycle gate")
	}
}
