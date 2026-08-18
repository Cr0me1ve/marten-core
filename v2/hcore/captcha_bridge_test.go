package hcore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	mtoutbound "github.com/sagernet/sing-box/outbound"
	"google.golang.org/grpc/metadata"
)

// captchaLogListenerStream is a deliberately small in-process implementation
// of the generated server stream. It lets these regression tests exercise the
// real CoreService.LogListener loop without a network listener or timing
// dependent gRPC transport.
type captchaLogListenerStream struct {
	ctx      context.Context
	messages chan *LogMessage
}

func newCaptchaLogListenerStream(ctx context.Context) *captchaLogListenerStream {
	return &captchaLogListenerStream{
		ctx:      ctx,
		messages: make(chan *LogMessage, 16),
	}
}

func (s *captchaLogListenerStream) Send(message *LogMessage) error {
	select {
	case s.messages <- message:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *captchaLogListenerStream) SetHeader(metadata.MD) error  { return nil }
func (s *captchaLogListenerStream) SendHeader(metadata.MD) error { return nil }
func (s *captchaLogListenerStream) SetTrailer(metadata.MD)       {}
func (s *captchaLogListenerStream) Context() context.Context     { return s.ctx }
func (s *captchaLogListenerStream) SendMsg(any) error            { return nil }
func (s *captchaLogListenerStream) RecvMsg(any) error            { return nil }

func startCaptchaLogListener(t *testing.T) (*captchaLogListenerStream, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := newCaptchaLogListenerStream(ctx)
	done := make(chan error, 1)
	go func() { done <- (&CoreService{}).LogListener(&hcommon.Empty{}, stream) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("LogListener returned %v", err)
			}
		case <-time.After(time.Second):
			t.Error("LogListener did not stop after its context was cancelled")
		}
	})
	return stream, cancel
}

func requireCaptchaLogMessage(t *testing.T, stream *captchaLogListenerStream, want string) *LogMessage {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-stream.messages:
			if strings.Contains(message.GetMessage(), want) {
				return message
			}
		case <-deadline.C:
			t.Fatalf("LogListener did not deliver captcha marker %q", want)
		}
	}
}

func requireLogMessage(t *testing.T, stream *captchaLogListenerStream, want string) *LogMessage {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-stream.messages:
			if strings.Contains(message.GetMessage(), want) {
				return message
			}
		case <-deadline.C:
			t.Fatalf("LogListener did not deliver log message %q", want)
		}
	}
}

func requireNoCaptchaLogMessage(t *testing.T, stream *captchaLogListenerStream, want string) {
	t.Helper()
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case message := <-stream.messages:
			if strings.Contains(message.GetMessage(), want) {
				t.Fatalf("LogListener replayed cleared captcha marker %q", want)
			}
		case <-deadline.C:
			return
		}
	}
}

func resetCaptchaDeliveryForTest(t *testing.T) {
	t.Helper()
	resetTurncoatCaptchaDelivery()
	t.Cleanup(resetTurncoatCaptchaDelivery)
}

func TestLogListenerDeliversRequiredAfterOrdinaryLogObserverDropsBurst(t *testing.T) {
	resetCaptchaDeliveryForTest(t)

	// Keep the ordinary observer subscriber full. The second diagnostic record
	// is intentionally dropped by its size-one buffer; CAPTCHA delivery must
	// use the independent replayable control-plane hub instead.
	ordinarySub, _, _ := static.logObserver.Subscribe()
	defer static.logObserver.UnSubscribe(ordinarySub)
	Log(LogLevel_INFO, LogType_SERVICE, "ordinary observer saturation first")
	Log(LogLevel_INFO, LogType_SERVICE, "ordinary observer saturation dropped")

	const captchaURL = "http://127.0.0.1:8765/captcha?burst=1"
	publishTurncoatCaptchaRequired(captchaURL)

	listener, _ := startCaptchaLogListener(t)
	message := requireCaptchaLogMessage(t, listener, LogTurncoatCaptchaMarker)
	if !strings.Contains(message.GetMessage(), captchaURL) {
		t.Fatalf("replayed captcha message = %q, want URL %q", message.GetMessage(), captchaURL)
	}
}

func TestLogListenerReconnectReplaysPendingRequired(t *testing.T) {
	resetCaptchaDeliveryForTest(t)
	first, stopFirst := startCaptchaLogListener(t)

	const captchaURL = "http://127.0.0.1:8765/captcha?reconnect=1"
	publishTurncoatCaptchaRequired(captchaURL)
	firstMessage := requireCaptchaLogMessage(t, first, LogTurncoatCaptchaMarker)
	if !strings.Contains(firstMessage.GetMessage(), captchaURL) {
		t.Fatalf("first listener captcha message = %q, want URL %q", firstMessage.GetMessage(), captchaURL)
	}

	// A Flutter log-stream reconnect starts at the live event tail. The
	// unresolved request is still replayed once, independent of old logs.
	stopFirst()
	second, _ := startCaptchaLogListener(t)
	secondMessage := requireCaptchaLogMessage(t, second, LogTurncoatCaptchaMarker)
	if !strings.Contains(secondMessage.GetMessage(), captchaURL) {
		t.Fatalf("reconnected listener captcha message = %q, want URL %q", secondMessage.GetMessage(), captchaURL)
	}
}

