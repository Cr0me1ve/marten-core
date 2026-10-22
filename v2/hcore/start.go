package hcore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/Cr0me1ve/marten-core/v2/db"
	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	service_manager "github.com/Cr0me1ve/marten-core/v2/service_manager"
	box "github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
)

const (
	startInFlightWaitPeriod      = 30 * time.Second
	startCleanupCloseGracePeriod = 12 * time.Second
	startPortAdmissionWaitPeriod = 12 * time.Second
	selectedRouteTimeoutDelay    = int32(1<<16 - 1)
)

func applyRuntimeDebugLogLevel(options *option.Options, debug bool) {
	if !debug || options == nil {
		return
	}
	if options.Log == nil {
		options.Log = &option.LogOptions{Level: "debug"}
		return
	}

	options.Log.Disabled = false
	switch strings.ToLower(strings.TrimSpace(options.Log.Level)) {
	case "", "trace", "debug":
	default:
		options.Log.Level = "debug"
	}
}

func hasTunInbound(options *option.Options) bool {
	if options == nil {
		return false
	}
	for _, inbound := range options.Inbounds {
		if inbound.Type == C.TypeTun {
			return true
		}
	}
	return false
}

func validateAndroidTunPlatform(
	goos string,
	options *option.Options,
	platformInterface libbox.PlatformInterface,
) error {
	if goos == "android" && hasTunInbound(options) && platformInterface == nil {
		return fmt.Errorf("android TUN platform interface unavailable")
	}
	return nil
}

func validateAndroidRuntimeSettings(
	goos string,
	runtimeConfig coreRuntimeConfigSnapshot,
) error {
	if goos == "android" && !runtimeConfig.settingsReady {
		return fmt.Errorf("android core settings unavailable")
	}
	return nil
}

func isUsableSelectedRoutePreflight(result *OutboundInfo) bool {
	return result != nil &&
		result.Tag != "" &&
		result.UrlTestTime != nil &&
		result.UrlTestDelay > 0 &&
		result.UrlTestDelay < selectedRouteTimeoutDelay
}

func preflightSelectedRouteBeforeAndroidTun(
	ctx context.Context,
	instance *libbox.BoxService,
) error {
	selectedType, err := selectedOutboundTypeOnBox(instance, config.OutboundSelectTag)
	if err != nil {
		return fmt.Errorf("selected route is unavailable before Android TUN: %w", err)
	}
	if selectedType != C.TypeVLESS {
		return nil
	}
	result, err := probeSelectedRouteOnBox(
		ctx,
		&UrlTestRequest{GroupTag: config.OutboundSelectTag},
		instance,
		func() bool {
			static.lock.Lock()
			defer static.lock.Unlock()
			return static.Box == instance && static.CoreState == CoreStates_STARTING
		},
	)
	if err != nil {
		return fmt.Errorf("selected route preflight failed before Android TUN: %w", err)
	}
	if !isUsableSelectedRoutePreflight(result) {
		return fmt.Errorf("selected route preflight was not usable before Android TUN")
	}
	Log(
		LogLevel_INFO,
		LogType_CORE,
		fmt.Sprintf(
			"selected route preflight verified before Android TUN type=%s delay=%dms",
			result.Type,
			result.UrlTestDelay,
		),
	)
	return nil
}

func logCoreStartState(options *option.Options) {
	logCoreStartStateWithRuntimeConfig(options, loadCoreRuntimeConfigSnapshot())
}

func logCoreStartStateWithRuntimeConfig(
	options *option.Options,
	runtimeConfig coreRuntimeConfigSnapshot,
) {
	if !RuntimeDebugEnabled() {
		return
	}
	Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf(
		"core start state debug=%t settings_ready=%t enable_tun=%t tun_inbound=%t platform_interface=%t",
		RuntimeDebugEnabled(),
		runtimeConfig.settingsReady,
		runtimeConfig.martenOptions != nil && runtimeConfig.martenOptions.EnableTun,
		hasTunInbound(options),
		runtimeConfig.platformInterface != nil,
	))
}

func (s *CoreService) Start(ctx context.Context, in *StartRequest) (*CoreInfoResponse, error) {
	return startService(ctx, in)
}

