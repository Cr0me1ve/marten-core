package hcore

import (
	"fmt"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"google.golang.org/grpc"
)

type coreInfoEvent struct {
	revision uint64
	info     *CoreInfoResponse
}

func coreInfoResponse(state CoreStates, msgType MessageType, message string) *CoreInfoResponse {
	return &CoreInfoResponse{
		CoreState:   state,
		MessageType: msgType,
		Message:     message,
	}
}

// setCoreStatusLocked publishes an authoritative lifecycle transition. The
// caller must hold static.lock so the state write and observer notification
// remain ordered with the Box generation that owns the transition.
func setCoreStatusLocked(state CoreStates, msgType MessageType, message string) *CoreInfoResponse {
	msg := fmt.Sprintf("%s: %s %s", state.String(), msgType.String(), message)
	if msgType == MessageType_EMPTY {
		msg = fmt.Sprintf("%s: %s", state.String(), message)
	}
	Log(LogLevel_INFO, LogType_CORE, msg)
	static.CoreState = state
	info := coreInfoResponse(state, msgType, message)
	static.coreInfoRevision++
	event := &coreInfoEvent{
		revision: static.coreInfoRevision,
		// Keep the replay source independent from the response returned to an
		// RPC caller, which is outside the lifecycle lock after this function.
		info: coreInfoResponse(state, msgType, message),
	}
	static.coreInfoLatest = event
	static.coreInfoObserver.Emit(event)

	return info
}

// SetCoreStatus is the lock-safe entry point for callers that do not already
// participate in the exact Box lifecycle transaction.
func SetCoreStatus(state CoreStates, msgType MessageType, message string) *CoreInfoResponse {
	static.lock.Lock()
	defer static.lock.Unlock()
	return setCoreStatusLocked(state, msgType, message)
}

func currentCoreInfoResponse(msgType MessageType, message string) *CoreInfoResponse {
	static.lock.Lock()
	defer static.lock.Unlock()
	return coreInfoResponse(static.CoreState, msgType, message)
}

func currentCoreInfoEvent(msgType MessageType, message string) *coreInfoEvent {
	static.lock.Lock()
	defer static.lock.Unlock()
	return &coreInfoEvent{
		revision: static.coreInfoRevision,
		info:     coreInfoResponse(static.CoreState, msgType, message),
	}
}

func latestCoreInfoEvent() *coreInfoEvent {
	static.lock.Lock()
	defer static.lock.Unlock()
	if static.coreInfoLatest == nil || static.coreInfoLatest.info == nil {
		return &coreInfoEvent{
			revision: static.coreInfoRevision,
			info:     coreInfoResponse(static.CoreState, MessageType_EMPTY, ""),
		}
	}
	latest := static.coreInfoLatest
	return &coreInfoEvent{
		revision: latest.revision,
		info: coreInfoResponse(
			latest.info.CoreState,
			latest.info.MessageType,
			latest.info.Message,
		),
	}
}

// sendLatestCoreInfo catches a slow stream up to the latest authoritative
// lifecycle revision. Observable notifications are only wake-ups: its bounded
// drop-new buffers cannot be the source of truth for terminal state delivery.
func sendLatestCoreInfo(
	stream grpc.ServerStreamingServer[CoreInfoResponse],
	lastRevision *uint64,
) error {
	for {
		latest := latestCoreInfoEvent()
		if latest.revision <= *lastRevision {
			return nil
		}
		if err := stream.Send(latest.info); err != nil {
			return err
		}
		*lastRevision = latest.revision
	}
}

func (s *CoreService) CoreInfoListener(req *hcommon.Empty, stream grpc.ServerStreamingServer[CoreInfoResponse]) error {
	coreSub, done, err := static.coreInfoObserver.Subscribe()
	if err != nil {
		return err
	}
	defer static.coreInfoObserver.UnSubscribe(coreSub)
	snapshot := currentCoreInfoEvent(MessageType_EMPTY, "")
	if err := stream.Send(snapshot.info); err != nil {
		return err
	}
	lastRevision := snapshot.revision
	if err := sendLatestCoreInfo(stream, &lastRevision); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-done:
			return nil
		case _, ok := <-coreSub:
			if !ok {
				return nil
			}
			if err := sendLatestCoreInfo(stream, &lastRevision); err != nil {
				return err
			}
		}
	}
}
