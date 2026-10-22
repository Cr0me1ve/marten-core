package hcore

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/observable"
)

type MartenInstance struct {
	Box           *libbox.BoxService
	MartenOptions *config.MartenOptions
	// activeConfigPath string
	CoreLogFactory            log.Factory
	coreInfoObserver          *observable.Observer[*coreInfoEvent]
	coreInfoRevision          uint64
	coreInfoLatest            *coreInfoEvent
	CoreState                 CoreStates
	logObserver               *observable.Observer[*LogMessage]
	systemInfoObserver        *observable.Observer[*SystemInfo]
	outboundsInfoObserver     *observable.Observer[*OutboundGroupList]
	mainOutboundsInfoObserver *observable.Observer[*OutboundGroupList]
	lock                      sync.Mutex
	globalPlatformInterface   libbox.PlatformInterface
	previousStartRequest      *StartRequest
	activeLocalListeners      []localListenEndpoint
	startInFlight             bool
	startDone                 chan struct{}
	startCancel               context.CancelFunc
	startAdmissionClosed      bool
	runtimeConfigLock         sync.RWMutex
	settingsChangeLock        sync.Mutex
	debug                     atomic.Bool
	settingsReady             atomic.Bool
	lastConfigHasTun          atomic.Bool
	lastConfigInboundCount    atomic.Int64
}

var static = &MartenInstance{
	coreInfoObserver: NewObserver[*coreInfoEvent](1),
	coreInfoLatest: &coreInfoEvent{
		revision: 0,
		info: &CoreInfoResponse{
			CoreState:   CoreStates_STOPPED,
			MessageType: MessageType_EMPTY,
			Message:     "",
		},
	},
	CoreState:                 CoreStates_STOPPED,
	logObserver:               NewObserver[*LogMessage](1),
	systemInfoObserver:        NewObserver[*SystemInfo](1),
	outboundsInfoObserver:     NewObserver[*OutboundGroupList](1),
	mainOutboundsInfoObserver: NewObserver[*OutboundGroupList](1),
}

type coreRuntimeConfigSnapshot struct {
	martenOptions     *config.MartenOptions
	platformInterface libbox.PlatformInterface
	settingsReady     bool
}

func loadCoreRuntimeConfigSnapshot() coreRuntimeConfigSnapshot {
	static.runtimeConfigLock.RLock()
	defer static.runtimeConfigLock.RUnlock()

	var optionsCopy *config.MartenOptions
	if static.MartenOptions != nil {
		copied := *static.MartenOptions
		optionsCopy = &copied
	}
	return coreRuntimeConfigSnapshot{
		martenOptions:     optionsCopy,
		platformInterface: static.globalPlatformInterface,
		settingsReady:     static.settingsReady.Load(),
	}
}

func publishCoreRuntimeSettings(options *config.MartenOptions, ready bool) {
	static.runtimeConfigLock.Lock()
	static.MartenOptions = options
	static.settingsReady.Store(ready)
	static.runtimeConfigLock.Unlock()
}

func updateCorePlatformInterface(platformInterface libbox.PlatformInterface) {
	static.runtimeConfigLock.Lock()
	if platformInterface != nil || static.globalPlatformInterface == nil {
		static.globalPlatformInterface = platformInterface
	}
	static.runtimeConfigLock.Unlock()
}

func clearCorePlatformInterface() {
	static.runtimeConfigLock.Lock()
	static.globalPlatformInterface = nil
	static.runtimeConfigLock.Unlock()
}
