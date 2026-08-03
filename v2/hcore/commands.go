package hcore

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/Cr0me1ve/marten-core/v2/db"
	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	adapter "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing-box/experimental/clashapi"
	outbound "github.com/sagernet/sing-box/outbound"
	common "github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/batch"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/memory"
	"google.golang.org/grpc"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

func readStatus(prev *SystemInfo) *SystemInfo {
	var message SystemInfo
	message.Memory = int64(memory.Inuse())
	message.Goroutines = int32(runtime.NumGoroutine())
	message.ConnectionsOut = int32(conntrack.Count())
	message.TurncoatRxProofCount = turncoatRXProofCount.Load()
	message.TurncoatActiveSessions = turncoatActiveSessions.Load()
	message.TurncoatReadySessions = turncoatReadySessions.Load()
	message.TurncoatHealthReportCount = turncoatHealthReportCount.Load()

	hasRunningCore := readRunningCoreStatus(&message, prev)
	if hasRunningCore {
		if prev == nil || prev.CurrentProfile == "" || message.UplinkTotal < 1000000 {
			settings := db.GetTable[hcommon.AppSettings]()
			lastName, err := settings.Get("lastStartRequestName")
			if err == nil {
				message.CurrentProfile = lastName.Value.(string)
			}
		} else {
			message.CurrentProfile = prev.CurrentProfile
		}
	}

	return &message
}

func readRunningCoreStatus(message *SystemInfo, prev *SystemInfo) bool {
	static.lock.Lock()
	defer static.lock.Unlock()

	boxService := static.Box
	if boxService != nil && static.CoreState == CoreStates_STARTED {
		instance := boxService.GetInstance()
		if instance != nil {
			router := instance.Router()
			if router != nil {
				if clashServer := router.ClashServer(); clashServer != nil {
					if server, ok := clashServer.(*clashapi.Server); ok {
						message.TrafficAvailable = true
						trafficManager := server.TrafficManager()
						message.UplinkTotal, message.DownlinkTotal = trafficManager.Total()
						message.ConnectionsIn = int32(trafficManager.ConnectionsLen())
						if prev != nil {
							message.Uplink = message.UplinkTotal - prev.UplinkTotal
							message.Downlink = message.DownlinkTotal - prev.DownlinkTotal
						}
					}
				}

				if currentOutBound, ok := router.Outbound(config.OutboundSelectTag); ok {
					if selectOutBound, ok := currentOutBound.(*outbound.Selector); ok {
						message.CurrentOutbound = TrimTagName(selectOutBound.Now())
					}
				}
				if message.CurrentOutbound == config.OutboundURLTestTag {
					if currentOutBound, ok := router.Outbound(config.OutboundURLTestTag); ok {
						if urltest, ok := currentOutBound.(*outbound.URLTest); ok {
							message.CurrentOutbound = fmt.Sprint(message.CurrentOutbound, "→", TrimTagName(urltest.Now()))
						}
					}
				}
				return true
			}
		}
	}
	return false
}

func (s *CoreService) GetSystemInfo(req *hcommon.Empty, stream grpc.ServerStreamingServer[SystemInfo]) error {
	// return fmt.Errorf("not implemented yet")
	ticker := time.NewTicker(time.Duration(1 * time.Second))
	defer ticker.Stop()
	current_status := readStatus(nil)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			current_status = readStatus(current_status)
			if err := stream.Send(current_status); err != nil {
				return err
			}
		}
	}
}

func (s *CoreService) GetTurncoatRouteEvidence(context.Context, *hcommon.Empty) (*TurncoatRouteEvidence, error) {
	return &TurncoatRouteEvidence{
		RxProofCount:      turncoatRXProofCount.Load(),
		ActiveSessions:    turncoatActiveSessions.Load(),
		ReadySessions:     turncoatReadySessions.Load(),
		HealthReportCount: turncoatHealthReportCount.Load(),
	}, nil
}

