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

func (coordinator *coreStopCoordinator) currentOrStart(
	stop func() (*CoreInfoResponse, error),
) (*coreStopOperation, bool) {
	coordinator.lock.Lock()
	defer coordinator.lock.Unlock()
	if operation := coordinator.operation; operation != nil {
		return operation, false
	}

	operation := &coreStopOperation{done: make(chan struct{})}
	coordinator.operation = operation
	go func() {
		operation.response, operation.err = stop()
		coordinator.lock.Lock()
		if coordinator.operation == operation {
			coordinator.operation = nil
		}
		close(operation.done)
		coordinator.lock.Unlock()
	}()
	return operation, true
}

func (coordinator *coreStopCoordinator) run(
	stop func() (*CoreInfoResponse, error),
) (*CoreInfoResponse, error) {
	return coordinator.runWithTimeout(stop, stopCloseGracePeriod)
}

func (coordinator *coreStopCoordinator) runWithTimeout(
	stop func() (*CoreInfoResponse, error),
	timeout time.Duration,
) (*CoreInfoResponse, error) {
	operation, started := coordinator.currentOrStart(stop)
	if !started {
		Log(LogLevel_DEBUG, LogType_CORE, "joining in-flight core stop")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-operation.done:
		return operation.response, operation.err
	case <-timer.C:
		err := fmt.Errorf("in-flight core stop did not finish within %s", timeout)
		Log(LogLevel_WARNING, LogType_CORE, err.Error())
		return nil, err
	}
}

// runExact is reserved for the single process-wide Close operation. Public
// Stop callers receive a bounded result from runWithTimeout, but Close must
// keep owning and joining the original stop until it really finishes; returning
// early would let a later Close start a second teardown for the same Box.
func (coordinator *coreStopCoordinator) runExact(
	stop func() (*CoreInfoResponse, error),
) (*CoreInfoResponse, error) {
	operation, started := coordinator.currentOrStart(stop)
	if !started {
		Log(LogLevel_DEBUG, LogType_CORE, "joining exact in-flight core stop")
	}
	<-operation.done
	return operation.response, operation.err
}

func (coordinator *coreStopCoordinator) wait(timeout time.Duration) error {
	return coordinator.waitContext(context.Background(), timeout)
}

func (coordinator *coreStopCoordinator) waitContext(ctx context.Context, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	case <-ctx.Done():
		return ctx.Err()
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
	operation := coordinator.currentOrStart(box, closeAction)

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-operation.done:
		return coordinator.result(box, operation)
	case <-timer.C:
		err := fmt.Errorf("core close did not finish within %s during %s", gracePeriod, reason)
		Log(LogLevel_WARNING, LogType_CORE, err.Error())
		return err
	}
}

func (coordinator *boxCloseCoordinator) closeExactWithAction(
	box *libbox.BoxService,
	closeAction func() error,
) error {
	operation := coordinator.currentOrStart(box, closeAction)
	<-operation.done
	return coordinator.result(box, operation)
}

func (coordinator *boxCloseCoordinator) currentOrStart(
	box *libbox.BoxService,
	closeAction func() error,
) *boxCloseOperation {
	coordinator.lock.Lock()
	defer coordinator.lock.Unlock()
	if coordinator.operations == nil {
		coordinator.operations = make(map[*libbox.BoxService]*boxCloseOperation)
	}
	operation := coordinator.operations[box]
	if operation == nil {
		operation = &boxCloseOperation{done: make(chan struct{})}
		coordinator.operations[box] = operation
		go coordinator.run(operation, closeAction)
	}
	return operation
}

