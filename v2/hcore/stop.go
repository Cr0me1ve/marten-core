package hcore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing/common"
)

// libbox owns the protocol-specific shutdown and already has a bounded fatal
// stop timeout.  Waiting past that boundary keeps one authoritative close in
// flight instead of starting a second Close on the same Box after two seconds.
const stopCloseGracePeriod = 12 * time.Second

type coreStopOperation struct {
	done     chan struct{}
	response *CoreInfoResponse
	err      error
}

type coreStopCoordinator struct {
	lock      sync.Mutex
	operation *coreStopOperation
}

func (coordinator *coreStopCoordinator) run(
	stop func() (*CoreInfoResponse, error),
) (*CoreInfoResponse, error) {
	coordinator.lock.Lock()
	if operation := coordinator.operation; operation != nil {
		coordinator.lock.Unlock()
		Log(LogLevel_DEBUG, LogType_CORE, "joining in-flight core stop")
		<-operation.done
		return operation.response, operation.err
	}
	operation := &coreStopOperation{done: make(chan struct{})}
	coordinator.operation = operation
	coordinator.lock.Unlock()

	operation.response, operation.err = stop()
	coordinator.lock.Lock()
	if coordinator.operation == operation {
		coordinator.operation = nil
	}
	close(operation.done)
	coordinator.lock.Unlock()
	return operation.response, operation.err
}

func (coordinator *coreStopCoordinator) wait(timeout time.Duration) error {
	coordinator.lock.Lock()
	operation := coordinator.operation
	coordinator.lock.Unlock()
	if operation == nil {
		return nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-operation.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("previous stop is still finishing")
	}
}

var stopOperations coreStopCoordinator

type boxCloseOperation struct {
	done chan struct{}
	err  error
}

// boxCloseCoordinator gives every Box instance exactly one close operation.
// A caller may time out while the close continues, but a later Stop/Close joins
// that same operation.  This is important on Android where Mobile.stop and
// Mobile.close are deliberately separate lifecycle barriers.
type boxCloseCoordinator struct {
	lock       sync.Mutex
	operations map[*libbox.BoxService]*boxCloseOperation
}

func (coordinator *boxCloseCoordinator) close(
	box *libbox.BoxService,
	reason string,
	gracePeriod time.Duration,
) error {
	return coordinator.closeWithAction(box, reason, gracePeriod, func() error {
		return common.Close(box)
	})
}

func (coordinator *boxCloseCoordinator) closeWithAction(
	box *libbox.BoxService,
	reason string,
	gracePeriod time.Duration,
	closeAction func() error,
) error {
	coordinator.lock.Lock()
	if coordinator.operations == nil {
		coordinator.operations = make(map[*libbox.BoxService]*boxCloseOperation)
	}
	operation := coordinator.operations[box]
	if operation == nil {
		operation = &boxCloseOperation{done: make(chan struct{})}
		coordinator.operations[box] = operation
		go coordinator.run(operation, closeAction)
	}
	coordinator.lock.Unlock()

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-operation.done:
		coordinator.lock.Lock()
		err := operation.err
		// A completed successful close needs no retained registry entry.  A
		// real close failure remains sticky so a later caller cannot mistake
		// Box.Close's os.ErrClosed response for completed cleanup while an
		// internal shutdown goroutine may still own resources.
		if err == nil && coordinator.operations[box] == operation {
			delete(coordinator.operations, box)
		}
		coordinator.lock.Unlock()
		return err
	case <-timer.C:
		err := fmt.Errorf("core close did not finish within %s during %s", gracePeriod, reason)
		Log(LogLevel_WARNING, LogType_CORE, err.Error())
		return err
	}
}

func (coordinator *boxCloseCoordinator) run(operation *boxCloseOperation, closeAction func() error) {
	var closeErr error
	defer func() {
		coordinator.lock.Lock()
		operation.err = normalizeAlreadyClosedError(closeErr)
		close(operation.done)
		coordinator.lock.Unlock()
	}()
	defer config.DeferPanicToError("stop-close", func(recoveredErr error) {
		closeErr = recoveredErr
	})
	closeErr = closeAction()
}

