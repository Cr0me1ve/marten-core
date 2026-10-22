package hcore

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/hello"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const foregroundReadinessTestMode = SetupMode_GRPC_NORMAL_INSECURE

func readForegroundReadinessSource(t *testing.T, name string) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate foreground readiness test source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(source)
}

func TestNativeIOSForegroundSetupWiresReadinessRepair(t *testing.T) {
	serviceSource := readForegroundReadinessSource(t, "service.go")
	for _, expected := range []string{
		"if owner == nil && (runtime.GOOS == \"ios\" || runtime.GOOS == \"darwin\") {",
		"repairForegroundGrpcReadiness(ctx, params.Mode, params.Listen)",
		"setupWithAdmission(ctx, req, nil, s.grpcOwner)",
	} {
		if !strings.Contains(serviceSource, expected) {
			t.Errorf("service.go is missing native foreground readiness wiring %q", expected)
		}
	}

	grpcSource := readForegroundReadinessSource(t, "grpc_server.go")
	if !strings.Contains(grpcSource, "if mode != SetupMode_GRPC_NORMAL_INSECURE {") ||
		!strings.Contains(grpcSource, "if mode != SetupMode_GRPC_NORMAL_INSECURE {\n\t\treturn nil\n\t}") {
		t.Error("readiness repair must remain limited to foreground insecure gRPC mode")
	}
}

func foregroundReadinessAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback address: %v", err)
	}
	return address
}

func foregroundHelloClient(t *testing.T, address string) hello.HelloClient {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("create gRPC client: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return hello.NewHelloClient(connection)
}

func foregroundHelloResponse(t *testing.T, client hello.HelloClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := client.SayHello(ctx, &hello.HelloRequest{Name: "readiness"})
	if err != nil {
		t.Fatalf("Hello RPC: %v", err)
	}
	if response.GetMessage() != "Hello, readiness" {
		t.Fatalf("Hello response = %q, want %q", response.GetMessage(), "Hello, readiness")
	}
}

func registerForegroundReadinessServer(
	t *testing.T,
	mode SetupMode,
	server *grpc.Server,
	owner *grpcServerOwner,
) {
	t.Helper()
	saveGrpcServerRegistryEntry(t, mode)
	grpcServerMu.Lock()
	grpcServer[mode] = server
	grpcServerOwn[mode] = owner
	delete(grpcServerStop, mode)
	grpcServerMu.Unlock()
	t.Cleanup(server.Stop)
}

func TestForegroundGrpcReadinessKeepsHealthySameEndpoint(t *testing.T) {
	address := foregroundReadinessAddress(t)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen on loopback address: %v", err)
	}
	server := grpc.NewServer()
	hello.RegisterHelloServer(server, &hello.HelloService{})
	owner := &grpcServerOwner{mode: foregroundReadinessTestMode, listenAddress: address}
	registerForegroundReadinessServer(t, foregroundReadinessTestMode, server, owner)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("healthy foreground test server did not stop")
		}
	})

	if err := repairForegroundGrpcReadiness(context.Background(), foregroundReadinessTestMode, address); err != nil {
		t.Fatalf("repair healthy listener: %v", err)
	}
	grpcServerMu.RLock()
	currentServer, currentOwner := grpcServer[foregroundReadinessTestMode], grpcServerOwn[foregroundReadinessTestMode]
	grpcServerMu.RUnlock()
	if currentServer != server || currentOwner != owner {
		t.Fatalf("healthy listener was replaced: server=%p owner=%p", currentServer, currentOwner)
	}
	foregroundHelloResponse(t, foregroundHelloClient(t, address))
}

