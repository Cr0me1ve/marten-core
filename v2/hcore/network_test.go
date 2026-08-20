package hcore

import (
	"os"
	"strings"
	"testing"
)

func TestNetworkResetUsesBoundedDetachedBoxSnapshot(t *testing.T) {
	source, err := os.ReadFile("network.go")
	if err != nil {
		t.Fatalf("read network.go: %v", err)
	}
	body := string(source)

	for _, required := range []string{
		"const networkResetTimeout = 10 * time.Second",
		"static.lock.Lock()",
		"box := static.Box",
		"state := static.CoreState",
		"static.lock.Unlock()",
		"resetDone := make(chan error, 1)",
		"go func() {",
		"case <-timer.C:",
		"network reset did not finish within",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("network reset must contain %q", required)
		}
	}

	unlock := strings.Index(body, "static.lock.Unlock()")
	reset := strings.Index(body, "box.ResetNetwork()")
	if unlock < 0 || reset < unlock {
		t.Fatal("network reset must snapshot and unlock static state before invoking Box.ResetNetwork")
	}
}

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
