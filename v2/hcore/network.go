package hcore

import "errors"

// ResetNetwork asks the running sing-box instance to refresh every
// network-bound outbound without stopping the TUN or the gRPC shell.
func ResetNetwork() error {
	return coreLifecycle.run(func() error {
		static.lock.Lock()
		defer static.lock.Unlock()
		box := static.Box
		state := static.CoreState

		if box == nil || state != CoreStates_STARTED {
			return errors.New("marten core is not started")
		}
		box.ResetNetwork()
		return nil
	})
}
