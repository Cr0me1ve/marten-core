package hcore

// Marten captcha bridge.
//
// The TURNcoat dialer (github.com/cacggghp/turncoat/dialer) raises a captcha
// challenge by opening a local HTTP server on 127.0.0.1:8765 and offering two
// optional hooks for the embedder: a "captcha required" callback that yields
// the URL the user should solve, and a "captcha resolved" callback that fires
// once the proxy has accepted a token. Without those hooks installed the
// dialer falls back to openBrowser(), which is unreliable inside iOS and
// Android app sandboxes.
//
// To get a proper in-app UX, marten-sing-box exposes process-wide setters
// (SetTurncoatCaptchaHandler / SetTurncoatCaptchaResolvedHandler). We install
// handlers at Setup() that emit two stable, Marten-namespaced log markers the
// Dart layer scans for:
//
//   - MARTEN_TURNCOAT_CAPTCHA url=<local-captcha-url>      → open WebView
//   - MARTEN_TURNCOAT_CAPTCHA_DONE                         → close WebView
//
// The markers are Marten's own log protocol, not part of TURNcoat's public
// surface, so renaming or extending them doesn't ripple into the upstream
// dialer.
//
// As a diagnostics bonus we also pipe Go's stdlib log output through the
// same observer, so the dialer's plain log.Printf() calls reach the Flutter
// log stream instead of disappearing into stderr.

import (
	"bytes"
	"fmt"
	stdlog "log"
	"strings"
	"sync/atomic"

	mtoutbound "github.com/sagernet/sing-box/outbound"
)

// LogTurncoatCaptchaMarker is the stable line-prefix the Dart layer scans for
// to detect that a TURNcoat outbound needs the user to solve a captcha.
//
// Full line format:
//
//	MARTEN_TURNCOAT_CAPTCHA url=<local-captcha-url>
//
// Where <local-captcha-url> is an http://127.0.0.1:8765/... URL that the
// Marten app should display in an in-app WebView. The token round-trip is
// handled by the dialer's local HTTP server — the UI just needs to host the
// page.
const LogTurncoatCaptchaMarker = "MARTEN_TURNCOAT_CAPTCHA"

// LogTurncoatCaptchaResolvedMarker is emitted once the captcha proxy has
// accepted a solution token. The Dart layer uses it to dismiss the WebView
// it pushed in response to LogTurncoatCaptchaMarker.
const LogTurncoatCaptchaResolvedMarker = "MARTEN_TURNCOAT_CAPTCHA_DONE"

var captchaBridgeInstalled atomic.Bool
var turncoatRXProofCount atomic.Int64
var turncoatActiveSessions atomic.Int32
var turncoatReadySessions atomic.Int32
var turncoatHealthReportCount atomic.Int64

const turncoatRXProofMarker = "promoted to active TX pool (RX proof received)"

func resetTurncoatRouteEvidence() {
	turncoatRXProofCount.Store(0)
	turncoatActiveSessions.Store(0)
	turncoatReadySessions.Store(0)
	turncoatHealthReportCount.Store(0)
}

// installCaptchaBridge wires the marten-sing-box turncoat outbound's captcha
// hooks to push structured log lines into the Marten log stream, and pipes
// stdlib log output (used by the dialer package) through the same stream
// for general diagnostics.
//
// Idempotent. Called from Setup().
func installCaptchaBridge() {
	if !captchaBridgeInstalled.CompareAndSwap(false, true) {
		return
	}

	mtoutbound.SetTurncoatCaptchaHandler(func(captchaURL string) {
		Log(LogLevel_WARNING, LogType_SERVICE, "captcha handler fired url="+captchaURL)
		Log(LogLevel_WARNING, LogType_SERVICE, LogTurncoatCaptchaMarker+" url="+captchaURL)
	})
	mtoutbound.SetTurncoatCaptchaResolvedHandler(func() {
		Log(LogLevel_INFO, LogType_SERVICE, "captcha resolved")
		Log(LogLevel_INFO, LogType_SERVICE, LogTurncoatCaptchaResolvedMarker)
	})

	stdlog.SetFlags(0)
	stdlog.SetOutput(&stdlogToObserver{})

	Log(LogLevel_INFO, LogType_SERVICE, "captcha bridge installed (markers="+LogTurncoatCaptchaMarker+"/"+LogTurncoatCaptchaResolvedMarker+")")
}

// stdlogToObserver routes Go stdlib log.Printf output into the marten-core
// log observable (so it flows to the Flutter UI via the gRPC log stream).
//
// Each Write may contain multiple newline-separated records or a partial
// record. We split on '\n' and emit each non-empty line as its own LogMessage.
type stdlogToObserver struct{}

func (s *stdlogToObserver) Write(p []byte) (int, error) {
	// log.Printf always appends a trailing '\n', but we strip aggressively to
	// avoid blank lines if the caller already included one.
	for _, line := range bytes.Split(p, []byte{'\n'}) {
		if msg := strings.TrimSpace(string(line)); msg != "" {
			if strings.Contains(msg, turncoatRXProofMarker) {
				turncoatRXProofCount.Add(1)
			}
			if active, ready, ok := parseTurncoatHealthReport(msg); ok {
				turncoatActiveSessions.Store(int32(active))
				turncoatReadySessions.Store(int32(ready))
				turncoatHealthReportCount.Add(1)
			}
			Log(LogLevel_INFO, LogType_SERVICE, msg)
		}
	}
	return len(p), nil
}

func parseTurncoatHealthReport(message string) (active, ready int, ok bool) {
	var total int
	if n, _ := fmt.Sscanf(message, "[turncoat] live streams active=%d ready=%d total=%d", &active, &ready, &total); n == 3 {
		return active, ready, true
	}
	if n, _ := fmt.Sscanf(message, "[turncoat] live streams active=%d total=%d", &active, &total); n == 2 {
		return active, active, true
	}
	return 0, 0, false
}
