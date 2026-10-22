package hcore

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
)

type countingString struct {
	value string
	calls atomic.Int64
}

func (c *countingString) String() string {
	c.calls.Add(1)
	return c.value
}

func withIsolatedLogObserver(t *testing.T, fn func(logSub <-chan *LogMessage)) {
	t.Helper()

	originalObserver := static.logObserver
	isolatedObserver := NewObserver[*LogMessage](8)
	static.logObserver = isolatedObserver
	logSub, stopch, err := static.logObserver.Subscribe()
	if err != nil {
		t.Fatalf("subscribe static log observer: %v", err)
	}
	defer func() {
		static.logObserver.UnSubscribe(logSub)
		_ = static.logObserver.Close()
		static.logObserver = originalObserver
	}()

	select {
	case <-stopch:
		t.Fatal("log observer stopped before test started")
	default:
	}

	fn(logSub)
}

func receiveLogMessage(t *testing.T, logSub <-chan *LogMessage) *LogMessage {
	t.Helper()
	select {
	case msg := <-logSub:
		return msg
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for log observer message")
	}
	return nil
}

func assertStdErrTimestampMatchesObserver(t *testing.T, line string, message *LogMessage) {
	t.Helper()

	trimmedLine := strings.TrimSpace(line)
	if trimmedLine == "" {
		t.Fatalf("stderr line is empty")
	}

	lineEnd := strings.IndexByte(trimmedLine, '\n')
	if lineEnd != -1 {
		trimmedLine = trimmedLine[:lineEnd]
	}

	fields := strings.Fields(trimmedLine)
	if len(fields) < 1 {
		t.Fatalf("stderr line is malformed: %q", trimmedLine)
	}

	observedTs, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		t.Fatalf("failed to parse first token as RFC3339Nano: %q (%v)", fields[0], err)
	}

	want := message.GetTime().AsTime()
	if !observedTs.UTC().Equal(want.UTC()) {
		t.Fatalf("stderr timestamp = %s, observer timestamp = %s", observedTs.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func assertObserverEvent(t *testing.T, logSub <-chan *LogMessage, wantLevel LogLevel, wantType LogType, wantMessage string) *LogMessage {
	t.Helper()
	message := receiveLogMessage(t, logSub)
	if got := message.GetLevel(); got != wantLevel {
		t.Fatalf("logged level = %v, want %v", got, wantLevel)
	}
	if got := message.GetType(); got != wantType {
		t.Fatalf("logged type = %v, want %v", got, wantType)
	}
	if got := message.GetMessage(); got != wantMessage {
		t.Fatalf("logged message = %q, want %q", got, wantMessage)
	}
	if message.GetTime() == nil || message.GetTime().AsTime().IsZero() {
		t.Fatalf("logged time is not set: %v", message.GetTime())
	}
	if ts := message.GetTime().AsTime(); ts.After(time.Now().Add(time.Second)) {
		t.Fatalf("logged time in the future: %v", ts)
	}

	select {
	case extra := <-logSub:
		t.Fatalf("expected one observer event, extra message: %q", extra.GetMessage())
	case <-time.After(5 * time.Millisecond):
	}

	return message
}

func captureLogStreams(t *testing.T, loggerWriter io.Writer, customStderr *os.File, captureStderrOutput bool, fn func()) (stdout string, stderr string) {
	t.Helper()

	originalStdout := os.Stdout
	originalStderr := os.Stderr
	originalLogger := log.StdLogger()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}

	captureStderrPipe := customStderr == nil
	var stderrR *os.File
	stderrW := customStderr
	if captureStderrPipe {
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			t.Fatalf("create stderr pipe: %v", err)
		}
	}

	if loggerWriter == nil {
		loggerWriter = stderrW
	}

	baseFormatter := log.Formatter{BaseTime: time.Now()}
	loggerFactory := log.NewDefaultFactory(context.Background(), baseFormatter, loggerWriter, "", nil, false)
	loggerFactory.SetLevel(log.LevelTrace)

	os.Stdout = stdoutW
	os.Stderr = stderrW
	log.SetStdLogger(loggerFactory.Logger())

	defer func() {
		os.Stdout = originalStdout
		os.Stderr = originalStderr
		log.SetStdLogger(originalLogger)
		_ = stdoutW.Close()
		_ = stdoutR.Close()
		if captureStderrPipe {
			_ = stderrW.Close()
			_ = stderrR.Close()
		}
	}()

	fn()

	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close mocked stdout writer: %v", err)
	}
	if captureStderrPipe {
		if err := stderrW.Close(); err != nil {
			t.Fatalf("close mocked stderr writer: %v", err)
		}
	}

	var stdoutBuf bytes.Buffer
	if _, err := io.Copy(&stdoutBuf, stdoutR); err != nil {
		t.Fatalf("read mocked stdout: %v", err)
	}
	if captureStderrOutput && captureStderrPipe {
		var stderrBuf bytes.Buffer
		if _, err := io.Copy(&stderrBuf, stderrR); err != nil {
			t.Fatalf("read mocked stderr: %v", err)
		}
		stderr = stderrBuf.String()
	}

	stdout = stdoutBuf.String()
	return stdout, stderr
}

