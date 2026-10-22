package hcore

import (
	"strings"
	"testing"
)

// This source-level guard deliberately covers lifecycle ordering rather than
// constructing a real Android TUN. The latter would require a device-owned fd
// and cannot prove that no inbound was opened before the selected route had a
// fresh successful probe result.
func TestAndroidStartPreflightsSelectedRouteBeforeOpeningInbounds(t *testing.T) {
	source := readHcoreSource(t, "start.go")
	start := sourceSection(t, source, "func startService", "\nfunc currentStartRejectionResponse")

	publish := strings.Index(start, "static.Box = instance")
	preStart := strings.Index(start, "instance.PreStart()")
	preflight := strings.Index(start, "preflightSelectedRouteBeforeAndroidTun(startCtx, instance)")
	startInbounds := strings.Index(start, "instance.StartAfterPreStart()")
	if publish < 0 || preStart < publish || preflight < preStart || startInbounds < preflight {
		t.Fatal("Android startup must publish one exact Box, pre-start router/outbounds, preflight the selected route, then open inbounds")
	}

	androidStart := sourceSection(t, start, "if runtime.GOOS == \"android\" && hasTunInbound(options)", "\tif instanceStartErr != nil {")
	if !strings.Contains(androidStart, "instanceStartErr = instance.PreStart()") ||
		!strings.Contains(androidStart, "instanceStartErr = preflightSelectedRouteBeforeAndroidTun(startCtx, instance)") ||
		!strings.Contains(androidStart, "instanceStartErr = instance.StartAfterPreStart()") {
		t.Fatal("Android TUN startup must stop at the first failed split-lifecycle phase")
	}
	failure := sourceSection(t, start, "if instanceStartErr != nil {", "\n\tstatic.lock.Lock()\n\tif static.Box == instance")
	if !strings.Contains(failure, "closeStartInstance(instance, \"failed start\")") {
		t.Fatal("a cancelled or failed startup preflight must close the published exact Box before returning")
	}
}

func TestSelectedRouteStartupPreflightRequiresFreshUsableLatency(t *testing.T) {
	source := readHcoreSource(t, "start.go")
	preflight := sourceSection(t, source, "func preflightSelectedRouteBeforeAndroidTun", "\nfunc logCoreStartState")

	if !strings.Contains(preflight, "config.OutboundSelectTag") ||
		!strings.Contains(preflight, "probeSelectedRouteOnBox(") ||
		!strings.Contains(preflight, "static.Box == instance && static.CoreState == CoreStates_STARTING") {
		t.Fatal("startup preflight must probe the selected route on the exact STARTING Box")
	}
	usable := sourceSection(t, source, "func isUsableSelectedRoutePreflight", "\nfunc preflightSelectedRouteBeforeAndroidTun")
	if !strings.Contains(usable, "result.UrlTestTime != nil") ||
		!strings.Contains(usable, "result.UrlTestDelay > 0") ||
		!strings.Contains(usable, "result.UrlTestDelay < selectedRouteTimeoutDelay") ||
		!strings.Contains(source, "selectedRouteTimeoutDelay    = int32(1<<16 - 1)") {
		t.Fatal("only a fresh selected-route latency in (0, 65535) may admit Android TUN startup")
	}
	commands := readHcoreSource(t, "commands.go")
	probe := sourceSection(t, commands, "func probeSelectedRouteOnBox", "\nfunc (s *CoreService) UrlTest")
	mobileProbe := sourceSection(t, probe, "if runtime.GOOS == \"ios\"", "\n\toperationCtx, cancel := selectedRouteOperationContext")
	legacyProbe := sourceSection(t, probe, "operationCtx, cancel := selectedRouteOperationContext", "\n}\n\nfunc selectedOutboundTypeOnBox")
	if !strings.Contains(mobileProbe, "probeAndroidSelectedRoute") ||
		!strings.Contains(mobileProbe, "if err := ctx.Err(); err != nil") ||
		!strings.Contains(mobileProbe, "currentOutbound != selectedOutbound") ||
		!strings.Contains(mobileProbe, "historyStorage.StoreURLTestHistory") {
		t.Fatal("iOS and Android VLESS preflight must honor cancellation and verify the current selected outbound before publishing history")
	}
	if !strings.Contains(legacyProbe, "history.Time.Before(probeStartedAt)") {
		t.Fatal("legacy outbound preflight must reject a cached selected-route result")
	}
}

func TestFailedSplitStartCleanupWrapsTheActualInstanceStartError(t *testing.T) {
	source := readHcoreSource(t, "start.go")
	start := sourceSection(t, source, "func startService", "\nfunc currentStartRejectionResponse")
	failure := sourceSection(t, start, "if instanceStartErr != nil {", "\n\tstatic.lock.Lock()\n\tif static.Box == instance")

	if !strings.Contains(failure, "fmt.Errorf(\"failed start cleanup did not finish: %w\", instanceStartErr)") {
		t.Fatal("failed split-start cleanup must preserve the PreStart/preflight/StartAfterPreStart error")
	}
	if strings.Contains(failure, "fmt.Errorf(\"failed start cleanup did not finish: %w\", err)") {
		t.Fatal("failed split-start cleanup must not wrap the stale outer error")
	}
}