func TestForegroundGrpcReadinessRetiresUnresponsiveRegistrationThenStartsListener(t *testing.T) {
	address := foregroundReadinessAddress(t)
	stale := grpc.NewServer()
	hello.RegisterHelloServer(stale, &hello.HelloService{})
	registerForegroundReadinessServer(t, foregroundReadinessTestMode, stale, &grpcServerOwner{
		mode: foregroundReadinessTestMode, listenAddress: address,
	})

	if err := repairForegroundGrpcReadiness(context.Background(), foregroundReadinessTestMode, address); err != nil {
		t.Fatalf("repair unresponsive registration: %v", err)
	}
	grpcServerMu.RLock()
	currentServer := grpcServer[foregroundReadinessTestMode]
	grpcServerMu.RUnlock()
	if currentServer != nil {
		t.Fatalf("unresponsive registration remains published: %p", currentServer)
	}

	started, err := StartGrpcServerByMode(address, foregroundReadinessTestMode)
	if err != nil {
		t.Fatalf("start replacement foreground listener: %v", err)
	}
	t.Cleanup(started.Stop)
	foregroundHelloResponse(t, foregroundHelloClient(t, address))
}

func TestForegroundGrpcReadinessRetiresRequestedEndpointMismatch(t *testing.T) {
	registeredAddress := foregroundReadinessAddress(t)
	requestedAddress := foregroundReadinessAddress(t)
	listener, err := net.Listen("tcp", registeredAddress)
	if err != nil {
		t.Fatalf("listen on registered loopback address: %v", err)
	}
	server := grpc.NewServer()
	hello.RegisterHelloServer(server, &hello.HelloService{})
	owner := &grpcServerOwner{mode: foregroundReadinessTestMode, listenAddress: registeredAddress}
	registerForegroundReadinessServer(t, foregroundReadinessTestMode, server, owner)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("mismatched foreground test server did not stop")
		}
	})

	if err := repairForegroundGrpcReadiness(context.Background(), foregroundReadinessTestMode, requestedAddress); err != nil {
		t.Fatalf("repair endpoint mismatch: %v", err)
	}
	grpcServerMu.RLock()
	currentServer, currentOwner := grpcServer[foregroundReadinessTestMode], grpcServerOwn[foregroundReadinessTestMode]
	grpcServerMu.RUnlock()
	if currentServer != nil || currentOwner != nil {
		t.Fatalf("mismatched registration remains published: server=%p owner=%p", currentServer, currentOwner)
	}
}

func TestForegroundGrpcReadinessCancellationPreservesRegistration(t *testing.T) {
	address := foregroundReadinessAddress(t)
	server := grpc.NewServer()
	hello.RegisterHelloServer(server, &hello.HelloService{})
	owner := &grpcServerOwner{mode: foregroundReadinessTestMode, listenAddress: address}
	registerForegroundReadinessServer(t, foregroundReadinessTestMode, server, owner)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repairForegroundGrpcReadiness(ctx, foregroundReadinessTestMode, address); err == nil {
		t.Fatal("repair with cancelled context succeeded, want an error")
	}
	grpcServerMu.RLock()
	currentServer, currentOwner := grpcServer[foregroundReadinessTestMode], grpcServerOwn[foregroundReadinessTestMode]
	grpcServerMu.RUnlock()
	if currentServer != server || currentOwner != owner {
		t.Fatalf("cancelled probe changed registration: server=%p owner=%p", currentServer, currentOwner)
	}
}

type gatedForegroundHelloService struct {
	hello.UnimplementedHelloServer
	entered  chan struct{}
	release  chan struct{}
	finished chan error
	once     sync.Once
}

func (s *gatedForegroundHelloService) releaseProbe() {
	s.once.Do(func() { close(s.release) })
}

func (s *gatedForegroundHelloService) SayHello(ctx context.Context, request *hello.HelloRequest) (*hello.HelloResponse, error) {
	close(s.entered)
	select {
	case <-s.release:
		err := status.Error(codes.Unavailable, "test probe failure")
		s.finished <- err
		return nil, err
	case <-ctx.Done():
		err := ctx.Err()
		s.finished <- err
		return nil, err
	}
}

