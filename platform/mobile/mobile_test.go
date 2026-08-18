package mobile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Compile-time contract: gomobile callers must receive hcore.Close failures.
var _ func(int) error = Close
var _ func() error = ResetNetworkForRouteRecovery

func TestCloseExposesErrorResult(t *testing.T) {}

func TestResetNetworkForRouteRecoveryDelegatesToHCore(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to locate mobile package source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "mobile.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "func ResetNetworkForRouteRecovery() error {\n\treturn hcore.ResetNetworkForRouteRecovery()\n}") {
		t.Fatal("ResetNetworkForRouteRecovery must delegate to hcore.ResetNetworkForRouteRecovery")
	}
}
