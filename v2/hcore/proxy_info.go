package hcore

import (
	"strings"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/outbound"
	"github.com/sagernet/sing/common/observable"
	"github.com/sagernet/sing/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

var errProxyInfoCoreUnavailable = status.Error(codes.Unavailable, "core service is not running")

func GetProxyInfo(detour adapter.Outbound) *OutboundInfo {
	static.lock.Lock()
	defer static.lock.Unlock()

	if static.Box == nil || static.CoreState != CoreStates_STARTED {
		return nil
	}

	return getProxyInfo(static.Box.UrlTestHistory(), detour)
}

func getProxyInfo(historyStorage *urltest.HistoryStorage, detour adapter.Outbound) *OutboundInfo {
	if historyStorage == nil || detour == nil {
		return nil
	}

	out := &OutboundInfo{
		Tag:  detour.Tag(),
		Type: detour.Type(),
	}
	url_test_history := historyStorage.LoadURLTestHistory(adapter.OutboundTag(detour))
	if url_test_history != nil {
		out.UrlTestTime = timestamppb.New(url_test_history.Time)
		out.UrlTestDelay = int32(url_test_history.Delay)
		if url_test_history.IpInfo != nil {
			out.Ipinfo = &IpInfo{
				Ip:          url_test_history.IpInfo.IP,
				CountryCode: url_test_history.IpInfo.CountryCode,
				Region:      url_test_history.IpInfo.Region,
				City:        url_test_history.IpInfo.City,
				Asn:         int32(url_test_history.IpInfo.ASN),
				Org:         url_test_history.IpInfo.Org,
				Latitude:    url_test_history.IpInfo.Latitude,
				Longitude:   url_test_history.IpInfo.Longitude,
				PostalCode:  url_test_history.IpInfo.PostalCode,
			}
		}
		if _, isGroup := detour.(adapter.OutboundGroup); isGroup {
			out.IsGroup = true
		}

	}

	return out
}

func GetAllProxiesInfo(onlyGroupitems bool) *OutboundGroupList {
	static.lock.Lock()
	defer static.lock.Unlock()

	groups, err := getAllProxiesInfoLocked(static.Box, onlyGroupitems)
	if err != nil {
		return nil
	}
	return groups
}

// getAllProxiesInfoLocked must be called while static.lock is held. Keeping the
// lifecycle lock while reading the box prevents Stop from closing the instance
// halfway through a proxy-info snapshot.
func getAllProxiesInfoLocked(boxService *libbox.BoxService, onlyGroupitems bool) (*OutboundGroupList, error) {
	if boxService == nil || static.Box != boxService || static.CoreState != CoreStates_STARTED {
		return nil, errProxyInfoCoreUnavailable
	}

	instance := boxService.GetInstance()
	historyStorage := boxService.UrlTestHistory()
	if instance == nil || historyStorage == nil {
		return nil, errProxyInfoCoreUnavailable
	}
	router := instance.Router()
	if router == nil {
		return nil, errProxyInfoCoreUnavailable
	}

	var cacheFile adapter.CacheFile
	if boxContext := boxService.Context(); boxContext != nil {
		cacheFile = service.FromContext[adapter.CacheFile](boxContext)
	}
	outbounds := router.Outbounds()
	outbounds_converted := make(map[string]*OutboundInfo, len(outbounds))
	var iGroups []adapter.OutboundGroup
	for _, it := range outbounds {
		if it == nil {
			continue
		}
		if group, isGroup := it.(adapter.OutboundGroup); isGroup {
			iGroups = append(iGroups, group)
		}

		outbounds_converted[it.Tag()] = getProxyInfo(historyStorage, it)
	}

	var groups OutboundGroupList
	for _, iGroup := range iGroups {
		var group OutboundGroup
		group.Tag = iGroup.Tag()
		group.Type = iGroup.Type()
		_, group.Selectable = iGroup.(*outbound.Selector)
		selectedTag := iGroup.Now()
		group.Selected = outbounds_converted[selectedTag]
		if groupInfo := outbounds_converted[iGroup.Tag()]; groupInfo != nil {
			groupInfo.GroupSelectedOutbound = group.Selected
		}
		if cacheFile != nil {
			if isExpand, loaded := cacheFile.LoadGroupExpand(group.Tag); loaded {
				group.IsExpand = isExpand
			}
		}

		for _, itemTag := range iGroup.All() {
			if onlyGroupitems && itemTag != selectedTag {
				continue
			}
			pinfo := outbounds_converted[itemTag]
			if pinfo == nil {
				continue
			}
			pinfo.IsSelected = itemTag == selectedTag

			group.Items = append(group.Items, pinfo)
			pinfo.IsVisible = !strings.Contains(itemTag, "§hide§")
			pinfo.TagDisplay = TrimTagName(itemTag)
		}
		if len(group.Items) == 0 {
			continue
		}
		groups.Items = append(groups.Items, &group)
	}

	return &groups, nil
}

func TrimTagName(tag string) string {
	return strings.Trim(strings.Split(tag, "§")[0], " ")
}

func (s *CoreService) OutboundsInfo(req *hcommon.Empty, stream grpc.ServerStreamingServer[OutboundGroupList]) error {
	return AllProxiesInfoStream(stream, false)
}

func (s *CoreService) MainOutboundsInfo(req *hcommon.Empty, stream grpc.ServerStreamingServer[OutboundGroupList]) error {
	return AllProxiesInfoStream(stream, true)
}

func AllProxiesInfoStream(stream grpc.ServerStreamingServer[OutboundGroupList], onlyMain bool) error {
	boxService, observer, urltestch, done, err := subscribeProxyInfo()
	if err != nil {
		return err
	}
	defer observer.UnSubscribe(urltestch)

	if err := sendProxyInfoSnapshot(stream, boxService, onlyMain); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-done:
			return nil
		case _, ok := <-urltestch:
			if !ok {
				return nil
			}
		debounce:
			for {
				select {
				case _, ok := <-urltestch:
					if !ok {
						return nil
					}
				default:
					break debounce
				}
			}
			if err := sendProxyInfoSnapshot(stream, boxService, onlyMain); err != nil {
				if status.Code(err) == codes.Unavailable {
					return nil
				}
				return err
			}
		}
	}
}

func subscribeProxyInfo() (
	boxService *libbox.BoxService,
	observer *observable.Observer[int],
	urltestch observable.Subscription[int],
	done <-chan struct{},
	err error,
) {
	static.lock.Lock()
	defer static.lock.Unlock()

	boxService = static.Box
	if boxService == nil || static.CoreState != CoreStates_STARTED {
		err = errProxyInfoCoreUnavailable
		return
	}
	historyStorage := boxService.UrlTestHistory()
	if historyStorage == nil {
		err = errProxyInfoCoreUnavailable
		return
	}
	observer = historyStorage.Observer()
	if observer == nil {
		err = errProxyInfoCoreUnavailable
		return
	}
	urltestch, done, err = observer.Subscribe()
	return
}

func sendProxyInfoSnapshot(
	stream grpc.ServerStreamingServer[OutboundGroupList],
	boxService *libbox.BoxService,
	onlyMain bool,
) error {
	groups, err := proxyInfoSnapshot(boxService, onlyMain)
	if err != nil {
		return err
	}
	return stream.Send(groups)
}

func proxyInfoSnapshot(boxService *libbox.BoxService, onlyMain bool) (*OutboundGroupList, error) {
	static.lock.Lock()
	defer static.lock.Unlock()
	return getAllProxiesInfoLocked(boxService, onlyMain)
}