func (coordinator *boxCloseCoordinator) result(
	box *libbox.BoxService,
	operation *boxCloseOperation,
) error {
	coordinator.lock.Lock()
	defer coordinator.lock.Unlock()
	err := operation.err
	// A completed successful close needs no retained registry entry. A real
	// close failure remains sticky so a later caller cannot mistake Box.Close's
	// os.ErrClosed response for completed cleanup while native resources may
	// still be owned by the failed operation.
	if err == nil && coordinator.operations[box] == operation {
		delete(coordinator.operations, box)
	}
	return err
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

func stopForClose() (coreResponse *CoreInfoResponse, err error) {
	return stopOperations.runExact(stopOnce)
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
	if static.startInFlight &&
		(static.CoreState == CoreStates_STARTING || static.CoreState == CoreStates_STOPPING) {
		cancelStart := static.startCancel
		startDone := static.startDone
		setCoreStatusLocked(CoreStates_STOPPING, MessageType_EMPTY, "cancelling start")
		static.lock.Unlock()
		if cancelStart != nil {
			cancelStart()
		}
		startJoinStartedAt := time.Now()
		Log(LogLevel_INFO, LogType_CORE, "core stop stage=wait_start outcome=begin")
		// The stop operation itself is always exact. Public Stop callers have a
		// bounded wait in coreStopCoordinator.run, but the shared owner continues
		// until the admitted Start and any reattached Box cleanup are joinable.
		// Otherwise Mobile.Close could finish before a late Start creates its Box.
		if err := waitForInFlightStartExact(startDone); err != nil {
			Log(LogLevel_WARNING, LogType_CORE, fmt.Sprintf(
				"core stop stage=wait_start outcome=error duration_ms=%d",
				time.Since(startJoinStartedAt).Milliseconds(),
			))
			static.lock.Lock()
			response := coreInfoResponse(static.CoreState, MessageType_UNEXPECTED_ERROR, err.Error())
			if static.startInFlight || static.Box != nil {
				response = setCoreStatusLocked(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, err.Error())
			}
			static.lock.Unlock()
			return response, err
		}
		Log(LogLevel_INFO, LogType_CORE, fmt.Sprintf(
			"core stop stage=wait_start outcome=complete duration_ms=%d",
			time.Since(startJoinStartedAt).Milliseconds(),
		))

		static.lock.Lock()
		if static.Box == nil {
			response := setCoreStatusLocked(CoreStates_STOPPED, MessageType_EMPTY, "start cancelled")
			static.lock.Unlock()
			return response, nil
		}
		// The cancelled start reattached an instance whose cleanup did not
		// complete. Keep exact ownership below and close it synchronously.
		setCoreStatusLocked(CoreStates_STOPPING, MessageType_EMPTY, "finishing cancelled start cleanup")
	}
	if static.Box == nil {
		response := setCoreStatusLocked(CoreStates_STOPPED, MessageType_ALREADY_STOPPED, "")
		static.lock.Unlock()
		return response, nil
	}
	box := static.Box
	setCoreStatusLocked(CoreStates_STOPPING, MessageType_EMPTY, "")
	static.lock.Unlock()

	// The coordinator, not the public caller, owns the exact Box.Close. Public
	// Stop calls wait only for stopCloseGracePeriod, while Mobile.Close joins
	// this same operation until the native close action has actually returned.
	boxCloseStartedAt := time.Now()
	Log(LogLevel_INFO, LogType_CORE, "core stop stage=box_close outcome=begin")
	err = closeBoxExact(box)
	Log(LogLevel_INFO, LogType_CORE, fmt.Sprintf(
		"core stop stage=box_close outcome=complete duration_ms=%d error=%t",
		time.Since(boxCloseStartedAt).Milliseconds(), err != nil,
	))
	if err != nil {
		// Preserve the exact instance so a later bounded cleanup or process
		// teardown still owns every native resource, including the TUN fd.
		static.lock.Lock()
		response := coreInfoResponse(static.CoreState, MessageType_UNEXPECTED_ERROR, err.Error())
		if static.Box == box {
			response = setCoreStatusLocked(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, err.Error())
		}
		static.lock.Unlock()
		return response, err
	}
	// Listener availability is a next-generation admission concern. Waiting
	// without a deadline here can strand Mobile.Close after Box.Close has already
	// returned (and previously after CORE STOPPED was observed) when an unrelated
	// process owns the same port. StartService performs the bounded exact-address
	// bind probes before constructing the replacement Box.
	static.lock.Lock()
	if static.Box == box {
		static.Box = nil
		static.activeLocalListeners = nil
		response := setCoreStatusLocked(CoreStates_STOPPED, MessageType_EMPTY, "")
		static.lock.Unlock()
		return response, nil
	}
	response := coreInfoResponse(static.CoreState, MessageType_EMPTY, "")
	static.lock.Unlock()
	return response, nil
}

func waitForInFlightStop(timeout time.Duration) error {
	return stopOperations.wait(timeout)
}

func waitForInFlightStopContext(ctx context.Context, timeout time.Duration) error {
	return stopOperations.waitContext(ctx, timeout)
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

func waitForInFlightStartExact(done <-chan struct{}) error {
	if done == nil {
		return fmt.Errorf("in-flight start has no completion signal")
	}
	<-done
	return nil
}

func closeBoxWithGrace(box *libbox.BoxService, reason string, gracePeriod time.Duration) error {
	return boxCloseOperations.close(box, reason, gracePeriod)
}

func closeBoxExact(box *libbox.BoxService) error {
	return boxCloseOperations.closeExactWithAction(box, func() error {
		return common.Close(box)
	})
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