func captureStdStreams(t *testing.T, fn func()) (stdout string, stderr string) {
	return captureLogStreams(t, nil, nil, true, fn)
}

func runWithBoundedStreamSinks(fn func()) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	originalStdout := os.Stdout
	originalStderr := os.Stderr
	originalLogger := log.StdLogger()

	stdoutDrained := make(chan struct{}, 1)
	stderrDrained := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(io.Discard, stdoutR)
		stdoutDrained <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(io.Discard, stderrR)
		stderrDrained <- struct{}{}
	}()

	loggerFactory := log.NewDefaultFactory(context.Background(), log.Formatter{BaseTime: time.Now()}, stderrW, "", nil, false)
	loggerFactory.SetLevel(log.LevelTrace)

	os.Stdout = stdoutW
	os.Stderr = stderrW
	log.SetStdLogger(loggerFactory.Logger())

	defer func() {
		os.Stdout = originalStdout
		os.Stderr = originalStderr
		log.SetStdLogger(originalLogger)
		_ = stdoutW.Close()
		_ = stderrW.Close()
		<-stdoutDrained
		<-stderrDrained
		_ = stdoutR.Close()
		_ = stderrR.Close()
	}()

	fn()
}

func TestLogObserverReceivesDebugMessagesWhenConsoleIsSuppressed(t *testing.T) {
	originalDebug := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(originalDebug) })

	withIsolatedLogObserver(t, func(logSub <-chan *LogMessage) {
		SetRuntimeDebug(false)

		const payload = "runtime-debug-disabled debug-level log should still be observable"
		stdout, stderr := captureStdStreams(t, func() {
			Log(LogLevel_DEBUG, LogType_CORE, payload)
		})

		if stdout != "" {
			t.Fatalf("stdout should not be written by Log when static.debug=false and level=DEBUG: %q", stdout)
		}
		if stderr != "" {
			t.Fatalf("stderr should not be written by Log when static.debug=false and level=DEBUG: %q", stderr)
		}

		assertObserverEvent(t, logSub, LogLevel_DEBUG, LogType_CORE, payload)
	})
}

func TestLogFormatsArgumentsOnceAndWritesSingleStderrLine(t *testing.T) {
	originalDebug := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(originalDebug) })

	withIsolatedLogObserver(t, func(logSub <-chan *LogMessage) {
		SetRuntimeDebug(false)

		counter := &countingString{value: "fresh-probe-payload"}
		stdout, stderr := captureStdStreams(t, func() {
			Log(LogLevel_WARNING, LogType_SERVICE, "test=", counter)
		})

		if got := counter.calls.Load(); got != 1 {
			t.Fatalf("String() calls = %d, want 1", got)
		}

		if stdout != "" {
			t.Fatalf("stdout should not be written by Log once sing-box logger is isolated: %q", stdout)
		}
		if !strings.Contains(stderr, "test=fresh-probe-payload") {
			t.Fatalf("stderr payload missing expected log line: %q", stderr)
		}
		if strings.Count(stderr, "\n") != 1 {
			t.Fatalf("expected exactly one stderr line for one Log call, got %q", stderr)
		}

		message := assertObserverEvent(t, logSub, LogLevel_WARNING, LogType_SERVICE, "test=fresh-probe-payload")
		assertStdErrTimestampMatchesObserver(t, stderr, message)
	})
}

