package hcore

import (
	"fmt"
	"log"
	"sync"
)

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

const turncoatCaptchaLifecycleMarker = "MARTEN_CAPTCHA_LIFECYCLE"

type turncoatCaptchaDeliveryEvent struct {
	sequence uint64
	message  *LogMessage
}

type turncoatCaptchaDeliveryHub struct {
	mu                    sync.Mutex
	sequence              uint64
	nextChallengeRevision uint64
	pendingRevision       uint64
	history               []turncoatCaptchaDeliveryEvent
	pending               *turncoatCaptchaDeliveryEvent
	wake                  chan struct{}
}

var captchaDelivery = turncoatCaptchaDeliveryHub{wake: make(chan struct{})}

func publishTurncoatCaptchaRequired(captchaURL string) {
	captchaDelivery.mu.Lock()
	defer captchaDelivery.mu.Unlock()
	replacedPending := captchaDelivery.pending != nil
	captchaDelivery.nextChallengeRevision++
	revision := captchaDelivery.nextChallengeRevision
	publishTurncoatCaptchaEventLocked(
		newLogMessage(
			LogLevel_WARNING,
			LogType_SERVICE,
			fmt.Sprintf("%s url=%s revision=%d", LogTurncoatCaptchaMarker, captchaURL, revision),
		),
		true,
		revision,
	)
	log.Printf(
		"%s event=required revision=%d sequence=%d replaced_pending=%t",
		turncoatCaptchaLifecycleMarker,
		revision,
		captchaDelivery.sequence,
		replacedPending,
	)
}

func publishTurncoatCaptchaResolved() {
	publishTurncoatCaptchaTerminal(LogLevel_INFO, LogTurncoatCaptchaResolvedMarker)
}

func publishTurncoatCaptchaFailed() {
	publishTurncoatCaptchaTerminal(LogLevel_WARNING, LogTurncoatCaptchaFailedMarker)
}

func publishTurncoatCaptchaTerminal(level LogLevel, marker string) {
	captchaDelivery.mu.Lock()
	defer captchaDelivery.mu.Unlock()
	revision := captchaDelivery.pendingRevision
	pendingMatched := captchaDelivery.pending != nil && revision != 0
	outcome := "failed"
	if marker == LogTurncoatCaptchaResolvedMarker {
		outcome = "resolved"
	}
	message := marker
	if revision != 0 {
		message = fmt.Sprintf("%s revision=%d", marker, revision)
	}
	publishTurncoatCaptchaEventLocked(
		newLogMessage(level, LogType_SERVICE, message),
		false,
		revision,
	)
	log.Printf(
		"%s event=terminal outcome=%s revision=%d sequence=%d pending_matched=%t",
		turncoatCaptchaLifecycleMarker,
		outcome,
		revision,
		captchaDelivery.sequence,
		pendingMatched,
	)
}

func publishTurncoatCaptchaEventLocked(message *LogMessage, pending bool, challengeRevision uint64) {
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
		captchaDelivery.pendingRevision = challengeRevision
	} else {
		captchaDelivery.pending = nil
		captchaDelivery.pendingRevision = 0
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
		revision := captchaDelivery.pendingRevision
		publishTurncoatCaptchaEventLocked(
			newLogMessage(
				LogLevel_WARNING,
				LogType_SERVICE,
				fmt.Sprintf("%s revision=%d", LogTurncoatCaptchaFailedMarker, revision),
			),
			false,
			revision,
		)
		log.Printf(
			"%s event=reset outcome=failed_terminal revision=%d sequence=%d pending=true",
			turncoatCaptchaLifecycleMarker,
			revision,
			captchaDelivery.sequence,
		)
		return
	}
	log.Printf(
		"%s event=reset outcome=no_pending revision=0 sequence=%d pending=false",
		turncoatCaptchaLifecycleMarker,
		captchaDelivery.sequence,
	)
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
	log.Printf(
		"%s event=subscribe revision=%d sequence=%d pending=%t",
		turncoatCaptchaLifecycleMarker,
		captchaDelivery.pendingRevision,
		captchaDelivery.sequence,
		pending != nil,
	)
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
		log.Printf(
			"%s event=replay outcome=history_resync revision=%d sequence=%d pending=%t",
			turncoatCaptchaLifecycleMarker,
			captchaDelivery.pendingRevision,
			captchaDelivery.sequence,
			captchaDelivery.pending != nil,
		)
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
