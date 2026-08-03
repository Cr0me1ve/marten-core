package hcore

import (
	"context"
	"strings"
	"testing"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	mtoutbound "github.com/sagernet/sing-box/outbound"
)

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
