package hcore

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ensureSetupCallOrder parses Setup wrapper + setup implementation and validates sequencing
// independent of runtime behavior.
func TestSetupRestoresSettingsBeforeReadinessPublishing(t *testing.T) {
	data, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "service.go", data, 0)
	if err != nil {
		t.Fatalf("parse service.go: %v", err)
	}

	var setupWrapper, setupAdmission, setupImpl *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		switch fn.Name.Name {
		case "Setup":
			if fn.Recv == nil {
				setupWrapper = fn
			}
		case "setupWithAdmission":
			if fn.Recv == nil {
				setupAdmission = fn
			}
		case "setup":
			if fn.Recv == nil {
				setupImpl = fn
			}
		}
	}
	if setupWrapper == nil || setupWrapper.Body == nil {
		t.Fatal("Setup function not found in service.go")
	}
	if setupAdmission == nil || setupAdmission.Body == nil {
		t.Fatal("setupWithAdmission implementation function not found in service.go")
	}
	if setupImpl == nil || setupImpl.Body == nil {
		t.Fatal("setup implementation function not found in service.go")
	}
	if !containsCallToFunction(setupWrapper.Body, "setupWithAdmission") {
		t.Fatal("Setup wrapper should delegate to generation-aware setup admission")
	}

	calls := map[string][]token.Pos{}
	ast.Inspect(setupImpl.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || call == nil || call.Fun == nil {
			return true
		}
		name := calledIdentName(call.Fun)
		switch name {
		case "restoreMartenSettings", "InitMartenService", "StartGrpcServerByMode":
			calls[name] = append(calls[name], call.Pos())
		}
		return true
	})

	required := []string{
		"restoreMartenSettings",
		"InitMartenService",
		"StartGrpcServerByMode",
	}
	for _, name := range required {
		if len(calls[name]) == 0 {
			t.Fatalf("setup should contain call %q", name)
		}
	}

	changePos := calls["restoreMartenSettings"][0]
	initPos := calls["InitMartenService"][0]
	startPos := calls["StartGrpcServerByMode"][0]

	if !(changePos < initPos && initPos < startPos) {
		t.Fatalf("setup order changed: restoreMartenSettings=%v, InitMartenService=%v, StartGrpcServerByMode=%v", fset.Position(changePos), fset.Position(initPos), fset.Position(startPos))
	}

	guardPos, ok := findBackgroundGuardPosition(setupImpl.Body)
	if !ok {
		t.Fatal("did not find background fail-closed guard before setup completion")
	}
	if !(guardPos < initPos && guardPos < startPos) {
		t.Fatalf("fail-closed background guard must run before InitMartenService and StartGrpcServerByMode; guard=%v init=%v start=%v",
			fset.Position(guardPos), fset.Position(initPos), fset.Position(startPos))
	}
	setupSource := string(data)
	alreadyStarted := strings.Index(setupSource, "if serverAlreadyStarted {")
	readinessStart := strings.LastIndex(setupSource, "StartGrpcServerByMode(params.Listen, params.Mode)")
	if alreadyStarted < 0 || readinessStart < alreadyStarted {
		t.Fatal("non-OLD Setup must retain serverAlreadyStarted diagnostics and always revalidate readiness")
	}
	alreadyStartedBody := setupSource[alreadyStarted:readinessStart]
	if strings.Contains(alreadyStartedBody, "return nil") || strings.Contains(alreadyStartedBody, "return err") {
		t.Fatal("stale serverAlreadyStarted must not return success before atomic gRPC readiness revalidation")
	}
}