func Start(in *StartRequest) (*CoreInfoResponse, error) {
	return StartService(in)
}

func (s *CoreService) StartService(ctx context.Context, in *StartRequest) (*CoreInfoResponse, error) {
	return startService(ctx, in)
}

func saveLastStartRequest(in *StartRequest) error {
	if in.ConfigContent == "" && in.ConfigPath == "" {
		return nil
	}
	settings := db.GetTable[hcommon.AppSettings]()
	return settings.UpdateInsert(
		&hcommon.AppSettings{
			Id:    "lastStartRequestPath",
			Value: in.ConfigPath,
		},
		&hcommon.AppSettings{
			Id:    "lastStartRequestContent",
			Value: in.ConfigContent,
		},
		&hcommon.AppSettings{
			Id:    "lastStartRequestName",
			Value: in.ConfigName,
		},
	)
}

func loadLastStartRequestIfNeeded(in *StartRequest) (*StartRequest, error) {
	if in != nil && (in.ConfigContent != "" || in.ConfigPath != "") {
		return in, nil
	}
	settings := db.GetTable[hcommon.AppSettings]()
	lastPath, err := settings.Get("lastStartRequestPath")
	if err != nil {
		return nil, err
	}
	lastContent, err := settings.Get("lastStartRequestContent")
	if err != nil {
		return nil, err
	}

	lastName, err := settings.Get("lastStartRequestName")
	if err != nil {
		return nil, err
	}
	return &StartRequest{
		ConfigPath:    lastPath.Value.(string),
		ConfigContent: lastContent.Value.(string),
		ConfigName:    lastName.Value.(string),
	}, nil
}

func StartService(in *StartRequest) (coreResponse *CoreInfoResponse, err error) {
	return startService(context.Background(), in)
}