func TestForegroundGrpcReadinessCancellationDuringProbePreservesRegistration(t *testing.T) {
	address := foregroundReadinessAddress(t)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen on loopback address: %v", err)
	}
	server := grpc.NewServer()
	gated := &gatedForegroundHelloService{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan error, 1),
	}
	hello.RegisterHelloServer(server, gated)
	owner := &grpcServerOwner{mode: foregroundReadinessTestMode, listenAddress: address}
	registerForegroundReadinessServer(t, foregroundReadinessTestMode, server, owner)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		gated.releaseProbe()
		server.Stop()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("cancelled foreground test server did not stop")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repairDone := make(chan error, 1)
	go func() { repairDone <- repairForegroundGrpcReadiness(ctx, foregroundReadinessTestMode, address) }()
	select {
	case <-gated.entered:
	case <-time.After(time.Second):
		t.Fatal("readiness probe did not reach the registered server")
	}
	cancel()

	select {
	case err := <-repairDone:
		if err == nil {
			t.Fatal("cancelled in-flight readiness probe succeeded, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled readiness probe did not finish")
	}
	select {
	case probeErr := <-gated.finished:
		if !errors.Is(probeErr, context.Canceled) {
			t.Fatalf("server-side probe error = %v, want canceled", probeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not observe readiness probe cancellation")
	}
	grpcServerMu.RLock()
	currentServer, currentOwner := grpcServer[foregroundReadinessTestMode], grpcServerOwn[foregroundReadinessTestMode]
	grpcServerMu.RUnlock()
	if currentServer != server || currentOwner != owner {
		t.Fatalf("cancelled in-flight probe changed registration: server=%p owner=%p", currentServer, currentOwner)
	}
}

func TestForegroundGrpcReadinessStaleProbeCannotRetireReplacement(t *testing.T) {
	address := foregroundReadinessAddress(t)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen on loopback address: %v", err)
	}
	oldServer := grpc.NewServer()
	gated := &gatedForegroundHelloService{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan error, 1),
	}
	hello.RegisterHelloServer(oldServer, gated)
	oldOwner := &grpcServerOwner{mode: foregroundReadinessTestMode, listenAddress: address}
	registerForegroundReadinessServer(t, foregroundReadinessTestMode, oldServer, oldOwner)
	serveDone := make(chan error, 1)
	go func() { serveDone <- oldServer.Serve(listener) }()
	t.Cleanup(func() {
		gated.releaseProbe()
		oldServer.Stop()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("stale probe test server did not stop")
		}
	})

	repairDone := make(chan error, 1)
	go func() {
		repairDone <- repairForegroundGrpcReadiness(context.Background(), foregroundReadinessTestMode, address)
	}()
	select {
	case <-gated.entered:
	case <-time.After(time.Second):
		t.Fatal("readiness probe did not reach the registered server")
	}

	replacement, replacementOwner := grpc.NewServer(), &grpcServerOwner{mode: foregroundReadinessTestMode, listenAddress: address}
	grpcServerMu.Lock()
	grpcServer[foregroundReadinessTestMode] = replacement
	grpcServerOwn[foregroundReadinessTestMode] = replacementOwner
	grpcServerMu.Unlock()
	t.Cleanup(replacement.Stop)
	gated.releaseProbe()
	select {
	case probeErr := <-gated.finished:
		if status.Code(probeErr) != codes.Unavailable {
			t.Fatalf("stale probe server error = %v, want unavailable", probeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("stale readiness probe did not complete with its intended failure")
	}

	select {
	case err := <-repairDone:
		if err != nil {
			t.Fatalf("stale readiness probe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale readiness probe did not finish")
	}
	grpcServerMu.RLock()
	currentServer, currentOwner := grpcServer[foregroundReadinessTestMode], grpcServerOwn[foregroundReadinessTestMode]
	grpcServerMu.RUnlock()
	if currentServer != replacement || currentOwner != replacementOwner {
		t.Fatalf("stale readiness probe retired replacement: server=%p owner=%p", currentServer, currentOwner)
	}
}

func TestBackgroundGrpcReadinessDoesNotRepairRegistration(t *testing.T) {
	const mode = SetupMode_GRPC_BACKGROUND_INSECURE
	address := foregroundReadinessAddress(t)
	server := grpc.NewServer()
	registerForegroundReadinessServer(t, mode, server, &grpcServerOwner{mode: mode, listenAddress: address})

	if err := repairForegroundGrpcReadiness(context.Background(), mode, address); err != nil {
		t.Fatalf("background mode readiness check: %v", err)
	}
	grpcServerMu.RLock()
	currentServer := grpcServer[mode]
	grpcServerMu.RUnlock()
	if currentServer != server {
		t.Fatalf("background registration was repaired: server=%p want %p", currentServer, server)
	}
}
