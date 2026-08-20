package hcore

import (
	"errors"
	"fmt"
	"time"
)

const networkResetTimeout = 10 * time.Second

// ResetNetwork asks the running sing-box instance to refresh every
// network-bound outbound without stopping the TUN or the gRPC shell.
func ResetNetwork() error {
	return resetNetwork(false)
}

// ResetNetworkForRouteRecovery keeps the running core and TUN but asks
// transports to perform the stronger reset required after an authoritative
// end-to-end route proof has failed. In particular, TURNcoat must not retain a
// carrier merely because its relay-only probe still succeeds.
func ResetNetworkForRouteRecovery() error {
	return resetNetwork(true)
}

func resetNetwork(routeRecovery bool) error {
	// A network reset operates on the exact Box generation observed here, but
	// it is not a lifecycle transition. In particular, do not retain either the
	// process-global state lock or coreLifecycle while invoking transport reset
	// callbacks: a stuck transport must never prevent Stop from detaching and
	// closing this Box. libbox/router shutdown already permits an in-flight
	// reset to finish against the old instance while the current generation is
	// stopped or replaced.
	static.lock.Lock()
	box := static.Box
	state := static.CoreState
	static.lock.Unlock()

	if box == nil || state != CoreStates_STARTED {
		return errors.New("marten core is not started")
	}

	resetDone := make(chan error, 1)
	go func() {
		var resetErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				resetErr = fmt.Errorf("marten core network reset panicked: %v", recovered)
			}
			resetDone <- resetErr
		}()
		if routeRecovery {
			box.ResetNetworkForRouteRecovery()
		} else {
			box.ResetNetwork()
		}
	}()

	timer := time.NewTimer(networkResetTimeout)
	defer timer.Stop()
	select {
	case err := <-resetDone:
		return err
	case <-timer.C:
		return fmt.Errorf("marten core network reset did not finish within %s", networkResetTimeout)
	}
}
