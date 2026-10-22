package hcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedStartReattachesOnlyWhenExactCloseFails(t *testing.T) {
	source := readHcoreSource(t, "start.go")
	failedStart := sourceSection(t, source, "if instanceStartErr != nil {", "\n\tstatic.lock.Lock()\n\tif static.Box == instance && static.CoreState == CoreStates_STARTING {")
	closeResult := strings.Index(failedStart, "closed := closeStartInstance(instance, \"failed start\")")
	if closeResult < 0 {
		t.Fatal("failed-start cleanup must retain the exact Box close result")
	}
	reattach := strings.Index(failedStart, "reattachLeakedStartInstance(instance, localListeners, \"failed start cleanup incomplete\")")
	if reattach < 0 {
		t.Fatal("failed-start cleanup must retain an instance only after an incomplete close")
	}
	guard := strings.LastIndex(failedStart[:reattach], "if !closed {")
	if guard < closeResult {
		t.Fatal("failed-start reattach is not guarded by the exact close result")
	}
	decision := failedStart[closeResult:reattach]
	if strings.Contains(decision, "waitForLocalListeners(") || strings.Contains(decision, "unavailableLocalListeners(") {
		t.Fatal("failed-start ownership must not be inferred from globally busy configured listeners")
	}
}

func TestPostStartSuccessRequiresThePublishedInstanceAndStartingState(t *testing.T) {
	source := readHcoreSource(t, "start.go")
	postStart := sourceSection(t, source, "\n\tstatic.lock.Lock()\n\tif static.Box == instance && static.CoreState == CoreStates_STARTING", "\nfunc cleanupPlaintextRuntimeConfig")
	if !strings.Contains(postStart, "if static.Box == instance && static.CoreState == CoreStates_STARTING {") {
		t.Fatal("post-start success must require the published Box and STARTING state together")
	}
	if !strings.Contains(postStart, "response := setCoreStatusLocked(CoreStates_STARTED, MessageType_EMPTY, \"\")") ||
		!strings.Contains(postStart, "static.lock.Unlock()\n\t\treturn response, nil") {
		t.Fatal("post-start Box/state guard must run under the static lifecycle lock")
	}
	if strings.Count(postStart, "CoreStates_STARTED") != 1 ||
		!strings.Contains(postStart, "CoreStates_STOPPING,") ||
		!strings.Contains(postStart, "start cancellation awaiting exact close") {
		t.Fatal("late cancellation after instance.Start must remain STOPPING without republishing STARTED")
	}
	if !strings.Contains(postStart, "owned := static.Box == instance") ||
		strings.Contains(postStart, "static.Box = nil") ||
		strings.Contains(postStart, "static.activeLocalListeners = nil") {
		t.Fatal("late cancellation must retain the same Box and listener ownership for exact Stop/Mobile.Close")
	}
}

func TestAuthoritativeStartAndStopTransitionsUseLockedWriterAndPreserveNewerGeneration(t *testing.T) {
	startSource := readHcoreSource(t, "start.go")
	start := sourceSection(t, startSource, "func startService", "\nfunc currentStartRejectionResponse")
	if strings.Contains(start, "SetCoreStatus(") {
		t.Fatal("start lifecycle transaction must not relock and republish through SetCoreStatus")
	}
	starting := strings.Index(start, "setCoreStatusLocked(CoreStates_STARTING, MessageType_EMPTY, \"\")")
	startUnlock := starting + strings.Index(start[starting:], "static.lock.Unlock()")
	started := strings.Index(start, "setCoreStatusLocked(CoreStates_STARTED, MessageType_EMPTY, \"\")")
	if starting < 0 || startUnlock < starting || started < startUnlock {
		t.Fatal("start must publish STARTING and STARTED through the lock-owned lifecycle writer")
	}

	cleanup := sourceSection(t, startSource, "func publishStartCleanupFailure", "\n}")
	if !strings.Contains(cleanup, "static.lock.Lock()") ||
		!strings.Contains(cleanup, "if static.Box == instance {") ||
		!strings.Contains(cleanup, "return setCoreStatusLocked(CoreStates_STOPPING") ||
		!strings.Contains(cleanup, "return coreInfoResponse(static.CoreState") {
		t.Fatal("stale start cleanup may publish only for its exact Box; a newer generation must receive a pure snapshot")
	}

	stopSource := readHcoreSource(t, "stop.go")
	stop := sourceSection(t, stopSource, "func stopOnce()", "\nfunc waitForInFlightStop")
	if strings.Contains(stop, "SetCoreStatus(") {
		t.Fatal("stop cleanup must not use an out-of-lock mutating SetCoreStatus writer")
	}
	if !strings.Contains(stop, "if static.Box == box {") ||
		!strings.Contains(stop, "response := setCoreStatusLocked(CoreStates_STOPPED") ||
		!strings.Contains(stop, "response := coreInfoResponse(static.CoreState") {
		t.Fatal("stop must publish STOPPED only for the closed exact Box and snapshot a replacement generation")
	}
}

