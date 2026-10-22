package hcore

import (
	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/sagernet/sing-box/log"
)

func errorWrapper(state MessageType, err error) (*CoreInfoResponse, error) {
	Log(LogLevel_FATAL, LogType_CORE, err.Error())
	static.lock.Lock()
	if static.Box == nil {
		response := setCoreStatusLocked(CoreStates_STOPPED, state, err.Error())
		static.lock.Unlock()
		return response, err
	}
	static.lock.Unlock()

	StopAndAlert(MessageType_UNEXPECTED_ERROR, err.Error())
	static.lock.Lock()
	response := setCoreStatusLocked(static.CoreState, state, err.Error())
	static.lock.Unlock()
	return response, err
}

func StopAndAlert(msgType MessageType, message string) {
	static.lock.Lock()
	if static.Box == nil && !static.startInFlight {
		setCoreStatusLocked(CoreStates_STOPPED, msgType, message)
		static.lock.Unlock()
		return
	}
	setCoreStatusLocked(CoreStates_STOPPING, msgType, message)
	static.lock.Unlock()

	// Never detach a Box before its exact close and fixed-listener barrier have
	// completed. The stop coordinator owns that cleanup asynchronously so an
	// error-reporting path cannot orphan a still-running native instance merely
	// because its bounded caller returned.
	_, started := stopOperations.currentOrStart(stopOnce)
	if !started {
		Log(LogLevel_DEBUG, LogType_CORE, "stop and alert joined existing exact cleanup")
	}
}

func Close(mode SetupMode) error {
	return coreLifecycle.run(func() error {
		return closeCore(mode)
	})
}

func closeCore(mode SetupMode) (closeErr error) {
	defer config.DeferPanicToError("close", func(err error) {
		closeErr = err
		Log(LogLevel_FATAL, LogType_CORE, err.Error())
		StopAndAlert(MessageType_UNEXPECTED_ERROR, err.Error())
	})
	log.Debug("[Service] Closing")
	Log(LogLevel_DEBUG, LogType_CORE, "native close phase=core_stop_begin")
	// Share the same lock used by StartService registration. Either Start is
	// registered first and the exact stop cancels/joins it, or Close seals the
	// generation first and that Start is rejected. There is no unowned gap.
	sealStartAdmissionForClose()

	// Close is the exact owner of native teardown. A concurrent public Stop may
	// time out for its caller, but this close operation keeps joining that same
	// stop so another Close cannot overtake it and touch the same Box twice.
	_, err := stopForClose()
	Log(LogLevel_DEBUG, LogType_CORE, "native close phase=core_stop_complete")
	CloseGrpcServer(mode)
	Log(LogLevel_DEBUG, LogType_CORE, "native close phase=grpc_stop_complete_or_handed_off")
	if isBackgroundSetupMode(mode) {
		clearCorePlatformInterface()
		Log(LogLevel_DEBUG, LogType_CORE, "native close phase=platform_interface_cleared")
	}

	Log(LogLevel_DEBUG, LogType_CORE, "native close phase=complete")
	return err
}

// func (s *CoreService) Status(ctx context.Context, empty *hcommon.Empty) (*CoreInfoResponse, error) {
// 	return Status()
// }
