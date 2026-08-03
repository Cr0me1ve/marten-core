package hcore

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Cr0me1ve/marten-core/v2/config"
)

func TestBackgroundCloseClearsOnlyThePlatformSnapshot(t *testing.T) {
	static.runtimeConfigLock.Lock()
	originalOptions := static.MartenOptions
	originalPlatform := static.globalPlatformInterface
	originalReady := static.settingsReady.Load()
	static.runtimeConfigLock.Unlock()
	static.lock.Lock()
	originalBox := static.Box
	originalState := static.CoreState
	originalWorkingPath := sWorkingPath
	static.Box = nil
	static.CoreState = CoreStates_STOPPED
	sWorkingPath = ""
	static.lock.Unlock()
	t.Cleanup(func() {
		static.runtimeConfigLock.Lock()
		static.MartenOptions = originalOptions
		static.globalPlatformInterface = originalPlatform
		static.settingsReady.Store(originalReady)
		static.runtimeConfigLock.Unlock()
		static.lock.Lock()
		static.Box = originalBox
		static.CoreState = originalState
		sWorkingPath = originalWorkingPath
		static.lock.Unlock()
	})

	publishCoreRuntimeSettings(config.DefaultMartenOptions(), true)
	backgroundPlatform := &MobilePlatformInterface{}
	updateCorePlatformInterface(backgroundPlatform)
	if err := Close(SetupMode_GRPC_BACKGROUND_INSECURE); err != nil {
		t.Fatalf("background Close: %v", err)
	}
	if snapshot := loadCoreRuntimeConfigSnapshot(); snapshot.platformInterface != nil {
		t.Fatalf("background Close retained platform interface: %T", snapshot.platformInterface)
	}

	normalPlatform := &MobilePlatformInterface{}
	updateCorePlatformInterface(normalPlatform)
	if err := Close(SetupMode_GRPC_NORMAL_INSECURE); err != nil {
		t.Fatalf("normal Close: %v", err)
	}
	if snapshot := loadCoreRuntimeConfigSnapshot(); snapshot.platformInterface != normalPlatform {
		t.Fatalf("normal Close unexpectedly cleared platform interface: %T", snapshot.platformInterface)
	}
}

func TestValidateAndroidRuntimeSettingsFailsClosedOnlyOnAndroid(t *testing.T) {
	for _, test := range []struct {
		name    string
		goos    string
		ready   bool
		wantErr bool
	}{
		{name: "android missing settings", goos: "android", ready: false, wantErr: true},
		{name: "android ready", goos: "android", ready: true},
		{name: "non android missing settings", goos: "linux", ready: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAndroidRuntimeSettings(test.goos, coreRuntimeConfigSnapshot{settingsReady: test.ready})
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAndroidRuntimeSettings(%q, ready=%t) error=%v, want error=%t", test.goos, test.ready, err, test.wantErr)
			}
		})
	}
}

func TestGrpcServerRegistryStartAndCloseAreRaceSafe(t *testing.T) {
	const mode = SetupMode_GRPC_NORMAL_INSECURE
	CloseGrpcServer(mode)
	t.Cleanup(func() { CloseGrpcServer(mode) })

	const workers = 6
	const iterations = 8
	errors := make(chan error, workers*iterations)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				if _, err := StartGrpcServerByMode("127.0.0.1:0", mode); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				_ = hasGrpcServer(mode)
				CloseGrpcServer(mode)
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent gRPC registry operation: %v", err)
	}
	CloseGrpcServer(mode)
	if hasGrpcServer(mode) {
		t.Fatal("CloseGrpcServer left a registry entry behind")
	}
}

func TestGrpcServerStartPublishesOnlyAfterListenAndServesLocalInstance(t *testing.T) {
	data, err := os.ReadFile("grpc_server.go")
	if err != nil {
		t.Fatalf("read grpc_server.go: %v", err)
	}
	source := string(data)
	existing := strings.Index(source, "if server := grpcServer[mode]; server != nil")
	portCheck := strings.Index(source, "hutils.IsPortInUse")
	listen := strings.Index(source, "net.Listen(\"tcp\", listenAddressG)")
	publish := strings.Index(source, "grpcServer[mode] = server")
	localServe := strings.Index(source, "server.Serve(lis)")
	if existing < 0 || portCheck < 0 || listen < 0 || publish < 0 || localServe < 0 {
		t.Fatalf("missing expected gRPC registry lifecycle markers: existing=%d port=%d listen=%d publish=%d serve=%d", existing, portCheck, listen, publish, localServe)
	}
	if !(existing < portCheck && listen < publish) {
		t.Fatalf("gRPC registry order changed: existing=%d port=%d listen=%d publish=%d", existing, portCheck, listen, publish)
	}
	if strings.Contains(source[localServe-32:localServe+len("server.Serve(lis)")+32], "grpcServer[mode].Serve") {
		t.Fatal("serve goroutine must retain the local server instance, not reread the mutable registry")
	}
}

func TestCloseUsesBackgroundOnlyPlatformCleanup(t *testing.T) {
	data, err := os.ReadFile("custom.go")
	if err != nil {
		t.Fatalf("read custom.go: %v", err)
	}
	source := string(data)
	backgroundGuard := strings.Index(source, "if isBackgroundSetupMode(mode)")
	clear := strings.Index(source, "clearCorePlatformInterface()")
	if backgroundGuard < 0 || clear < 0 || clear < backgroundGuard {
		t.Fatalf("Close must gate platform cleanup to background mode: guard=%d clear=%d", backgroundGuard, clear)
	}
	if count := strings.Count(source, "clearCorePlatformInterface()"); count != 1 {
		t.Fatalf("Close should have one background-only platform cleanup, got %d", count)
	}
}

func TestGrpcServerExistingInstanceSkipsInvalidPortValidation(t *testing.T) {
	const mode = SetupMode_GRPC_BACKGROUND_INSECURE
	CloseGrpcServer(mode)
	t.Cleanup(func() { CloseGrpcServer(mode) })

	server, err := StartGrpcServerByMode("127.0.0.1:0", mode)
	if err != nil {
		t.Fatalf("start initial gRPC server: %v", err)
	}
	existing, err := StartGrpcServerByMode("not-a-listen-address", mode)
	if err != nil {
		t.Fatalf("existing server should be returned before address validation: %v", err)
	}
	if existing != server {
		t.Fatalf("existing server = %p, want %p", existing, server)
	}
	if !hasGrpcServer(mode) {
		t.Fatalf("gRPC registry lost existing instance")
	}
}