func TestStopReleasesInstanceImmediatelyAfterExactCloseWithoutUnboundedListenerWait(t *testing.T) {
	source := readHcoreSource(t, "stop.go")
	stop := sourceSection(t, source, "func stopOnce()", "\nfunc waitForInFlightStop")
	snapshotBox := strings.Index(stop, "box := static.Box")
	exactClose := strings.Index(stop, "err = closeBoxExact(box)")
	clearBox := strings.Index(stop, "static.Box = nil")
	clearListeners := strings.Index(stop, "static.activeLocalListeners = nil")
	if snapshotBox < 0 || exactClose < snapshotBox || clearBox < exactClose || clearListeners < clearBox {
		t.Fatal("stop must retain ownership through exact Box.Close then clear Box/listener metadata")
	}
	if strings.Contains(stop, "waitForLocalListeners(") || strings.Contains(stop, "unavailableLocalListeners(") {
		t.Fatal("Stop must not wait for local listener availability after Box.Close")
	}
	if !strings.Contains(stop[exactClose:clearBox], "Listener availability is a next-generation admission concern") {
		t.Fatal("stop must document why listener admission cannot hold native close indefinitely")
	}
}

func TestStopOnceWaitsForInFlightStartExactlyBeforeClosingBox(t *testing.T) {
	source := readHcoreSource(t, "stop.go")
	stop := sourceSection(t, source, "func stopOnce()", "\nfunc waitForInFlightStop")
	startDone := strings.Index(stop, "startDone := static.startDone")
	exactWait := strings.Index(stop, "waitForInFlightStartExact(startDone)")
	exactClose := strings.Index(stop, "err = closeBoxExact(box)")
	if startDone < 0 || exactWait < startDone || exactClose < exactWait {
		t.Fatal("stopOnce must join the in-flight Start exactly before exact Box.Close")
	}
	if strings.Contains(stop, "waitForInFlightStart(startDone,") ||
		strings.Contains(stop[exactWait:exactClose], "30*time.Second") ||
		strings.Contains(stop[exactWait:exactClose], "12*time.Second") {
		t.Fatal("stopOnce must not give the exact Mobile.Close owner a bounded Start wait")
	}
	if !strings.Contains(source, "coreStopCoordinator.run") ||
		!strings.Contains(source, "stopCloseGracePeriod") {
		t.Fatal("only the public stop coordinator may retain the bounded caller wait")
	}
}

func TestTimedOutStartCleanupReattachesForLaterExactClose(t *testing.T) {
	start := readHcoreSource(t, "start.go")
	closeStart := sourceSection(t, start, "func closeStartInstance", "\nfunc reattachLeakedStartInstance")
	if !strings.Contains(closeStart, "closeBoxWithGrace(instance, reason, startCleanupCloseGracePeriod)") {
		t.Fatal("start cleanup must retain its bounded diagnostic close attempt")
	}
	if !strings.Contains(start, "if !closed {") ||
		!strings.Contains(start, "reattachLeakedStartInstance(instance, localListeners") {
		t.Fatal("a timed-out start cleanup must reattach the exact Box instead of dropping ownership")
	}

	stop := sourceSection(t, readHcoreSource(t, "stop.go"), "func stopOnce()", "\nfunc waitForInFlightStop")
	if !strings.Contains(stop, "err = closeBoxExact(box)") {
		t.Fatal("a reattached start Box must be joined by stopOnce through closeBoxExact")
	}
}