func startService(requestContext context.Context, in *StartRequest) (coreResponse *CoreInfoResponse, err error) {
	if requestContext == nil {
		requestContext = context.Background()
	}
	cleanupPlaintextRuntimeConfig()
	defer cleanupPlaintextRuntimeConfig()
	startInFlightOwned := false
	// Register the exact-start completion barrier before the panic publisher so
	// LIFO defer ordering keeps startDone closed until every panic side effect
	// (status publication and exact-stop ownership) has finished. Before start
	// admission this defer is intentionally a no-op.
	defer func() {
		if startInFlightOwned {
			finishStartInFlight()
		}
	}()
	defer config.DeferPanicToError("startmobile", func(recovered_err error) {
		coreResponse, err = errorWrapper(MessageType_UNEXPECTED_ERROR, recovered_err)
	})
	if err := waitForInFlightStopContext(requestContext, startInFlightWaitPeriod); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return currentStartRejectionResponse(err), err
		}
		return currentStartResponse(MessageType_START_SERVICE, err.Error()), err
	}
	if err := waitForPreviousStartToFinishContext(requestContext); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return currentStartRejectionResponse(err), err
		}
		return currentStartResponse(MessageType_START_SERVICE, err.Error()), err
	}
	static.lock.Lock()
	if requestErr := requestContext.Err(); requestErr != nil {
		state := static.CoreState
		static.lock.Unlock()
		return &CoreInfoResponse{
			CoreState:   state,
			MessageType: MessageType_START_SERVICE,
			Message:     requestErr.Error(),
		}, requestErr
	}
	if static.startAdmissionClosed {
		state := static.CoreState
		static.lock.Unlock()
		err := fmt.Errorf("core start admission is sealed until setup completes")
		return startResponse(state, MessageType_START_SERVICE, err.Error()), err
	}
	if static.CoreState != CoreStates_STOPPED {
		state := static.CoreState
		static.lock.Unlock()
		return startResponse(state, MessageType_ALREADY_STARTED, "instance already started"), nil
	}
	resetTurncoatRouteEvidence()
	startCtx, cancelStart := context.WithCancel(context.Background())
	markStartInFlightLocked(cancelStart)
	startInFlightOwned = true
	setCoreStatusLocked(CoreStates_STARTING, MessageType_EMPTY, "")
	static.lock.Unlock()

	in, err = loadLastStartRequestIfNeeded(in)
	if err != nil {
		return errorWrapper(MessageType_ERROR_BUILDING_CONFIG, err)
	}

	static.lock.Lock()
	if static.CoreState != CoreStates_STARTING {
		response := startResponse(static.CoreState, MessageType_ALREADY_STOPPED, "start cancelled")
		static.lock.Unlock()
		return response, nil
	}
	static.previousStartRequest = in
	static.lock.Unlock()

	runtimeConfig := loadCoreRuntimeConfigSnapshot()
	if err := validateAndroidRuntimeSettings(runtime.GOOS, runtimeConfig); err != nil {
		return errorWrapper(MessageType_ERROR_BUILDING_CONFIG, err)
	}
	options, err := buildConfigWithRuntimeConfig(in, runtimeConfig)
	if err != nil {
		return errorWrapper(MessageType_ERROR_BUILDING_CONFIG, err)
	}
	applyRuntimeDebugLogLevel(options, static.debug.Load())
	logCoreStartStateWithRuntimeConfig(options, runtimeConfig)
	if err := validateAndroidTunPlatform(runtime.GOOS, options, runtimeConfig.platformInterface); err != nil {
		return errorWrapper(MessageType_CREATE_SERVICE, err)
	}
	saveLastStartRequest(in)

	Log(LogLevel_DEBUG, LogType_CORE, "Main Service pre start")
	if err := service_manager.OnMainServicePreStart(options); err != nil {
		return errorWrapper(MessageType_ERROR_EXTENSION, err)
	}
	localListeners := localListenEndpoints(options)
	if err := waitForLocalListeners(
		startCtx,
		localListeners,
		"start admission",
		startPortAdmissionWaitPeriod,
	); err != nil {
		if errors.Is(err, context.Canceled) {
			return currentStartResponse(MessageType_ALREADY_STOPPED, "start cancelled"), nil
		}
		return errorWrapper(MessageType_START_SERVICE, err)
	}
	bopts := box.Options{
		Context:           startCtx,
		Options:           *options,
		PlatformLogWriter: &LogInterface{},
	}
	if runtimeConfig.platformInterface != nil {
		bopts.PlatformInterface = libbox.WrapPlatformInterface(runtimeConfig.platformInterface)
	}
	libbox.SetMemoryLimit(!in.DisableMemoryLimit)
	instance, err := libbox.NewHService(bopts)
	if err != nil {
		return errorWrapper(MessageType_CREATE_SERVICE, err)
	}
	// for i := 0; i < 10; i++ {
	// 	if hutils.IsPortInUse(options.Inbounds[0].SocksOptions.ListenPort) {
	// 		<-time.After(100 * time.Millisecond)
	// 	}
	// }
	Log(LogLevel_DEBUG, LogType_CORE, "Stating Service with delay ?", in.DelayStart)
	if in.DelayStart {
		<-time.After(1000 * time.Millisecond)
	}

	instance.GetInstance().AddPostService("martenMainServiceManager", &martenMainServiceManager{})

	// if err := startCommandServer(instance); err != nil {
	// 	return errorWrapper(MessageType_START_COMMAND_SERVER, err)
	// }

	static.lock.Lock()
	if static.CoreState != CoreStates_STARTING {
		static.lock.Unlock()
		if !closeStartInstance(instance, "cancelled start") {
			reattachLeakedStartInstance(instance, localListeners, "cancelled start cleanup incomplete")
			cleanupErr := fmt.Errorf("cancelled start cleanup did not finish")
			return publishStartCleanupFailure(instance, cleanupErr), cleanupErr
		}
		return currentStartResponse(MessageType_ALREADY_STOPPED, "start cancelled"), nil
	}
	static.Box = instance
	static.activeLocalListeners = cloneLocalListenEndpoints(localListeners)
	static.lock.Unlock()

	var instanceStartErr error
	if runtime.GOOS == "android" && hasTunInbound(options) {
		preStartStartedAt := time.Now()
		Log(LogLevel_INFO, LogType_CORE, "core start stage=pre_start outcome=begin")
		instanceStartErr = instance.PreStart()
		Log(LogLevel_INFO, LogType_CORE, fmt.Sprintf(
			"core start stage=pre_start outcome=complete duration_ms=%d error=%t",
			time.Since(preStartStartedAt).Milliseconds(), instanceStartErr != nil,
		))
		if instanceStartErr == nil {
			preflightStartedAt := time.Now()
			Log(LogLevel_INFO, LogType_CORE, "core start stage=route_preflight outcome=begin")
			instanceStartErr = preflightSelectedRouteBeforeAndroidTun(startCtx, instance)
			Log(LogLevel_INFO, LogType_CORE, fmt.Sprintf(
				"core start stage=route_preflight outcome=complete duration_ms=%d error=%t",
				time.Since(preflightStartedAt).Milliseconds(), instanceStartErr != nil,
			))
		}
		if instanceStartErr == nil {
			Log(LogLevel_INFO, LogType_CORE, "core start stage=open_inbounds outcome=begin")
			instanceStartErr = instance.StartAfterPreStart()
			Log(LogLevel_INFO, LogType_CORE, fmt.Sprintf(
				"core start stage=open_inbounds outcome=complete error=%t",
				instanceStartErr != nil,
			))
		}
	} else {
		instanceStartErr = instance.Start()
	}
	if instanceStartErr != nil {
		static.lock.Lock()
		cancelled := static.CoreState != CoreStates_STARTING || static.Box != instance
		if static.Box == instance {
			static.Box = nil
			static.activeLocalListeners = nil
		}
		static.lock.Unlock()
		closed := closeStartInstance(instance, "failed start")
		if !closed {
			reattachLeakedStartInstance(instance, localListeners, "failed start cleanup incomplete")
			cleanupErr := fmt.Errorf("failed start cleanup did not finish: %w", instanceStartErr)
			return publishStartCleanupFailure(instance, cleanupErr), cleanupErr
		}
		if cancelled {
			return currentStartResponse(MessageType_ALREADY_STOPPED, "start cancelled"), nil
		}
		return errorWrapper(MessageType_START_SERVICE, instanceStartErr)
	}

	static.lock.Lock()
	if static.Box == instance && static.CoreState == CoreStates_STARTING {
		response := setCoreStatusLocked(CoreStates_STARTED, MessageType_EMPTY, "")
		static.lock.Unlock()
		return response, nil
	}
	owned := static.Box == instance
	if !owned {
		static.lock.Unlock()
		if !closeStartInstance(instance, "stale start") {
			reattachLeakedStartInstance(instance, localListeners, "stale start cleanup incomplete")
			cleanupErr := fmt.Errorf("stale start cleanup did not finish")
			return publishStartCleanupFailure(instance, cleanupErr), cleanupErr
		}
		return currentStartResponse(MessageType_ALREADY_STOPPED, "start cancelled"), nil
	}
	response := setCoreStatusLocked(
		CoreStates_STOPPING,
		MessageType_ALREADY_STOPPED,
		"start cancellation awaiting exact close",
	)
	static.lock.Unlock()

	// A late CancelStart may win after Box.Start returned successfully. Keep the
	// attached instance authoritative and STOPPING: the Stop/Mobile.Close that
	// requested cancellation is the one exact owner of Box.Close plus the fixed
	// listener availability barrier.
	return response, nil
}

