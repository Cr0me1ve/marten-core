package hcore

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	startCleanupPortWaitPeriod   = 12 * time.Second
	startCleanupPortPollPeriod   = 200 * time.Millisecond
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
	return Start(in)
}

func Start(in *StartRequest) (*CoreInfoResponse, error) {
	return StartService(in)
}

func (s *CoreService) StartService(ctx context.Context, in *StartRequest) (*CoreInfoResponse, error) {
	return StartService(in)
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
	cleanupPlaintextRuntimeConfig()
	defer cleanupPlaintextRuntimeConfig()
	defer config.DeferPanicToError("startmobile", func(recovered_err error) {
		coreResponse, err = errorWrapper(MessageType_UNEXPECTED_ERROR, recovered_err)
	})
	if err := waitForInFlightStop(startInFlightWaitPeriod); err != nil {
		return SetCoreStatus(CoreStates_STOPPING, MessageType_START_SERVICE, err.Error()), err
	}
	if err := waitForPreviousStartToFinish(); err != nil {
		return SetCoreStatus(CoreStates_STOPPED, MessageType_START_SERVICE, err.Error()), err
	}
	static.lock.Lock()
	if static.CoreState != CoreStates_STOPPED {
		state := static.CoreState
		static.lock.Unlock()
		return SetCoreStatus(state, MessageType_ALREADY_STARTED, "instance already started"), nil
	}
	resetTurncoatRouteEvidence()
	startCtx, cancelStart := context.WithCancel(context.Background())
	markStartInFlightLocked(cancelStart)
	SetCoreStatus(CoreStates_STARTING, MessageType_EMPTY, "")
	static.lock.Unlock()
	defer finishStartInFlight()

	in, err = loadLastStartRequestIfNeeded(in)
	if err != nil {
		return errorWrapper(MessageType_ERROR_BUILDING_CONFIG, err)
	}

	static.lock.Lock()
	if static.CoreState != CoreStates_STARTING {
		static.lock.Unlock()
		return SetCoreStatus(CoreStates_STOPPED, MessageType_ALREADY_STOPPED, "start cancelled"), nil
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
		if !closeStartInstance(instance, options, "cancelled start") {
			reattachLeakedStartInstance(instance, "cancelled start cleanup incomplete")
			cleanupErr := fmt.Errorf("cancelled start cleanup did not finish")
			return SetCoreStatus(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, cleanupErr.Error()), cleanupErr
		}
		return SetCoreStatus(CoreStates_STOPPED, MessageType_ALREADY_STOPPED, "start cancelled"), nil
	}
	static.Box = instance
	static.lock.Unlock()

	if err := instance.Start(); err != nil {
		static.lock.Lock()
		cancelled := static.CoreState != CoreStates_STARTING || static.Box != instance
		if static.Box == instance {
			static.Box = nil
		}
		static.lock.Unlock()
		closed := closeStartInstance(instance, options, "failed start")
		if !closed {
			reattachLeakedStartInstance(instance, "failed start cleanup incomplete")
			cleanupErr := fmt.Errorf("failed start cleanup did not finish: %w", err)
			return SetCoreStatus(CoreStates_STOPPING, MessageType_UNEXPECTED_ERROR, cleanupErr.Error()), cleanupErr
		}
		if cancelled {
			return SetCoreStatus(CoreStates_STOPPED, MessageType_ALREADY_STOPPED, "start cancelled"), nil
		}
		return errorWrapper(MessageType_START_SERVICE, err)
	}

	static.lock.Lock()
	if static.Box != instance {
		static.lock.Unlock()
		if !closeStartInstance(instance, options, "stale start") {
			reattachLeakedStartInstance(instance, "stale start still owns local ports")
			return SetCoreStatus(CoreStates_STARTED, MessageType_EMPTY, "recovered stale start"), nil
		}
		return SetCoreStatus(CoreStates_STOPPED, MessageType_ALREADY_STOPPED, "start cancelled"), nil
	}
	static.lock.Unlock()

	return SetCoreStatus(CoreStates_STARTED, MessageType_EMPTY, ""), nil
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

func closeStartInstance(instance *libbox.BoxService, options *option.Options, reason string) bool {
	err := closeBoxWithGrace(instance, reason, startCleanupCloseGracePeriod)
	if err != nil {
		Log(LogLevel_WARNING, LogType_CORE, fmt.Sprintf("core close returned error during %s: %v", reason, err))
	}
	portsReleased := waitForStartPortsReleased(options, reason)
	return err == nil && portsReleased
}

func reattachLeakedStartInstance(instance *libbox.BoxService, reason string) {
	static.lock.Lock()
	if static.Box == nil {
		static.Box = instance
	}
	static.lock.Unlock()
	Log(LogLevel_WARNING, LogType_CORE, reason)
}

func waitForStartPortsReleased(options *option.Options, reason string) bool {
	ports := startLocalTCPPorts(options)
	if len(ports) == 0 {
		return true
	}

	deadline := time.After(startCleanupPortWaitPeriod)
	ticker := time.NewTicker(startCleanupPortPollPeriod)
	defer ticker.Stop()

	for {
		busy := busyLocalTCPPorts(ports)
		if len(busy) == 0 {
			return true
		}

		select {
		case <-deadline:
			Log(
				LogLevel_WARNING,
				LogType_CORE,
				fmt.Sprintf("local ports still busy after %s during %s: %v", startCleanupPortWaitPeriod, reason, busy),
			)
			return false
		case <-ticker.C:
		}
	}
}

func startLocalTCPPorts(options *option.Options) []uint16 {
	seen := map[uint16]struct{}{}
	add := func(port uint16) {
		if port == 0 {
			return
		}
		seen[port] = struct{}{}
	}

	for _, inbound := range options.Inbounds {
		switch inbound.Type {
		case C.TypeMixed:
			add(inbound.MixedOptions.ListenPort)
		case C.TypeHTTP:
			add(inbound.HTTPOptions.ListenPort)
		case C.TypeSOCKS:
			add(inbound.SocksOptions.ListenPort)
		case C.TypeDirect:
			add(inbound.DirectOptions.ListenPort)
		}
	}

	if options.Experimental != nil && options.Experimental.ClashAPI != nil {
		if port := portFromListenAddress(options.Experimental.ClashAPI.ExternalController); port != 0 {
			add(port)
		}
	}

	ports := make([]uint16, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	return ports
}

func portFromListenAddress(address string) uint16 {
	if address == "" {
		return 0
	}
	if strings.Count(address, ":") == 1 {
		_, portString, err := net.SplitHostPort(address)
		if err == nil {
			port, parseErr := strconv.ParseUint(portString, 10, 16)
			if parseErr == nil {
				return uint16(port)
			}
		}
	}
	port, err := strconv.ParseUint(address, 10, 16)
	if err == nil {
		return uint16(port)
	}
	return 0
}

func busyLocalTCPPorts(ports []uint16) []uint16 {
	busy := make([]uint16, 0, len(ports))
	for _, port := range ports {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 80*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			busy = append(busy, port)
		}
	}
	return busy
}
