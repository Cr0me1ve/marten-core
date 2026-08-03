package hcore

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"google.golang.org/grpc"
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

	var setupWrapper, setupImpl *ast.FuncDecl
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
		case "setup":
			if fn.Recv == nil {
				setupImpl = fn
			}
		}
	}
	if setupWrapper == nil || setupWrapper.Body == nil {
		t.Fatal("Setup function not found in service.go")
	}
	if setupImpl == nil || setupImpl.Body == nil {
		t.Fatal("setup implementation function not found in service.go")
	}
	if !containsCallToFunction(setupWrapper.Body, "setup") {
		t.Fatal("Setup wrapper should delegate to setup implementation")
	}

	calls := map[string][]token.Pos{}
	var existingServerReturn token.Pos
	ast.Inspect(setupImpl.Body, func(node ast.Node) bool {
		if ifStmt, ok := node.(*ast.IfStmt); ok && isBooleanIdent(ifStmt.Cond, "serverAlreadyStarted") {
			ast.Inspect(ifStmt.Body, func(bodyNode ast.Node) bool {
				if returnStmt, ok := bodyNode.(*ast.ReturnStmt); ok {
					existingServerReturn = returnStmt.Pos()
					return false
				}
				return true
			})
		}
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
	if existingServerReturn == token.NoPos {
		t.Fatal("did not find already-started gRPC return")
	}
	if !(guardPos < existingServerReturn && initPos < existingServerReturn) {
		t.Fatalf("existing gRPC server must revalidate settings and initialize services before returning; guard=%v init=%v return=%v",
			fset.Position(guardPos), fset.Position(initPos), fset.Position(existingServerReturn))
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
