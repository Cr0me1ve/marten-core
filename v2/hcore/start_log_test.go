package hcore

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/sagernet/sing-box/option"
)

func TestApplyRuntimeDebugLogLevelLeavesOptionsUnchangedWhenDebugDisabled(t *testing.T) {
	original := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(original) })
	SetRuntimeDebug(false)

	logOptions := &option.LogOptions{Disabled: true, Level: "error"}
	options := &option.Options{Log: logOptions}

	applyRuntimeDebugLogLevel(options, false)

	if options.Log != logOptions || options.Log.Disabled != true || options.Log.Level != "error" {
		t.Fatalf("debug=false mutated log options: %#v", options.Log)
	}
}

func TestApplyRuntimeDebugLogLevelCreatesAndEnablesDebugLogOptions(t *testing.T) {
	original := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(original) })
	SetRuntimeDebug(true)

	options := &option.Options{}

	applyRuntimeDebugLogLevel(options, RuntimeDebugEnabled())

	if options.Log == nil {
		t.Fatal("debug=true did not create LogOptions")
	}
	if options.Log.Disabled || options.Log.Level != "debug" {
		t.Fatalf("new LogOptions = %#v, want enabled debug", options.Log)
	}
}

func TestApplyRuntimeDebugLogLevelPromotesRestrictiveLevels(t *testing.T) {
	original := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(original) })
	SetRuntimeDebug(true)

	for _, level := range []string{"warn", "error"} {
		t.Run(level, func(t *testing.T) {
			options := &option.Options{Log: &option.LogOptions{Disabled: true, Level: level}}

			applyRuntimeDebugLogLevel(options, RuntimeDebugEnabled())

			if options.Log.Disabled || options.Log.Level != "debug" {
				t.Fatalf("level %q became %#v, want enabled debug", level, options.Log)
			}
		})
	}
}

func TestApplyRuntimeDebugLogLevelPreservesVerboseAndEmptyLevels(t *testing.T) {
	original := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(original) })
	SetRuntimeDebug(true)

	for _, level := range []string{"", "trace", "debug"} {
		t.Run("level="+level, func(t *testing.T) {
			options := &option.Options{Log: &option.LogOptions{Disabled: true, Level: level}}

			applyRuntimeDebugLogLevel(options, RuntimeDebugEnabled())

			if options.Log.Disabled || options.Log.Level != level {
				t.Fatalf("level %q became %#v, want enabled with level unchanged", level, options.Log)
			}
		})
	}
}

func TestSetRuntimeDebugTogglesRuntimeState(t *testing.T) {
	original := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(original) })

	SetRuntimeDebug(false)
	if RuntimeDebugEnabled() {
		t.Fatalf("runtime debug enabled after disable")
	}

	SetRuntimeDebug(true)
	if !RuntimeDebugEnabled() {
		t.Fatalf("runtime debug disabled after enable")
	}

	SetRuntimeDebug(false)
	if RuntimeDebugEnabled() {
		t.Fatalf("runtime debug still enabled after disable")
	}
}

func TestApplyRuntimeDebugLogLevelDependsOnRuntimeDebugState(t *testing.T) {
	original := RuntimeDebugEnabled()
	t.Cleanup(func() { SetRuntimeDebug(original) })

	SetRuntimeDebug(false)
	options := &option.Options{Log: &option.LogOptions{Disabled: true, Level: "error"}}
	applyRuntimeDebugLogLevel(options, RuntimeDebugEnabled())
	if options.Log.Disabled != true || options.Log.Level != "error" {
		t.Fatalf("runtime debug disabled should not mutate warn/error level, got %#v", options.Log)
	}

	SetRuntimeDebug(true)
	applyRuntimeDebugLogLevel(options, RuntimeDebugEnabled())
	if options.Log.Disabled {
		t.Fatalf("runtime debug enabled should make logs enabled")
	}
	if options.Log.Level != "debug" {
		t.Fatalf("runtime debug enabled should promote warn/error level to debug, got %q", options.Log.Level)
	}
}

func TestRuntimeStateAndCoreStartLogAreSecretFreeSummaries(t *testing.T) {
	originalRuntimeDebug := RuntimeDebugEnabled()
	originalOptions := static.MartenOptions
	originalPlatformInterface := static.globalPlatformInterface
	originalReady := static.settingsReady.Load()
	originalLastConfigHasTun := static.lastConfigHasTun.Load()
	originalLastConfigInboundCount := static.lastConfigInboundCount.Load()
	static.lock.Lock()
	originalCoreState := static.CoreState
	static.CoreState = CoreStates_STARTING
	static.lock.Unlock()
	t.Cleanup(func() {
		SetRuntimeDebug(originalRuntimeDebug)
		static.MartenOptions = originalOptions
		static.globalPlatformInterface = originalPlatformInterface
		static.settingsReady.Store(originalReady)
		static.lastConfigHasTun.Store(originalLastConfigHasTun)
		static.lastConfigInboundCount.Store(originalLastConfigInboundCount)
		static.lock.Lock()
		static.CoreState = originalCoreState
		static.lock.Unlock()
	})

	static.MartenOptions = config.DefaultMartenOptions()
	static.MartenOptions.EnableTun = true
	static.globalPlatformInterface = &MobilePlatformInterface{}
	static.settingsReady.Store(true)
	static.lastConfigHasTun.Store(true)
	static.lastConfigInboundCount.Store(2)
	SetRuntimeDebug(true)

	const secretAddress = "sensitive.example.internal"
	const secretTag = "profile-secret-token"
	options := &option.Options{Inbounds: []option.Inbound{{Type: "tun", Tag: secretTag}, {Type: "mixed", Tag: secretAddress}}}

	runtimeState := RuntimeState()
	const runtimePattern = `^debug=(true|false) settings_ready=(true|false) enable_tun=(true|false) platform_interface=(true|false) core_state=[A-Z_]+ last_config_tun=(true|false) last_config_inbounds=\d+$`
	if !regexp.MustCompile(runtimePattern).MatchString(runtimeState) {
		t.Fatalf("unexpected runtime state: %q", runtimeState)
	}
	assertNoSecretSummary(t, runtimeState, secretAddress, secretTag)

	logSub, stopch, _ := static.logObserver.Subscribe()
	t.Cleanup(func() { static.logObserver.UnSubscribe(logSub) })
	logCoreStartState(options)

	select {
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for core start state log")
	case <-stopch:
		t.Fatal("log observer stopped before core start state message arrived")
	case message := <-logSub:
		const startPattern = `^core start state debug=(true|false) settings_ready=(true|false) enable_tun=(true|false) tun_inbound=(true|false) platform_interface=(true|false)$`
		if !regexp.MustCompile(startPattern).MatchString(message.GetMessage()) {
			t.Fatalf("unexpected core start state: %q", message.GetMessage())
		}
		assertNoSecretSummary(t, message.GetMessage(), secretAddress, secretTag)
	}
}

func assertNoSecretSummary(t *testing.T, summary string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(summary, value) {
			t.Fatalf("summary leaked sensitive value %q: %q", value, summary)
		}
	}
}