func TestSetupAdmissionWaitsOutsideLifecycleAndRechecksOwnerAfterStopBarrier(t *testing.T) {
	service := readHcoreSource(t, "service.go")
	admission := sourceSection(t, service, "func setupWithAdmission", "\nfunc setup(")
	firstSnapshot := strings.Index(admission, "grpcSetupAdmissionSnapshot(owner)")
	stopWait := strings.Index(admission, "case <-operation.done:")
	lifecycle := strings.Index(admission, "coreLifecycle.run(func() error {")
	secondSnapshot := strings.Index(admission[lifecycle:], "grpcSetupAdmissionSnapshot(owner)")
	staleReject := strings.Index(admission[lifecycle:], "gRPC setup server is stopping")
	if firstSnapshot < 0 || stopWait < firstSnapshot || lifecycle < stopWait || secondSnapshot < 0 || staleReject < secondSnapshot {
		t.Fatal("Setup must wait for tracked stops outside coreLifecycle, then reject a stale owner after its lifecycle recheck")
	}
	lifecycleBody := admission[lifecycle:]
	if strings.Contains(lifecycleBody, "<-operation.done") {
		t.Fatal("Setup must never wait for a gRPC Stop barrier while holding coreLifecycle")
	}

	grpc := readHcoreSource(t, "grpc_server.go")
	start := sourceSection(t, grpc, "func StartGrpcServerByMode", "\nfunc grpcServerForMode")
	joinModeStop := strings.Index(start, "waitForGrpcServerStop(mode)")
	registryLock := strings.Index(start, "grpcServerMu.Lock()")
	existing := strings.Index(start, "if server := grpcServer[mode]; server != nil")
	if joinModeStop < 0 || registryLock < joinModeStop || existing < registryLock {
		t.Fatal("atomic readiness must join a concurrent mode stop before inspecting or returning a server")
	}
	if !strings.Contains(start, "grpcServer[mode] = server") {
		t.Fatal("successful Setup readiness must publish a concrete gRPC server in the registry")
	}
	if !strings.Contains(start, "RegisterCoreServer(server, &CoreService{grpcOwner: owner})") ||
		!strings.Contains(start, "grpcServerOwn[mode] = owner") {
		t.Fatal("each registered CoreService must carry the same owner generation that is published in the registry")
	}
}