func currentStartRejectionResponse(err error) *CoreInfoResponse {
	return currentStartResponse(MessageType_START_SERVICE, err.Error())
}

// Rejected or stale Start calls are observations, not lifecycle owners. Build
// their RPC response from the current state without publishing through
// SetCoreStatus: a concurrent exact Stop/Close may have advanced the generation
// after the caller last inspected it, and the rejected Start must never write
// STARTED/STOPPING/STOPPED back over that authoritative result.
func currentStartResponse(messageType MessageType, message string) *CoreInfoResponse {
	static.lock.Lock()
	state := static.CoreState
	static.lock.Unlock()
	return startResponse(state, messageType, message)
}

func startResponse(state CoreStates, messageType MessageType, message string) *CoreInfoResponse {
	return &CoreInfoResponse{
		CoreState:   state,
		MessageType: messageType,
		Message:     message,
	}
}

func cleanupPlaintextRuntimeConfig() {
	if sWorkingPath == "" {
		return
	}
	path := filepath.Join(sWorkingPath, "data/current-config.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		Log(LogLevel_WARNING, LogType_CORE, "failed to remove legacy plaintext runtime config: ", err.Error())
	}
}

func waitForPreviousStartToFinish() error {
	return waitForPreviousStartToFinishContext(context.Background())
}