func TestCloseSealsStartAdmissionUntilSuccessfulSetup(t *testing.T) {
	closeSource := readHcoreSource(t, "custom.go")
	closeCore := sourceSection(t, closeSource, "func closeCore", "\n// func")
	seal := strings.Index(closeCore, "sealStartAdmissionForClose()")
	exactStop := strings.Index(closeCore, "stopForClose()")
	if seal < 0 || exactStop < seal {
		t.Fatal("native Close must seal Start admission before it joins exact stop")
	}

	startSource := readHcoreSource(t, "start.go")
	start := sourceSection(t, startSource, "func startService", "\nfunc waitForPreviousStartToFinish")
	lock := strings.Index(start, "static.lock.Lock()")
	sealedReject := strings.Index(start, "if static.startAdmissionClosed")
	register := strings.Index(start, "markStartInFlightLocked(cancelStart)")
	if lock < 0 || sealedReject < lock || register < sealedReject {
		t.Fatal("Start must reject sealed admission before registering an in-flight generation")
	}

	serviceSource := readHcoreSource(t, "service.go")
	setup := sourceSection(t, serviceSource, "func setupWithAdmission", "\nfunc setup(")
	success := strings.Index(setup, "if setupErr == nil {")
	open := strings.Index(setup, "openStartAdmissionAfterSetup()")
	if success < 0 || open < success {
		t.Fatal("only successful Setup may reopen Start admission")
	}
}

func TestStartCompletionBarrierWaitsForPanicPublicationAndAdmission(t *testing.T) {
	// The panic hook has no injectable seam, and exercising a real failed
	// sing-box start would not deterministically panic after admission. Guard
	// the defer registration order directly: defers run LIFO, so the completion
	// barrier must be registered before the panic publisher.
	source := readHcoreSource(t, "start.go")
	start := sourceSection(t, source, "func startService", "\nfunc currentStartRejectionResponse")

	owned := strings.Index(start, "startInFlightOwned := false")
	completionDefer := strings.Index(start, "defer func() {\n\t\tif startInFlightOwned {\n\t\t\tfinishStartInFlight()")
	panicDefer := strings.Index(start, "defer config.DeferPanicToError(\"startmobile\"")
	panicPublication := strings.Index(start, "coreResponse, err = errorWrapper(MessageType_UNEXPECTED_ERROR, recovered_err)")
	admission := strings.Index(start, "markStartInFlightLocked(cancelStart)")
	claim := strings.Index(start, "startInFlightOwned = true")
	if owned < 0 || completionDefer < owned || panicDefer < completionDefer ||
		panicPublication < panicDefer || admission < panicPublication || claim < admission {
		t.Fatal("Start must install the guarded completion defer before panic publication, then claim it only after admission")
	}
	if strings.Count(start, "finishStartInFlight()") != 1 ||
		!strings.Contains(start[completionDefer:panicDefer], "if startInFlightOwned {") {
		t.Fatal("only an admitted Start may close its completion barrier")
	}
}

func TestRejectedStartResponsesArePureSnapshotsAndCannotRepublishOverStop(t *testing.T) {
	response := startResponse(CoreStates_STOPPING, MessageType_ALREADY_STARTED, "instance already started")
	if response.CoreState != CoreStates_STOPPING ||
		response.MessageType != MessageType_ALREADY_STARTED ||
		response.Message != "instance already started" {
		t.Fatal("startResponse must return the supplied lifecycle snapshot without deriving a new state")
	}

	source := readHcoreSource(t, "start.go")
	start := sourceSection(t, source, "func startService", "\nfunc currentStartRejectionResponse")
	preAdmission := sourceSection(t, start, "if err := waitForInFlightStopContext", "\tresetTurncoatRouteEvidence()")
	for _, rejection := range []struct {
		name   string
		marker string
	}{
		{"in-flight Stop wait", "if err := waitForInFlightStopContext"},
		{"previous Start wait", "if err := waitForPreviousStartToFinishContext"},
		{"request cancellation", "if requestErr := requestContext.Err(); requestErr != nil"},
		{"sealed admission", "if static.startAdmissionClosed"},
		{"already-started admission", "if static.CoreState != CoreStates_STOPPED"},
	} {
		if !strings.Contains(preAdmission, rejection.marker) {
			t.Fatalf("missing %s rejection branch", rejection.name)
		}
	}
	if strings.Contains(preAdmission, "SetCoreStatus(") {
		t.Fatal("wait/cancel/admission-closed/already-started rejections must not republish lifecycle state")
	}
	if !strings.Contains(preAdmission, "currentStartRejectionResponse(err)") ||
		!strings.Contains(preAdmission, "return startResponse(state, MessageType_START_SERVICE") ||
		!strings.Contains(preAdmission, "return startResponse(state, MessageType_ALREADY_STARTED") {
		t.Fatal("rejected Start paths must return pure current/start responses")
	}

	listenerWait := sourceSection(t, start, "if err := waitForLocalListeners(", "\tbopts := box.Options{")
	if !strings.Contains(listenerWait, "return currentStartResponse(MessageType_ALREADY_STOPPED, \"start cancelled\"), nil") ||
		strings.Contains(listenerWait, "SetCoreStatus(") {
		t.Fatal("cancelled listener admission must observe STOP without republishing a stale state")
	}

	if !strings.Contains(source, "func currentStartResponse") ||
		!strings.Contains(source, "state := static.CoreState") ||
		!strings.Contains(source, "return startResponse(state, messageType, message)") {
		t.Fatal("currentStartResponse must snapshot state under the lifecycle lock for concurrent Stop")
	}
}