func TestSetupQueuedBehindCloseRejectsStaleOwnerAfterExactStopBarrier(t *testing.T) {
	const mode = SetupMode_GRPC_NORMAL_INSECURE
	owner := &grpcServerOwner{mode: mode}
	operation := &grpcServerStopOperation{done: make(chan struct{})}

	grpcServerMu.Lock()
	previousServer, hadServer := grpcServer[mode]
	previousOwner, hadOwner := grpcServerOwn[mode]
	previousStop, hadStop := grpcServerStop[mode]
	grpcServerOwn[mode] = owner
	grpcServerStop[mode] = operation
	grpcServerMu.Unlock()
	t.Cleanup(func() {
		grpcServerMu.Lock()
		if hadServer {
			grpcServer[mode] = previousServer
		} else {
			delete(grpcServer, mode)
		}
		if hadOwner {
			grpcServerOwn[mode] = previousOwner
		} else {
			delete(grpcServerOwn, mode)
		}
		if hadStop {
			grpcServerStop[mode] = previousStop
		} else {
			delete(grpcServerStop, mode)
		}
		grpcServerMu.Unlock()
	})

	// Keep the lifecycle occupied as a Close handler would. The admitted Setup
	// must wait for the exact Stop barrier before it queues for this lock.
	lifecycleEntered := make(chan struct{})
	releaseLifecycle := make(chan struct{})
	go func() {
		_ = coreLifecycle.run(func() error {
			close(lifecycleEntered)
			<-releaseLifecycle
			return nil
		})
	}()
	select {
	case <-lifecycleEntered:
	case <-time.After(time.Second):
		t.Fatal("test setup did not acquire coreLifecycle")
	}

	result := make(chan error, 1)
	go func() {
		result <- setupWithAdmission(context.Background(), &SetupRequest{Mode: mode}, nil, owner)
	}()

	// Close unpublishes the owner before its exact server Stop can finish.
	grpcServerMu.Lock()
	delete(grpcServerOwn, mode)
	grpcServerMu.Unlock()
	close(operation.done)
	close(releaseLifecycle)

	select {
	case err := <-result:
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("Setup error code = %v, want %v (error %v)", status.Code(err), codes.Unavailable, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Setup deadlocked behind a completed exact server-stop barrier")
	}
}

func TestCoreServiceSetupNilErrorReturnsEmptyMessage(t *testing.T) {
	originalRuntimeDebug := RuntimeDebugEnabled()
	originalOptions := static.MartenOptions
	originalPlatformInterface := static.globalPlatformInterface
	t.Cleanup(func() {
		SetRuntimeDebug(originalRuntimeDebug)
		static.MartenOptions = originalOptions
		static.globalPlatformInterface = originalPlatformInterface
	})

	const setupMode = SetupMode_GRPC_NORMAL_INSECURE
	previousServer, hadPrevious := grpcServer[setupMode]

	grpcServer[setupMode] = grpc.NewServer()
	t.Cleanup(func() {
		if hadPrevious {
			grpcServer[setupMode] = previousServer
		} else {
			delete(grpcServer, setupMode)
		}
	})

	response, err := (&CoreService{}).Setup(context.Background(), &SetupRequest{Mode: setupMode})
	if err != nil {
		t.Fatalf("setup returned unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("setup response is nil")
	}
	if response.Code != hcommon.ResponseCode_OK {
		t.Fatalf("response code = %v, want %v", response.Code, hcommon.ResponseCode_OK)
	}
	if response.Message != "" {
		t.Fatalf("setup error message = %q, want empty", response.Message)
	}
}

func calledIdentName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func findBackgroundGuardPosition(stmt ast.Stmt) (token.Pos, bool) {
	var guardPos token.Pos
	found := false
	ast.Inspect(stmt, func(node ast.Node) bool {
		if found {
			return false
		}
		if ifStmt, ok := node.(*ast.IfStmt); ok {
			if isBackgroundSetupGuard(ifStmt.Cond) {
				guardPos = ifStmt.Pos()
				found = true
				return false
			}
		}
		return true
	})
	return guardPos, found
}

func isBackgroundSetupGuard(expr ast.Expr) bool {
	binary, ok := unwrapParen(expr).(*ast.BinaryExpr)
	if !ok || binary.Op != token.LAND {
		return false
	}
	if !isBackgroundSetupModeCall(binary.X) {
		return false
	}

	orExpr, ok := unwrapParen(binary.Y).(*ast.BinaryExpr)
	if !ok || orExpr.Op != token.LOR {
		return false
	}

	left := isNegatedSettingsReady(orExpr.X)
	right := isBooleanIdent(orExpr.Y, "settingsReadError")
	leftAlt := isNegatedSettingsReady(orExpr.Y)
	rightAlt := isBooleanIdent(orExpr.X, "settingsReadError")
	return (left && right) || (leftAlt && rightAlt)
}

func isNegatedSettingsReady(expr ast.Expr) bool {
	unary, ok := unwrapParen(expr).(*ast.UnaryExpr)
	if !ok || unary.Op != token.NOT {
		return false
	}
	switch value := unwrapParen(unary.X).(type) {
	case *ast.SelectorExpr:
		return value.Sel.Name == "settingsReady" && isBooleanIdent(value.X, "runtimeConfig")
	case *ast.CallExpr:
		if calledIdentName(value.Fun) != "Load" {
			return false
		}
		settingsReady, ok := value.Fun.(*ast.SelectorExpr)
		if !ok || settingsReady.Sel.Name != "Load" {
			return false
		}
		configSelector, ok := settingsReady.X.(*ast.SelectorExpr)
		return ok && configSelector.Sel.Name == "settingsReady" && isBooleanIdent(configSelector.X, "runtimeConfig")
	default:
		return false
	}
}

func isBackgroundSetupModeCall(expr ast.Expr) bool {
	call, ok := unwrapParen(expr).(*ast.CallExpr)
	if !ok {
		return false
	}
	return calledIdentName(call.Fun) == "isBackgroundSetupMode"
}

func isNegatedIdent(expr ast.Expr, expected string) bool {
	unary, ok := unwrapParen(expr).(*ast.UnaryExpr)
	if !ok || unary.Op != token.NOT {
		return false
	}
	return isBooleanIdent(unary.X, expected)
}

func isBooleanIdent(expr ast.Expr, expected string) bool {
	ident, ok := unwrapParen(expr).(*ast.Ident)
	return ok && ident.Name == expected
}

func unwrapParen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

func containsCallToFunction(stmt ast.Stmt, name string) bool {
	found := false
	ast.Inspect(stmt, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || call == nil {
			return true
		}
		if calledIdentName(call.Fun) == name {
			found = true
			return false
		}
		return true
	})
	return found
}
