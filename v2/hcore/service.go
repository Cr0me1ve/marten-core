package hcore

import (
	"context"
	"fmt"
	"io"
	"os"
	runtimeDebug "runtime/debug"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/Cr0me1ve/marten-core/v2/db"
	"github.com/Cr0me1ve/marten-core/v2/hcommon"
	"github.com/Cr0me1ve/marten-core/v2/hutils"
	"github.com/Cr0me1ve/marten-core/v2/service_manager"

	B "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"github.com/sagernet/sing/service/pause"
)

var (
	sWorkingPath          string
	sTempPath             string
	sUserID               int
	sGroupID              int
	statusPropagationPort int64
)

func InitMartenService() error {
	return service_manager.StartServices()
}

func (s *CoreService) Setup(ctx context.Context, req *SetupRequest) (*hcommon.Response, error) {
	err := Setup(req, nil)
	code := hcommon.ResponseCode_OK
	message := ""
	if err != nil {
		code = hcommon.ResponseCode_FAILED
		message = err.Error()
	}
	return &hcommon.Response{Code: code, Message: message}, err
}

func Setup(params *SetupRequest, platformInterface libbox.PlatformInterface) error {
	return coreLifecycle.run(func() error {
		return setup(params, platformInterface)
	})
}

func setup(params *SetupRequest, platformInterface libbox.PlatformInterface) (setupErr error) {
	defer config.DeferPanicToError("setup", func(err error) {
		setupErr = err
		Log(LogLevel_FATAL, LogType_CORE, err.Error())
	})

	static.debug.Store(params.Debug)
	updateCorePlatformInterface(platformInterface)
	serverAlreadyStarted := grpcServerForMode(params.Mode) != nil
	if !serverAlreadyStarted {
		tcpConn := true // runtime.GOOS == "windows" // TODO add TVOS
		libbox.Setup(
			&libbox.SetupOptions{
				BasePath:        params.BasePath,
				WorkingPath:     params.WorkingDir,
				TempPath:        params.TempDir,
				IsTVOS:          !tcpConn,
				FixAndroidStack: true,
			})

		hutils.RedirectStderr(fmt.Sprint(params.WorkingDir, "/data/stderr", params.Mode, ".log"))

		Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf(
			"core filesystem setup state mode=%s tcp=%t",
			params.Mode.String(),
			tcpConn,
		))

		// Install the TURNcoat captcha bridge so dialer-level captcha challenges
		// surface to the Flutter UI through the log stream. Idempotent.
		installCaptchaBridge()

		sWorkingPath = params.WorkingDir
		os.Chdir(sWorkingPath)
		sTempPath = params.TempDir
		sUserID = os.Getuid()
		sGroupID = os.Getgid()

		var defaultWriter io.Writer
		if !params.Debug {
			defaultWriter = io.Discard
		}
		factory, err := log.New(
			log.Options{
				DefaultWriter: defaultWriter,
				BaseTime:      time.Now(),
				Observable:    true,
				// Options: option.LogOptions{
				// 	Disabled: false,
				// 	Level:    "trace",
				// 	Output:   "stdout",
				// },
			})
		static.CoreLogFactory = factory

		if err != nil {
			return E.Cause(err, "create logger")
		}
	}

	settingsLoaded := false
	settingsReadError := false
	settingsSource := "none"
	settingsJSON := ""
	runtimeConfig := loadCoreRuntimeConfigSnapshot()
	if runtimeConfig.settingsReady && runtimeConfig.martenOptions != nil {
		// Foreground Flutter setup and Android background setup share this Go
		// process. Keep the freshly applied in-memory snapshot instead of
		// replacing it with an older LevelDB value during background setup.
		settingsLoaded = true
		settingsSource = "memory"
	} else {
		settings := db.GetTable[hcommon.AppSettings]()
		value, settingsErr := settings.Get("MartenSettingsJson")
		if settingsErr != nil {
			settingsReadError = true
		} else if value != nil {
			var ok bool
			settingsJSON, ok = value.Value.(string)
			if !ok {
				settingsReadError = true
				Log(LogLevel_ERROR, LogType_CORE, "persisted core settings have an unexpected value type")
				settingsJSON = ""
			} else {
				settingsLoaded = true
				settingsSource = "persisted"
			}
		}

		if _, changeErr := restoreMartenSettings(
			&ChangeMartenSettingsRequest{MartenSettingsJson: settingsJSON},
			settingsLoaded,
		); changeErr != nil {
			settingsReadError = true
			settingsLoaded = false
			settingsSource = "default"
			Log(LogLevel_ERROR, LogType_CORE, E.Cause(changeErr, "restore core settings").Error())
			if _, resetErr := restoreMartenSettings(
				&ChangeMartenSettingsRequest{MartenSettingsJson: ""},
				false,
			); resetErr != nil {
				return E.Cause(resetErr, "reset core settings")
			}
		} else if !settingsLoaded {
			settingsSource = "default"
		}
	}

	runtimeConfig = loadCoreRuntimeConfigSnapshot()
	enableTun := runtimeConfig.martenOptions != nil && runtimeConfig.martenOptions.EnableTun
	Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf(
		"core setup state mode=%s settings_source=%s settings_ready=%t settings_error=%t enable_tun=%t platform_interface=%t",
		params.Mode.String(),
		settingsSource,
		runtimeConfig.settingsReady,
		settingsReadError,
		enableTun,
		runtimeConfig.platformInterface != nil,
	))

	if isBackgroundSetupMode(params.Mode) && (!runtimeConfig.settingsReady || settingsReadError) {
		return fmt.Errorf("background core settings unavailable")
	}

	// The gRPC listener is the readiness signal observed by Flutter. Restore
	// settings and initialize extensions before publishing that signal so Start
	// can never build from the transient defaults.
	if err := InitMartenService(); err != nil {
		return E.Cause(err, "initialize core services")
	}
	if serverAlreadyStarted {
		Log(LogLevel_WARNING, LogType_CORE, "grpcServer already started; readiness revalidated")
		return nil
	}

	switch params.Mode {
	case SetupMode_OLD:
		statusPropagationPort = int64(params.FlutterStatusPort)
		return nil
	default:
		Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf(
			"core gRPC readiness publishing mode=%s",
			params.Mode.String(),
		))
		_, err := StartGrpcServerByMode(params.Listen, params.Mode)
		if err != nil {
			return err
		}
	}
	return nil
}

