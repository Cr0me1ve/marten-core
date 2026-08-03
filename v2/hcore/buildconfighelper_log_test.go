package hcore

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/sagernet/sing-box/option"
)

func TestLogBuiltConfigStateDoesNotLeakSensitiveConfigValues(t *testing.T) {
	originalRuntimeDebug := RuntimeDebugEnabled()
	originalOptions := static.MartenOptions
	originalPlatformInterface := static.globalPlatformInterface
	originalLastConfigHasTun := static.lastConfigHasTun.Load()
	originalLastConfigInboundCount := static.lastConfigInboundCount.Load()
	t.Cleanup(func() {
		SetRuntimeDebug(originalRuntimeDebug)
		static.MartenOptions = originalOptions
		static.globalPlatformInterface = originalPlatformInterface
		static.lastConfigHasTun.Store(originalLastConfigHasTun)
		static.lastConfigInboundCount.Store(originalLastConfigInboundCount)
	})

	static.MartenOptions = config.DefaultMartenOptions()
	static.MartenOptions.EnableTun = true
	static.globalPlatformInterface = &MobilePlatformInterface{}
	SetRuntimeDebug(true)

	rawOptions := option.Options{
		Inbounds: []option.Inbound{
			{Type: "trojan", Tag: "secret.trojan.endpoint"},
			{Type: "vless", Tag: "another.secret.endpoint"},
			{Type: "mixed", Tag: "uuid-sneaky-token-0001"},
		},
	}

	logSub, stopch, _ := static.logObserver.Subscribe()
	t.Cleanup(func() { static.logObserver.UnSubscribe(logSub) })

	logBuiltConfigState(&rawOptions)

	var snapshot string
	select {
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for logBuiltConfigState output")
	case <-stopch:
		t.Fatal("log observer stopped before config state message arrived")
	case msg := <-logSub:
		snapshot = msg.GetMessage()
	}

	const summaryPattern = `^core config state enable_tun=(true|false) platform_interface=(true|false) inbound_count=\d+ inbound_types=[a-z0-9_-]*(,[a-z0-9_-]+)*$`
	if !regexp.MustCompile(summaryPattern).MatchString(snapshot) {
		t.Fatalf("unexpected summary payload: %q", snapshot)
	}

	if strings.Contains(snapshot, "secret.trojan.endpoint") {
		t.Fatal("server address leaked into log")
	}
	if strings.Contains(snapshot, "another.secret.endpoint") {
		t.Fatal("server address leaked into log")
	}
	if strings.Contains(snapshot, "uuid-sneaky-token-0001") {
		t.Fatal("uuid/token leaked into log")
	}

	if !strings.Contains(snapshot, "inbound_count=3") {
		t.Fatalf("expected inbound_count summary, got %q", snapshot)
	}
	if !strings.Contains(snapshot, "enable_tun=true") {
		t.Fatalf("expected enable_tun summary, got %q", snapshot)
	}
	if !strings.Contains(snapshot, "platform_interface=true") {
		t.Fatalf("expected platform_interface summary, got %q", snapshot)
	}
	if !strings.Contains(snapshot, "inbound_types=trojan,vless,mixed") {
		t.Fatalf("unexpected inbound_types summary: %q", snapshot)
	}
	if static.lastConfigHasTun.Load() {
		t.Fatalf("lastConfigHasTun = %t, want false", static.lastConfigHasTun.Load())
	}
	if static.lastConfigInboundCount.Load() != 3 {
		t.Fatalf("lastConfigInboundCount = %d, want 3", static.lastConfigInboundCount.Load())
	}
}