func waitForPreviousStartToFinishContext(ctx context.Context) error {
	deadline := time.NewTimer(startInFlightWaitPeriod)
	defer deadline.Stop()

	for {
		static.lock.Lock()
		if !static.startInFlight {
			static.lock.Unlock()
			return nil
		}
		done := static.startDone
		static.lock.Unlock()

		if done == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("previous start is still finishing")
		}
	}
}

func markStartInFlightLocked(cancel context.CancelFunc) {
	static.startInFlight = true
	static.startDone = make(chan struct{})
	static.startCancel = cancel
}

func finishStartInFlight() {
	static.lock.Lock()
	done := static.startDone
	static.startInFlight = false
	static.startDone = nil
	static.startCancel = nil
	static.lock.Unlock()
	if done != nil {
		close(done)
	}
}

// sealStartAdmissionForClose shares static.lock with StartService's in-flight
// registration. If Start wins the lock, Close sees and cancels that exact
// operation. If Close wins, Start cannot appear behind a completed stop.
func sealStartAdmissionForClose() {
	static.lock.Lock()
	static.startAdmissionClosed = true
	cancelStart := static.startCancel
	if static.startInFlight || static.Box != nil {
		setCoreStatusLocked(CoreStates_STOPPING, MessageType_EMPTY, "closing core generation")
	}
	static.lock.Unlock()
	if cancelStart != nil {
		cancelStart()
	}
}

func openStartAdmissionAfterSetup() {
	static.lock.Lock()
	static.startAdmissionClosed = false
	static.lock.Unlock()
	Log(LogLevel_DEBUG, LogType_CORE, "core start admission opened after setup")
}

// CancelStart interrupts a native Start that is waiting for local listener
// admission. It deliberately does not close the Box: Mobile.Close/Stop remains
// the single authoritative cleanup owner and joins the same start afterward.
func CancelStart() bool {
	static.lock.Lock()
	if !static.startInFlight ||
		static.startCancel == nil ||
		(static.CoreState != CoreStates_STARTING && static.CoreState != CoreStates_STOPPING) {
		static.lock.Unlock()
		return false
	}
	cancelStart := static.startCancel
	if static.CoreState == CoreStates_STARTING {
		setCoreStatusLocked(CoreStates_STOPPING, MessageType_EMPTY, "cancelling start")
	}
	static.lock.Unlock()
	cancelStart()
	return true
}

func closeStartInstance(instance *libbox.BoxService, reason string) bool {
	err := closeBoxWithGrace(instance, reason, startCleanupCloseGracePeriod)
	if err != nil {
		Log(LogLevel_WARNING, LogType_CORE, fmt.Sprintf("core close returned error during %s: %v", reason, err))
	}
	return err == nil
}

func reattachLeakedStartInstance(
	instance *libbox.BoxService,
	localListeners []localListenEndpoint,
	reason string,
) {
	static.lock.Lock()
	if static.Box == nil {
		static.Box = instance
		static.activeLocalListeners = cloneLocalListenEndpoints(localListeners)
	}
	static.lock.Unlock()
	Log(LogLevel_WARNING, LogType_CORE, reason)
}

// A failed cleanup may publish STOPPING only while the leaked instance is
// still the exact Box owned by this Start. If another lifecycle owner has
// already advanced the generation, return a pure snapshot instead of
// overwriting its state.
func publishStartCleanupFailure(instance *libbox.BoxService, cleanupErr error) *CoreInfoResponse {
	static.lock.Lock()
	defer static.lock.Unlock()
	if static.Box == instance {
		return setCoreStatusLocked(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, cleanupErr.Error())
	}
	return coreInfoResponse(static.CoreState, MessageType_UNEXPECTED_ERROR, cleanupErr.Error())
}