func TestStopAndAlertDoesNotDetachTheBoxBeforeItsExactStop(t *testing.T) {
	source := readHcoreSource(t, "custom.go")
	stopAndAlert := sourceSection(t, source, "func StopAndAlert", "\nfunc Close")
	if !strings.Contains(stopAndAlert, "stopOperations.currentOrStart(stopOnce)") {
		t.Fatal("StopAndAlert must delegate exact cleanup to the shared stop operation")
	}
	if strings.Contains(stopAndAlert, "static.Box = nil") || strings.Contains(stopAndAlert, "static.activeLocalListeners = nil") {
		t.Fatal("StopAndAlert must not detach Box or listener state before exact stop completion")
	}
}

func TestNativeCloseEmitsPhasesAndGrpcStopIsTrackedAndBoundedForPublicCallers(t *testing.T) {
	closeSource := readHcoreSource(t, "custom.go")
	coreStopBegin := strings.Index(closeSource, "native close phase=core_stop_begin")
	coreStopComplete := strings.Index(closeSource, "native close phase=core_stop_complete")
	grpcComplete := strings.Index(closeSource, "native close phase=grpc_stop_complete_or_handed_off")
	platformCleared := strings.Index(closeSource, "native close phase=platform_interface_cleared")
	complete := strings.Index(closeSource, "native close phase=complete")
	if coreStopBegin < 0 || coreStopComplete < coreStopBegin || grpcComplete < coreStopComplete || platformCleared < grpcComplete || complete < platformCleared {
		t.Fatal("native close phase telemetry must cover exact core stop, bounded gRPC stop, platform detach, and completion in order")
	}

	grpcSource := readHcoreSource(t, "grpc_server.go")
	close := sourceSection(t, grpcSource, "func CloseGrpcServer", "\n// Publish the exact stop barrier")
	request := sourceSection(t, grpcSource, "func requestGrpcServerStop", "\nfunc runGrpcServerStop")
	registryDelete := strings.Index(request, "delete(grpcServer, mode)")
	operation := strings.Index(request, "operation = &grpcServerStopOperation{done: make(chan struct{})}")
	launch := strings.Index(request, "go runGrpcServerStop(mode, server, previous, operation)")
	timeout := strings.Index(close, "time.NewTimer(grpcServerStopTimeout)")
	requestCall := strings.Index(close, "requestGrpcServerStop(mode, nil)")
	if requestCall < 0 || timeout < requestCall {
		t.Fatal("public gRPC close must publish the exact stop operation before bounding its own wait")
	}
	if registryDelete < 0 || operation < registryDelete || launch < operation {
		t.Fatal("gRPC stop must unpublish, publish its tracked barrier, then launch the asynchronous stop")
	}
	if !strings.Contains(grpcSource, "grpcServerStop map[SetupMode]*grpcServerStopOperation") ||
		!strings.Contains(grpcSource, "func waitForAllGrpcServerStops()") {
		t.Fatal("gRPC stop must retain a process-wide tracked barrier for later exact cleanup")
	}

	run := sourceSection(t, grpcSource, "func runGrpcServerStop", "\nfunc waitForGrpcServerStop")
	previousWait := strings.Index(run, "<-previous.done")
	serverStop := strings.Index(run, "server.Stop()")
	operationComplete := strings.Index(run, "close(operation.done)")
	if previousWait < 0 || serverStop < previousWait || operationComplete < serverStop {
		t.Fatal("tracked gRPC stops must serialize prior ownership and close their barrier after server.Stop")
	}
	if !strings.Contains(grpcSource, "const grpcServerStopTimeout =") {
		t.Fatal("gRPC stop requires an explicit bounded public timeout")
	}
}

func readHcoreSource(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(source)
}

func sourceSection(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("missing source marker %q", startMarker)
	}
	endOffset := strings.Index(source[start:], endMarker)
	if endOffset < 0 {
		t.Fatalf("missing source marker %q after %q", endMarker, startMarker)
	}
	return source[start : start+endOffset]
}