// func (s *CoreService) OutboundsInfo(req *hcommon.Empty, stream grpc.ServerStreamingServer[OutboundGroupList]) error {
// 	if groupClient == nil {
// 		groupClient = libbox.NewCommandClient(
// 			&CommandClientHandler{
// 				command: libbox.CommandGroup,
// 				// port:   s.port,
// 			},
// 			&libbox.CommandClientOptions{
// 				Command:        libbox.CommandGroup,
// 				StatusInterval: 500000000, // 500ms debounce
// 			},
// 		)

// 		defer func() {
// 			groupClient.Disconnect()
// 			groupClient = nil
// 		}()

// 		groupClient.Connect()
// 	}

// 	sub, done, _ := outboundsInfoObserver.Subscribe()

// 	for {
// 		select {
// 		case <-stream.Context().Done():
// 			return nil
// 		case <-done:
// 			return nil
// 		case info := <-sub:
// 			stream.Send(info)
// 			// case <-time.After(500 * time.Millisecond):
// 		}
// 	}
// }

// func (s *CoreService) MainOutboundsInfo(req *hcommon.Empty, stream grpc.ServerStreamingServer[OutboundGroupList]) error {
// 	if groupInfoOnlyClient == nil {
// 		groupInfoOnlyClient = libbox.NewCommandClient(
// 			&CommandClientHandler{
// 				command: libbox.CommandGroupInfoOnly,
// 				// port:   s.port,
// 			},
// 			&libbox.CommandClientOptions{
// 				Command:        libbox.CommandGroupInfoOnly,
// 				StatusInterval: 500000000, // 500ms debounce
// 			},
// 		)

// 		defer func() {
// 			groupInfoOnlyClient.Disconnect()
// 			groupInfoOnlyClient = nil
// 		}()
// 		groupInfoOnlyClient.Connect()
// 	}

// 	sub, stopch, _ := mainOutboundsInfoObserver.Subscribe()

// 	for {
// 		select {
// 		case <-stream.Context().Done():
// 			return nil
// 		case <-stopch:
// 			return nil
// 		case info := <-sub:
// 			stream.Send(info)
// 			// case <-time.After(500 * time.Millisecond):
// 		}
// 	}
// }

func (s *CoreService) SelectOutbound(ctx context.Context, in *SelectOutboundRequest) (*hcommon.Response, error) {
	return SelectOutbound(in)
}

func SelectOutbound(in *SelectOutboundRequest) (*hcommon.Response, error) {
	// err := libbox.NewStandaloneCommandClient().SelectOutbound(in.GroupTag, in.OutboundTag)
	// if err != nil {
	// 	return &hcommon.Response{
	// 		Code:    hcommon.ResponseCode_FAILED,
	// 		Message: err.Error(),
	// 	}, err
	// }

	// return &hcommon.Response{
	// 	Code:    hcommon.ResponseCode_OK,
	// 	Message: "",
	// }, nil
	if in == nil {
		return failedCommandResponse(E.New("select outbound request is nil"))
	}
	Log(LogLevel_DEBUG, LogType_CORE, "select outbound: ", in.GroupTag, " -> ", in.OutboundTag)
	static.lock.Lock()
	boxService := static.Box
	if boxService == nil || static.CoreState != CoreStates_STARTED {
		static.lock.Unlock()
		return failedCommandResponse(E.New("core service is not running"))
	}
	instance := boxService.GetInstance()
	if instance == nil {
		static.lock.Unlock()
		return failedCommandResponse(E.New("core service instance is not running"))
	}
	router := instance.Router()
	outboundGroup, isLoaded := router.Outbound(in.GroupTag)
	if !isLoaded {
		static.lock.Unlock()
		return &hcommon.Response{
			Code:    hcommon.ResponseCode_FAILED,
			Message: E.New("selector not found: ", in.GroupTag).Error(),
		}, E.New("selector not found: ", in.GroupTag)
	}
	selector, isSelector := outboundGroup.(*outbound.Selector)
	if !isSelector {
		static.lock.Unlock()
		return &hcommon.Response{
			Code:    hcommon.ResponseCode_FAILED,
			Message: E.New("outbound is not a selector: ", in.GroupTag).Error(),
		}, E.New("outbound is not a selector: ", in.GroupTag)
	}
	if !selector.SelectOutbound(in.OutboundTag) {
		static.lock.Unlock()
		return &hcommon.Response{
			Code:    hcommon.ResponseCode_FAILED,
			Message: E.New("outbound not found in selector:: ", in.GroupTag).Error(),
		}, E.New("outbound not found in selector: ", in.GroupTag)
	}
	urlTests := make([]*outbound.URLTest, 0)
	if !in.SkipProbe {
		Log(LogLevel_DEBUG, LogType_CORE, "Trying to ping outbound: ", in.OutboundTag)
		for _, detour := range router.Outbounds() {
			if urlTest, ok := detour.(*outbound.URLTest); ok {
				urlTests = append(urlTests, urlTest)
			}
		}
	}
	historyStorage := boxService.UrlTestHistory()
	static.lock.Unlock()

	if !in.SkipProbe {
		go func(outboundTag string, urlTests []*outbound.URLTest) {
			for _, urlTest := range urlTests {
				if urlTest.ForceRecheckOutbound(outboundTag) == nil {
					break
				}
			}
		}(in.OutboundTag, urlTests)
	}
	if historyStorage != nil {
		historyStorage.Observer().Emit(2)
	}
	return &hcommon.Response{
		Code:    hcommon.ResponseCode_OK,
		Message: "",
	}, nil
}