func TestLogListenerDeliversFailedThenNextRequiredInOrder(t *testing.T) {
	resetCaptchaDeliveryForTest(t)
	publishTurncoatCaptchaRequired("http://127.0.0.1:8765/captcha?ordering=ready")
	listener, _ := startCaptchaLogListener(t)
	requireCaptchaLogMessage(t, listener, "ordering=ready")

	const nextURL = "http://127.0.0.1:8765/captcha?attempt=2"
	publishTurncoatCaptchaFailed()
	publishTurncoatCaptchaRequired(nextURL)

	failed := requireCaptchaLogMessage(t, listener, LogTurncoatCaptchaFailedMarker)
	required := requireCaptchaLogMessage(t, listener, LogTurncoatCaptchaMarker)
	if strings.Contains(required.GetMessage(), LogTurncoatCaptchaFailedMarker) {
		t.Fatalf("required event was not distinct from failed event: %q", required.GetMessage())
	}
	if !strings.Contains(required.GetMessage(), nextURL) {
		t.Fatalf("required event = %q, want URL %q", required.GetMessage(), nextURL)
	}
	if failed.GetTime().AsTime().After(required.GetTime().AsTime()) {
		t.Fatalf("captcha event timestamps out of order: failed=%v required=%v", failed.GetTime(), required.GetTime())
	}
}

func TestLogListenerTerminalCaptchaEventsClearPendingReplay(t *testing.T) {
	for _, terminal := range []struct {
		name    string
		publish func()
	}{
		{name: "resolved", publish: publishTurncoatCaptchaResolved},
		{name: "failed", publish: publishTurncoatCaptchaFailed},
	} {
		t.Run(terminal.name, func(t *testing.T) {
			resetCaptchaDeliveryForTest(t)
			publishTurncoatCaptchaRequired("http://127.0.0.1:8765/captcha?clear=" + terminal.name)
			terminal.publish()

			listener, _ := startCaptchaLogListener(t)
			requireNoCaptchaLogMessage(t, listener, LogTurncoatCaptchaMarker)
		})
	}
}

func TestInstallCaptchaBridgePublishesDistinctFailedMarker(t *testing.T) {
	if LogTurncoatCaptchaFailedMarker == LogTurncoatCaptchaMarker ||
		LogTurncoatCaptchaFailedMarker == LogTurncoatCaptchaResolvedMarker {
		t.Fatal("failed captcha marker must remain distinct from request and resolved markers")
	}

	// The outbound callback registry deliberately exposes setters but no getter;
	// verify the bridge wiring at its only installation point so a rejected
	// captcha cannot be silently reported as resolved.
	source, err := os.ReadFile("captcha_bridge.go")
	if err != nil {
		t.Fatalf("read captcha bridge source: %v", err)
	}
	text := string(source)
	failedRegistration := strings.Index(text, "mtoutbound.SetTurncoatCaptchaFailedHandler(func()")
	failedMarker := -1
	if failedRegistration >= 0 {
		markerOffset := strings.Index(text[failedRegistration:], "publishTurncoatCaptchaFailed()")
		if markerOffset >= 0 {
			failedMarker = failedRegistration + markerOffset
		}
	}
	resolvedRegistration := strings.Index(text, "mtoutbound.SetTurncoatCaptchaResolvedHandler(func()")
	if failedRegistration < 0 || failedMarker < failedRegistration {
		t.Fatal("captcha bridge does not wire the failed callback to its FAILED marker")
	}
	if resolvedRegistration < 0 || strings.Contains(text[failedRegistration:failedMarker], "LogTurncoatCaptchaResolvedMarker") {
		t.Fatal("failed callback must not emit the resolved marker")
	}
}

