package hcore

import (
	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
)

func errorWrapper(state MessageType, err error) (*CoreInfoResponse, error) {
	Log(LogLevel_FATAL, LogType_CORE, err.Error())
	StopAndAlert(MessageType_UNEXPECTED_ERROR, err.Error())
	return SetCoreStatus(CoreStates_STOPPED, state, err.Error()), err
}

func StopAndAlert(msgType MessageType, message string) {
	box := detachBoxAndSetStatus(msgType, message)

	if box != nil {
		if err := closeBoxWithGrace(box, "stop and alert", stopCloseGracePeriod); err != nil {
			Log(LogLevel_WARNING, LogType_CORE, "failed to close core after error: ", err.Error())
		}
	}
}

func detachBoxAndSetStatus(msgType MessageType, message string) *libbox.BoxService {
	static.lock.Lock()
	defer static.lock.Unlock()

	box := static.Box
	static.Box = nil
	SetCoreStatus(CoreStates_STOPPED, msgType, message)
	return box
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

	// Close is the exact owner of native teardown. A concurrent public Stop may
	// time out for its caller, but this close operation keeps joining that same
	// stop so another Close cannot overtake it and touch the same Box twice.
	_, err := stopForClose()
	CloseGrpcServer(mode)
	if isBackgroundSetupMode(mode) {
		clearCorePlatformInterface()
	}

	return err
}

// func (s *CoreService) Status(ctx context.Context, empty *hcommon.Empty) (*CoreInfoResponse, error) {
// 	return Status()
// }