func failedCommandResponse(err error) (*hcommon.Response, error) {
	return &hcommon.Response{
		Code:    hcommon.ResponseCode_FAILED,
		Message: err.Error(),
	}, err
}

func (s *CoreService) ProbeSelectedRoute(ctx context.Context, in *UrlTestRequest) (*OutboundInfo, error) {
	return ProbeSelectedRoute(ctx, in)
}

func ProbeSelectedRoute(ctx context.Context, in *UrlTestRequest) (*OutboundInfo, error) {
	if in == nil {
		return nil, E.New("selected route probe request is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	static.lock.Lock()
	boxService := static.Box
	if boxService == nil || static.CoreState != CoreStates_STARTED {
		static.lock.Unlock()
		return nil, E.New("core service is not running")
	}
	instance := boxService.GetInstance()
	if instance == nil {
		static.lock.Unlock()
		return nil, E.New("core service instance is not running")
	}
	router := instance.Router()
	historyStorage := boxService.UrlTestHistory()
	boxContext := boxService.Context()
	if router == nil || historyStorage == nil || boxContext == nil {
		static.lock.Unlock()
		return nil, E.New("selected route probe is unavailable")
	}

	current, loaded := router.Outbound(in.GroupTag)
	if !loaded {
		static.lock.Unlock()
		return nil, E.New("outbound group not found: ", in.GroupTag)
	}
	visited := make(map[string]bool)
	for {
		group, isGroup := current.(adapter.OutboundGroup)
		if !isGroup {
			break
		}
		selectedTag := group.Now()
		if selectedTag == "" {
			static.lock.Unlock()
			return nil, E.New("selected outbound is empty: ", group.Tag())
		}
		if visited[selectedTag] {
			static.lock.Unlock()
			return nil, E.New("selected outbound cycle detected: ", selectedTag)
		}
		visited[selectedTag] = true
		selected, found := router.Outbound(selectedTag)
		if !found {
			static.lock.Unlock()
			return nil, E.New("selected outbound not found: ", selectedTag)
		}
		current = selected
	}
	selectedOutbound := current
	selectedTag := outbound.RealTag(selectedOutbound)
	static.lock.Unlock()

	probeStartedAt := time.Now()
	delay := outbound.CheckOutbound(
		instance.GetLogger(),
		boxContext,
		historyStorage,
		router,
		"",
		selectedOutbound,
		nil,
	)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	static.lock.Lock()
	stillCurrent := static.Box == boxService && static.CoreState == CoreStates_STARTED
	static.lock.Unlock()
	if !stillCurrent {
		return nil, E.New("core service changed during selected route probe")
	}

	history := historyStorage.LoadURLTestHistory(selectedTag)
	if history == nil || history.Time.Before(probeStartedAt) {
		return nil, E.New("selected route probe did not publish a fresh result: ", selectedTag)
	}
	historyStorage.Observer().Emit(2)
	return &OutboundInfo{
		Tag:          selectedTag,
		Type:         selectedOutbound.Type(),
		UrlTestTime:  timestamppb.New(history.Time),
		UrlTestDelay: int32(delay),
	}, nil
}

func (s *CoreService) UrlTest(ctx context.Context, in *UrlTestRequest) (*hcommon.Response, error) {
	return UrlTest(in)
}

func UrlTest(in *UrlTestRequest) (*hcommon.Response, error) {
	// err := libbox.NewStandaloneCommandClient().URLTest(in.GroupTag)
	// if err != nil {
	// 	return &hcommon.Response{
	// 		Code:    hcommon.ResponseCode_FAILED,
	// 		Message: err.Error(),
	// 	}, err
	// }

	// return &hcommon.Response{
	// 	Code:    hcommon.ResponseCode_OK,
	// 	Message: "",
	// }, nil

	if in == nil {
		return failedCommandResponse(E.New("url test request is nil"))
	}

	groupTag := in.GroupTag

	static.lock.Lock()
	boxService := static.Box
	if boxService == nil || static.CoreState != CoreStates_STARTED {
		static.lock.Unlock()
		return failedCommandResponse(E.New("core service is not running"))
	}
	instance := boxService.GetInstance()
	if instance == nil {
		static.lock.Unlock()
		return failedCommandResponse(E.New("core service instance is not running"))
	}
	router := instance.Router()
	historyStorage := boxService.UrlTestHistory()
	boxContext := boxService.Context()
	static.lock.Unlock()

	if historyStorage == nil {
		return failedCommandResponse(E.New("url test history is not available"))
	}

	abstractOutboundGroup, isLoaded := router.Outbound(groupTag)
	if !isLoaded {
		return &hcommon.Response{
			Code:    hcommon.ResponseCode_FAILED,
			Message: E.New("outbound group not found: ", in.GroupTag).Error(),
		}, E.New("outbound group not found: ", groupTag)
	}
	outboundGroup, isOutboundGroup := abstractOutboundGroup.(adapter.OutboundGroup)
	if !isOutboundGroup {
		return &hcommon.Response{
			Code:    hcommon.ResponseCode_FAILED,
			Message: E.New("outbound is not a group: ", in.GroupTag).Error(),
		}, E.New("outbound is not a group: ", groupTag)
	}

	if urlTest, isURLTest := abstractOutboundGroup.(*outbound.URLTest); isURLTest {
		go func() {
			defer config.DeferPanicToError("urltest-force-recheck", func(err error) {
				Log(LogLevel_ERROR, LogType_CORE, err.Error())
			})
			for _, p := range router.Outbounds() {
				if p.Tag() == groupTag {
					continue
				}
				if group, isGroup := p.(adapter.OutboundGroup); isGroup {
					urlTest.ForceRecheckOutbound(group.Now())
				}
			}
			urlTest.CheckOutbounds()
		}()
	} else {
		outbounds := common.Filter(common.Map(outboundGroup.All(), func(it string) adapter.Outbound {
			itOutbound, _ := router.Outbound(it)
			return itOutbound
		}), func(it adapter.Outbound) bool {
			if it == nil {
				return false
			}
			_, isGroup := it.(adapter.OutboundGroup)
			return !isGroup
		})
		b, _ := batch.New(boxContext, batch.WithConcurrencyNum[any](20))
		for _, detour := range outbounds {
			outboundToTest := detour
			outboundTag := outboundToTest.Tag()
			b.Go(outboundTag, func() (result any, err error) {
				defer config.DeferPanicToError("urltest-check-outbound", func(recovered error) {
					err = recovered
				})
				outbound.CheckOutbound(instance.GetLogger(), boxContext, historyStorage, router, "", outboundToTest, nil)
				return nil, nil
			})
		}
		_, _ = b.WaitAndGetResult()
		static.lock.Lock()
		shouldEmit := static.Box == boxService && static.CoreState == CoreStates_STARTED
		static.lock.Unlock()
		if shouldEmit {
			historyStorage.Observer().Emit(2)
		}
	}

	return &hcommon.Response{
		Code:    hcommon.ResponseCode_OK,
		Message: "",
	}, nil
}
