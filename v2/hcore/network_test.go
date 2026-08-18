package hcore

import (
	"strings"
	"testing"
)

func TestNetworkResetsWhenCoreIsNotStartedReturnTheSameFailClosedError(t *testing.T) {
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

	regularErr := ResetNetwork()
	strongerErr := ResetNetworkForRouteRecovery()
	if regularErr == nil || !strings.Contains(regularErr.Error(), "core is not started") {
		t.Fatalf("ResetNetwork error = %v, want core-not-started error", regularErr)
	}
	if strongerErr == nil || !strings.Contains(strongerErr.Error(), "core is not started") {
		t.Fatalf("ResetNetworkForRouteRecovery error = %v, want core-not-started error", strongerErr)
	}
	if strongerErr.Error() != regularErr.Error() {
		t.Fatalf("ResetNetworkForRouteRecovery error = %q, want same fail-closed error as ResetNetwork (%q)", strongerErr, regularErr)
	}
}

func TestForcedRouteRecoveryResetRejectsStoppedCore(t *testing.T) {
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

	err := ResetNetworkForRouteRecovery()
	if err == nil || !strings.Contains(err.Error(), "core is not started") {
		t.Fatalf("ResetNetworkForRouteRecovery error = %v, want core-not-started error", err)
	}
}