func TestLogCoreStderrTimestampMatchesObserverTime(t *testing.T) {
	originalDebug := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(originalDebug) })

	withIsolatedLogObserver(t, func(logSub <-chan *LogMessage) {
		SetRuntimeDebug(false)

		counter := &countingString{value: "core-timestamp-check"}
		stdout, stderr := captureStdStreams(t, func() {
			Log(LogLevel_WARNING, LogType_CORE, "core-", counter)
		})

		if got := counter.calls.Load(); got != 1 {
			t.Fatalf("String() calls = %d, want 1", got)
		}

		if stdout != "" {
			t.Fatalf("stdout should not be written by Log once sing-box logger is isolated: %q", stdout)
		}
		if !strings.Contains(stderr, "core-core-timestamp-check") {
			t.Fatalf("stderr payload missing expected log line: %q", stderr)
		}
		if strings.Count(stderr, "\n") != 1 {
			t.Fatalf("expected exactly one stderr line for one Log call, got %q", stderr)
		}

		message := assertObserverEvent(t, logSub, LogLevel_WARNING, LogType_CORE, "core-core-timestamp-check")
		assertStdErrTimestampMatchesObserver(t, stderr, message)
	})
}

func TestLogConsoleEmissionMatchesRuntimeDebugAndLevel(t *testing.T) {
	originalDebug := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(originalDebug) })

	for _, tc := range []struct {
		name         string
		debugEnabled bool
		level        LogLevel
		wantStderr   bool
	}{
		{"debug-disabled blocks debug console output", false, LogLevel_DEBUG, false},
		{"debug-disabled emits warning to stderr", false, LogLevel_WARNING, true},
		{"debug-enabled emits debug to stderr", true, LogLevel_DEBUG, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withIsolatedLogObserver(t, func(logSub <-chan *LogMessage) {
				SetRuntimeDebug(tc.debugEnabled)
				const payload = "runtime debug matrix"

				stdout, stderr := captureStdStreams(t, func() {
					Log(tc.level, LogType_SERVICE, payload)
				})
				if stdout != "" {
					t.Fatalf("stdout should be empty for captured sing-box logger and configured Log behavior, got %q", stdout)
				}

				switch {
				case tc.wantStderr:
					if strings.Count(stderr, "\n") != 1 {
						t.Fatalf("stderr expected one line, got %q", stderr)
					}
					if !strings.Contains(stderr, payload) {
						t.Fatalf("stderr payload missing expected log line: %q", stderr)
					}
					message := assertObserverEvent(t, logSub, tc.level, LogType_SERVICE, payload)
					assertStdErrTimestampMatchesObserver(t, stderr, message)
					return
				case !tc.wantStderr:
					if stderr != "" {
						t.Fatalf("stderr should be empty for suppressed debug output, got %q", stderr)
					}
				}

				assertObserverEvent(t, logSub, tc.level, LogType_SERVICE, payload)
			})
		})
	}
}

func TestLogObserverDeliverySurvivesClosedStderr(t *testing.T) {
	originalDebug := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(originalDebug) })

	withIsolatedLogObserver(t, func(logSub <-chan *LogMessage) {
		SetRuntimeDebug(false)

		_, closedStderr, err := os.Pipe()
		if err != nil {
			t.Fatalf("create closed stderr sink: %v", err)
		}
		if err := closedStderr.Close(); err != nil {
			t.Fatalf("close stderr sink: %v", err)
		}

		captureLogStreams(t, closedStderr, closedStderr, false, func() {
			Log(LogLevel_WARNING, LogType_SERVICE, "closed stderr still observes")
		})

		message := assertObserverEvent(t, logSub, LogLevel_WARNING, LogType_SERVICE, "closed stderr still observes")
		if message.GetMessage() != "closed stderr still observes" {
			t.Fatalf("unexpected observed payload after write failure: %q", message.GetMessage())
		}
	})
}

func benchmarkCoreLogEnergy(b *testing.B, debug bool) {
	originalDebug := RuntimeDebugEnabled()
	originalObserver := static.logObserver
	benchObserver := NewObserver[*LogMessage](8)
	logPayload := "energy-benchmark-log-message"

	defer func() {
		_ = benchObserver.Close()
		static.logObserver = originalObserver
		SetRuntimeDebug(originalDebug)
	}()

	SetRuntimeDebug(debug)
	static.logObserver = benchObserver

	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	defer b.StartTimer()

	runWithBoundedStreamSinks(func() {
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			Log(LogLevel_DEBUG, LogType_SERVICE, logPayload)
		}
		b.StopTimer()
	})
}

func BenchmarkCoreLogEnergy(b *testing.B) {
	b.Run("runtime_debug=true", func(b *testing.B) {
		benchmarkCoreLogEnergy(b, true)
	})
	b.Run("runtime_debug=false", func(b *testing.B) {
		benchmarkCoreLogEnergy(b, false)
	})
}