func isBackgroundSetupMode(mode SetupMode) bool {
	return mode == SetupMode_GRPC_BACKGROUND ||
		mode == SetupMode_GRPC_BACKGROUND_INSECURE
}

// SetRuntimeDebug updates the diagnostic mode used by the next core instance.
// Setup can remain alive across multiple Android connects, so changing the app
// preference must not require rebuilding the gRPC service first.
func SetRuntimeDebug(enabled bool) {
	static.debug.Store(enabled)
}

func RuntimeDebugEnabled() bool {
	return static.debug.Load()
}

// RuntimeState returns a secret-free diagnostic snapshot suitable for Android
// logcat even when Flutter has not attached to the core log stream yet.
func RuntimeState() string {
	static.lock.Lock()
	coreState := static.CoreState.String()
	static.lock.Unlock()
	runtimeConfig := loadCoreRuntimeConfigSnapshot()
	enableTun := runtimeConfig.martenOptions != nil && runtimeConfig.martenOptions.EnableTun
	return fmt.Sprintf(
		"debug=%t settings_ready=%t enable_tun=%t platform_interface=%t core_state=%s last_config_tun=%t last_config_inbounds=%d",
		RuntimeDebugEnabled(),
		runtimeConfig.settingsReady,
		enableTun,
		runtimeConfig.platformInterface != nil,
		coreState,
		static.lastConfigHasTun.Load(),
		static.lastConfigInboundCount.Load(),
	)
}

func NewService(options option.Options) (*libbox.BoxService, error) {
	runtimeDebug.FreeOSMemory()
	ctx, cancel := context.WithCancel(context.Background())
	ctx = filemanager.WithDefault(ctx, sWorkingPath, sTempPath, sUserID, sGroupID)
	urlTestHistoryStorage := urltest.NewHistoryStorage()
	ctx = service.ContextWithPtr(ctx, urlTestHistoryStorage)
	instance, err := B.New(B.Options{
		Context:           ctx,
		Options:           options,
		PlatformLogWriter: &LogInterface{},
	})
	if err != nil {
		cancel()
		return nil, E.Cause(err, "create service")
	}
	runtimeDebug.FreeOSMemory()
	service := libbox.NewBoxService(
		ctx,
		cancel,
		instance,
		service.FromContext[pause.Manager](ctx),
		urlTestHistoryStorage,
	)
	return &service, nil
}

func readOptions(configContent string) (option.Options, error) {
	var options option.Options
	err := options.UnmarshalJSON([]byte(configContent))
	if err != nil {
		return option.Options{}, E.Cause(err, "decode config")
	}
	return options, nil
}