func normalizeAlreadyClosedError(err error) error {
	if isOnlyAlreadyClosedError(err) {
		Log(LogLevel_DEBUG, LogType_CORE, "core Box was already closed; accepting idempotent cleanup")
		return nil
	}
	return err
}

func isOnlyAlreadyClosedError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isOnlyAlreadyClosedError(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if child != nil {
			return isOnlyAlreadyClosedError(child)
		}
	}
	return errors.Is(err, os.ErrClosed)
}

var boxCloseOperations boxCloseCoordinator

func (s *CoreService) Stop(ctx context.Context, empty *hcommon.Empty) (*CoreInfoResponse, error) {
	return Stop()
}

func Stop() (coreResponse *CoreInfoResponse, err error) {
	return stopOperations.run(stopOnce)
}

func stopOnce() (coreResponse *CoreInfoResponse, err error) {
	defer cleanupPlaintextRuntimeConfig()
	defer config.DeferPanicToError("stop", func(recovered_err error) {
		coreResponse, err = errorWrapper(MessageType_UNEXPECTED_ERROR, recovered_err)
	})

	// if static.CoreState != CoreStates_STARTED {
	// 	return errorWrapper(MessageType_INSTANCE_NOT_STARTED, fmt.Errorf("instance not started"))
	// }
	// if static.Box == nil {
	// 	return errorWrapper(MessageType_INSTANCE_NOT_FOUND, fmt.Errorf("instance not found"))
	// }
	static.lock.Lock()
	if static.CoreState == CoreStates_STARTING && static.startInFlight {
		cancelStart := static.startCancel
		startDone := static.startDone
		SetCoreStatus(CoreStates_STOPPING, MessageType_EMPTY, "cancelling start")
		static.lock.Unlock()
		if cancelStart != nil {
			cancelStart()
		}
		if err := waitForInFlightStart(startDone, startInFlightWaitPeriod); err != nil {
			return SetCoreStatus(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, err.Error()), err
		}

		static.lock.Lock()
		if static.Box == nil {
			SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, "start cancelled")
			static.lock.Unlock()
			return SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, "start cancelled"), nil
		}
		// The cancelled start reattached an instance whose cleanup did not
		// complete. Keep exact ownership below and close it synchronously.
		SetCoreStatus(CoreStates_STOPPING, MessageType_EMPTY, "finishing cancelled start cleanup")
	}
	if static.Box == nil {
		static.lock.Unlock()
		return SetCoreStatus(CoreStates_STOPPED, MessageType_ALREADY_STOPPED, ""), nil
	}
	box := static.Box
	SetCoreStatus(CoreStates_STOPPING, MessageType_EMPTY, "")
	static.lock.Unlock()

	err = closeBoxWithGrace(box, "stop", stopCloseGracePeriod)
	if err != nil {
		// Preserve the exact instance so a later bounded cleanup or process
		// teardown still owns every native resource, including the TUN fd.
		return SetCoreStatus(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, err.Error()), err
	}

	static.lock.Lock()
	if static.Box == box {
		static.Box = nil
	}
	SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, "")
	static.lock.Unlock()
	return SetCoreStatus(CoreStates_STOPPED, MessageType_EMPTY, ""), nil
}

func waitForInFlightStop(timeout time.Duration) error {
	return stopOperations.wait(timeout)
}

func waitForInFlightStart(done <-chan struct{}, timeout time.Duration) error {
	if done == nil {
		return fmt.Errorf("in-flight start has no completion signal")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("in-flight start cleanup did not finish within %s", timeout)
	}
}

func closeBoxWithGrace(box *libbox.BoxService, reason string, gracePeriod time.Duration) error {
	return boxCloseOperations.close(box, reason, gracePeriod)
}

func closeWithGrace(closeAction func() error, reason string, gracePeriod time.Duration) error {
	closeDone := make(chan error, 1)
	go func() {
		defer config.DeferPanicToError("stop-close", func(recoveredErr error) {
			closeDone <- recoveredErr
		})
		closeDone <- closeAction()
	}()

	select {
	case err := <-closeDone:
		return err
	case <-time.After(gracePeriod):
		err := fmt.Errorf("core close did not finish within %s during %s", gracePeriod, reason)
		Log(LogLevel_WARNING, LogType_CORE, err.Error())
		return err
	}
}
