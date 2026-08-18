package hcore

import (
	"fmt"
	"os"
	"time"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/observable"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func NewObserver[T any](listenerBufferSize int) *observable.Observer[T] {
	return observable.NewObserver(observable.NewSubscriber[T](listenerBufferSize), listenerBufferSize)
}

func Log(level LogLevel, typ LogType, message ...any) {
	if static.debug.Load() || level != LogLevel_DEBUG {
		log.Debug(level, typ, fmt.Sprint(message...))
		fmt.Printf("%v %v %v\n", level, typ, fmt.Sprint(message...))
		os.Stderr.WriteString(fmt.Sprintf("%v %v %v\n", level, typ, fmt.Sprint(message...)))
	}

	static.logObserver.Emit(newLogMessage(level, typ, fmt.Sprint(message...)))
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
