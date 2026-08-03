package hcore

import "sync"

// lifecycleCoordinator protects process-global native core state from
// overlapping setup/close calls made through different platform entrypoints.
type lifecycleCoordinator struct {
	mutex sync.Mutex
}

func (coordinator *lifecycleCoordinator) run(operation func() error) error {
	coordinator.mutex.Lock()
	defer coordinator.mutex.Unlock()
	return operation()
}

var coreLifecycle lifecycleCoordinator
