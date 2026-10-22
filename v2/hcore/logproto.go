package hcore

import (
	"fmt"
	"os"
	"time"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"github.com/sagernet/sing/common/observable"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func NewObserver[T any](listenerBufferSize int) *observable.Observer[T] {
	return observable.NewObserver(observable.NewSubscriber[T](listenerBufferSize), listenerBufferSize)
}

func Log(level LogLevel, typ LogType, message ...any) {
	record := newLogMessage(level, typ, fmt.Sprint(message...))
	if static.debug.Load() || level != LogLevel_DEBUG {
		// Mobile setup redirects stderr to the native diagnostic file. The
		// global sing-box logger also writes to stderr; forwarding through it
		// and stdout duplicated every record even with no Flutter listener.
		// Keep one timestamped native copy and reuse the same record for
		// runtime delivery, including when the console write fails.
		_, _ = fmt.Fprintf(os.Stderr, "%s %v %v %s\n",
			record.Time.AsTime().Format(time.RFC3339Nano), level, typ, record.Message)
	}

	static.logObserver.Emit(record)
}

func newLogMessage(level LogLevel, typ LogType, message string) *LogMessage {
	return &LogMessage{
		Level:   level,
		Type:    typ,
		Time:    timestamppb.New(time.Now()),
		Message: message,
	}
}

func (s *CoreService) LogListener(req *hcommon.Empty, stream grpc.ServerStreamingServer[LogMessage]) error {
	logSub, stopch, _ := static.logObserver.Subscribe()
	defer static.logObserver.UnSubscribe(logSub)
	captchaCursor, pendingCaptcha := subscribeTurncoatCaptchaDelivery()
	if pendingCaptcha != nil {
		if err := stream.Send(pendingCaptcha); err != nil {
			return err
		}
	}

	for {
		captchaEvents, nextCursor, captchaWake := turncoatCaptchaEventsAfter(captchaCursor)
		for _, info := range captchaEvents {
			if err := stream.Send(info); err != nil {
				return err
			}
		}
		captchaCursor = nextCursor

		select {
		case <-stream.Context().Done():
			return nil
		case <-stopch:
			return nil
		case <-captchaWake:
			continue
		case info, ok := <-logSub:
			if !ok {
				return nil
			}
			if err := stream.Send(info); err != nil {
				return err
			}
			// case <-time.After(500 * time.Millisecond):
		}
	}
}
