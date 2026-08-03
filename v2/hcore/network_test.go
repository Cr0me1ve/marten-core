package hcore

import (
	"strings"
	"testing"
)

func TestResetNetworkWhenCoreIsNotStartedReturnsError(t *testing.T) {
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

	err := ResetNetwork()
	if err == nil || !strings.Contains(err.Error(), "core is not started") {
		t.Fatalf("ResetNetwork error = %v, want core-not-started error", err)
	}
}
