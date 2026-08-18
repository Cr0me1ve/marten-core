package hcore

import "errors"

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
	return coreLifecycle.run(func() error {
		static.lock.Lock()
		defer static.lock.Unlock()
		box := static.Box
		state := static.CoreState

		if box == nil || state != CoreStates_STARTED {
			return errors.New("marten core is not started")
		}
		if routeRecovery {
			box.ResetNetworkForRouteRecovery()
		} else {
			box.ResetNetwork()
		}
		return nil
	})
}
