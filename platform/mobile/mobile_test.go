package mobile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreconfig "github.com/Cr0me1ve/marten-core/v2/config"
)

// Compile-time contract: gomobile callers must receive hcore.Close failures.
var _ func(int) error = Close
var _ func() error = ResetNetworkForRouteRecovery

func TestCloseExposesErrorResult(t *testing.T) {}

type endpointResolveCallback func() error

func (callback endpointResolveCallback) Invoke() error { return callback() }

func TestEndpointResolveContextCancellationAndCloseAreExact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := &EndpointResolveContext{ctx: ctx}
	var callbacks atomic.Int32
	completed := make(chan struct{})
	request.OnCancel(endpointResolveCallback(func() error {
		callbacks.Add(1)
		close(completed)
		return nil
	}))

	cancel()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("cancellation callback did not cross the gomobile bridge")
	}
	request.close()
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("cancellation callback ran %d times, want once", got)
	}

	closingContext, cancelClosing := context.WithCancel(context.Background())
	closingRequest := &EndpointResolveContext{ctx: closingContext}
	var afterClose atomic.Int32
	closingRequest.OnCancel(endpointResolveCallback(func() error {
		afterClose.Add(1)
		return nil
	}))
	closingRequest.close()
	cancelClosing()
	time.Sleep(10 * time.Millisecond)
	if got := afterClose.Load(); got != 0 {
		t.Fatalf("closed resolver context invoked cancellation callback %d times", got)
	}
}

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

func TestCanonicalizeSubscriptionAlwaysUsesPureCompatibilityMode(t *testing.T) {
	const content = `{
		"providers":[
			{"hosts":["provider.example.test"],"credentials":[{"client_id":"placeholder-id","client_secret":"placeholder-secret","future_credential_field":true}],"future_provider_field":true},
			{"hosts":"wrong-type","credentials":[{"client_id":"placeholder-id","client_secret":"placeholder-secret"}]}
		],
		"outbounds":[
			{"type":"future-protocol","tag":"future"},
			{"type":"vless","tag":"usable","server":"usable.example.test","server_port":443,"uuid":"00000000-0000-0000-0000-000000000001","future_outbound_field":true},
			{"type":"vless","tag":"malformed","server":"malformed.example.test","server_port":443}
		]
	}`
	const settings = `{"enable-full-config":true}`

	canonical, err := CanonicalizeSubscription(content, settings)
	if err != nil {
		t.Fatalf("pure compatibility canonicalization failed: %v", err)
	}
	if content == canonical {
		t.Fatal("canonicalizer must return a separately canonicalized payload")
	}
	if settings != `{"enable-full-config":true}` {
		t.Fatalf("canonicalizer mutated caller settings: %q", settings)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(canonical), &decoded); err != nil {
		t.Fatalf("decode canonical payload: %v", err)
	}
	serialized := canonical
	if strings.Contains(serialized, "future-protocol") || strings.Contains(serialized, "malformed.example.test") {
		t.Fatalf("compatibility mode retained unsupported sibling: %s", canonical)
	}
	if !strings.Contains(serialized, "usable.example.test") || !strings.Contains(serialized, "provider.example.test") {
		t.Fatalf("compatibility mode discarded usable subscription entries: %s", canonical)
	}

	options := coreconfig.DefaultMartenOptions()
	if options.EnableFullConfig {
		t.Fatal("test default options unexpectedly enable full config")
	}
}
