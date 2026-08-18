package hcore

import "sync"

// TURNcoat captcha messages are control-plane events. They must not depend on
// the general log observer, whose deliberately lossy, size-one buffers are
// appropriate for diagnostics but can drop a captcha request while Flutter is
// reconnecting or draining another log line.
//
// The delivery hub keeps a short ordered history for already-connected log
// listeners and separately remembers the one unresolved request for replay to
// a newly connected listener. Publishers only take a mutex; they never wait
// for a gRPC client.
const turncoatCaptchaDeliveryHistoryLimit = 64

type turncoatCaptchaDeliveryEvent struct {
	sequence uint64
	message  *LogMessage
}

type turncoatCaptchaDeliveryHub struct {
	mu       sync.Mutex
	sequence uint64
	history  []turncoatCaptchaDeliveryEvent
	pending  *turncoatCaptchaDeliveryEvent
	wake     chan struct{}
}

var captchaDelivery = turncoatCaptchaDeliveryHub{wake: make(chan struct{})}

func publishTurncoatCaptchaRequired(captchaURL string) {
	publishTurncoatCaptchaEvent(
		newLogMessage(LogLevel_WARNING, LogType_SERVICE, LogTurncoatCaptchaMarker+" url="+captchaURL),
		true,
	)
}

func publishTurncoatCaptchaResolved() {
	publishTurncoatCaptchaEvent(
		newLogMessage(LogLevel_INFO, LogType_SERVICE, LogTurncoatCaptchaResolvedMarker),
		false,
	)
}

func publishTurncoatCaptchaFailed() {
	publishTurncoatCaptchaEvent(
		newLogMessage(LogLevel_WARNING, LogType_SERVICE, LogTurncoatCaptchaFailedMarker),
		false,
	)
}

func publishTurncoatCaptchaEvent(message *LogMessage, pending bool) {
	captchaDelivery.mu.Lock()
	defer captchaDelivery.mu.Unlock()
	publishTurncoatCaptchaEventLocked(message, pending)
}

func publishTurncoatCaptchaEventLocked(message *LogMessage, pending bool) {
	captchaDelivery.sequence++
	event := turncoatCaptchaDeliveryEvent{
		sequence: captchaDelivery.sequence,
		message:  message,
	}
	captchaDelivery.history = append(captchaDelivery.history, event)
	if overflow := len(captchaDelivery.history) - turncoatCaptchaDeliveryHistoryLimit; overflow > 0 {
		copy(captchaDelivery.history, captchaDelivery.history[overflow:])
		captchaDelivery.history = captchaDelivery.history[:turncoatCaptchaDeliveryHistoryLimit]
	}
	if pending {
		pendingEvent := event
		captchaDelivery.pending = &pendingEvent
	} else {
		captchaDelivery.pending = nil
	}

	close(captchaDelivery.wake)
	captchaDelivery.wake = make(chan struct{})
}

// resetTurncoatCaptchaDelivery closes an unresolved request from a previous
// core generation. A connected Flutter listener receives the FAILED marker;
// future listeners no longer replay the stale URL.
func resetTurncoatCaptchaDelivery() {
	captchaDelivery.mu.Lock()
	defer captchaDelivery.mu.Unlock()
	if captchaDelivery.pending != nil {
		publishTurncoatCaptchaEventLocked(
			newLogMessage(LogLevel_WARNING, LogType_SERVICE, LogTurncoatCaptchaFailedMarker),
			false,
		)
	}
}

// subscribeTurncoatCaptchaDelivery starts a listener at the current tail and
// returns the unresolved request, if any, for one immediate replay.
func subscribeTurncoatCaptchaDelivery() (cursor uint64, pending *LogMessage) {
	captchaDelivery.mu.Lock()
	defer captchaDelivery.mu.Unlock()

	cursor = captchaDelivery.sequence
	if captchaDelivery.pending != nil {
		pending = captchaDelivery.pending.message
	}
	return cursor, pending
}

// turncoatCaptchaEventsAfter returns all retained events newer than cursor,
// the cursor to use for the next call, and a channel closed on the next
// publication. If a stalled listener ever falls behind the bounded history,
// it is resynchronised to the current state instead of reopening an obsolete
// request.
func turncoatCaptchaEventsAfter(cursor uint64) (events []*LogMessage, nextCursor uint64, wake <-chan struct{}) {
	captchaDelivery.mu.Lock()
	defer captchaDelivery.mu.Unlock()

	nextCursor = cursor
	wake = captchaDelivery.wake
	if len(captchaDelivery.history) == 0 {
		return nil, nextCursor, wake
	}

	oldest := captchaDelivery.history[0].sequence
	if cursor+1 < oldest {
		if captchaDelivery.pending != nil {
			events = append(events, captchaDelivery.pending.message)
		}
		return events, captchaDelivery.sequence, wake
	}

	for _, event := range captchaDelivery.history {
		if event.sequence <= cursor {
			continue
		}
		events = append(events, event.message)
		nextCursor = event.sequence
	}
	return events, nextCursor, wake
}