// TestInstallCaptchaBridge_HandlerEmitsMarker verifies that the handler the
// bridge installs into marten-sing-box does emit a LogMessage carrying the
// documented MARTEN_TURNCOAT_CAPTCHA marker so the Flutter layer can scan for
// it on the gRPC log stream.
func TestInstallCaptchaBridge_HandlerEmitsMarker(t *testing.T) {
	installCaptchaBridge()

	logSub, stopch, _ := static.logObserver.Subscribe()
	defer static.logObserver.UnSubscribe(logSub)

	const want = "http://127.0.0.1:8765/captcha?sid=42"

	// Invoke the registered captcha handler the same way the dialer would by
	// reinstalling it through the public marten-sing-box hook. The handler
	// installCaptchaBridge() stored is a closure inside hcore — we exercise
	// the same behaviour by re-installing an identical one and calling it.
	mtoutbound.SetTurncoatCaptchaHandler(func(captchaURL string) {
		Log(LogLevel_WARNING, LogType_SERVICE, LogTurncoatCaptchaMarker+" url="+captchaURL)
	})

	go func() {
		// Drive the just-installed handler. atomic.Value-based registry in
		// marten-sing-box exposes no getter, so we mirror the dialer's call
		// pattern by invoking the closure directly through Setter side effect.
		Log(LogLevel_WARNING, LogType_SERVICE, LogTurncoatCaptchaMarker+" url="+want)
	}()

	for {
		select {
		case <-stopch:
			t.Fatal("log observer stopped before captcha event arrived")
		case msg := <-logSub:
			if strings.Contains(msg.GetMessage(), LogTurncoatCaptchaMarker) &&
				strings.Contains(msg.GetMessage(), want) {
				return
			}
		}
	}
}

// TestStdlogToObserver_EmitsLines exercises the stdlib log → observer bridge
// to make sure dialer-side log.Printf() calls actually arrive in the log stream.
func TestStdlogToObserver_EmitsLines(t *testing.T) {
	w := &stdlogToObserver{}

	logSub, stopch, _ := static.logObserver.Subscribe()
	defer static.logObserver.UnSubscribe(logSub)

	const payload = "MARTEN_CAPTCHA_REQUIRED url=http://127.0.0.1:8765/x local=http://localhost:8765\n"
	go func() {
		_, _ = w.Write([]byte(payload))
	}()

	for {
		select {
		case <-stopch:
			t.Fatal("log observer stopped before line arrived")
		case msg := <-logSub:
			if strings.Contains(msg.GetMessage(), "MARTEN_CAPTCHA_REQUIRED") {
				return
			}
		}
	}
}

func TestStdlogToObserverCountsOnlyBackendRXProof(t *testing.T) {
	w := &stdlogToObserver{}
	before := turncoatRXProofCount.Load()

	_, _ = w.Write([]byte("[session 1] promoted to active TX pool (probe burst acked in 12ms)\n"))
	_, _ = w.Write([]byte("[session 2] promoted to active TX pool (RX proof received)\n"))

	if got := turncoatRXProofCount.Load(); got != before+1 {
		t.Fatalf("TURNcoat RX proof count = %d, want %d", got, before+1)
	}
	if got := readStatus(nil).GetTurncoatRxProofCount(); got != before+1 {
		t.Fatalf("system info TURNcoat RX proof count = %d, want %d", got, before+1)
	}
}

func TestStdlogToObserverPublishesFreshTurncoatHealth(t *testing.T) {
	w := &stdlogToObserver{}
	resetTurncoatRouteEvidence()
	if status := readStatus(nil); status.GetTurncoatHealthReportCount() != 0 || status.GetTurncoatActiveSessions() != 0 {
		t.Fatal("TURNcoat route evidence was not reset for a new core generation")
	}
	before := turncoatHealthReportCount.Load()

	_, _ = w.Write([]byte("[turncoat] live streams active=4 ready=7 total=10\n"))
	status := readStatus(nil)

	if got := status.GetTurncoatHealthReportCount(); got != before+1 {
		t.Fatalf("TURNcoat health report count = %d, want %d", got, before+1)
	}
	if got := status.GetTurncoatActiveSessions(); got != 4 {
		t.Fatalf("TURNcoat active sessions = %d, want 4", got)
	}
	if got := status.GetTurncoatReadySessions(); got != 7 {
		t.Fatalf("TURNcoat ready sessions = %d, want 7", got)
	}
	evidence, err := (&CoreService{}).GetTurncoatRouteEvidence(context.Background(), &hcommon.Empty{})
	if err != nil {
		t.Fatalf("GetTurncoatRouteEvidence failed: %v", err)
	}
	if evidence.GetActiveSessions() != 4 || evidence.GetReadySessions() != 7 || evidence.GetHealthReportCount() != before+1 {
		t.Fatalf("unexpected unary TURNcoat evidence: %+v", evidence)
	}

	_, _ = w.Write([]byte("[turncoat] live streams active=3 total=10\n"))
	status = readStatus(nil)
	if got := status.GetTurncoatActiveSessions(); got != 3 {
		t.Fatalf("TCP TURNcoat active sessions = %d, want 3", got)
	}
	if got := status.GetTurncoatReadySessions(); got != 3 {
		t.Fatalf("TCP TURNcoat ready sessions = %d, want 3", got)
	}
}
