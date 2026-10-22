package hcore

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestPauseRPCReturnsWithoutJoiningStopAndSuccessorWaitsForExactBarrier(t *testing.T) {
	const mode = SetupMode_GRPC_NORMAL_INSECURE
	saveGrpcServerRegistryEntry(t, mode)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := newRecoveredGrpcServer()
	owner := &grpcServerOwner{mode: mode}
	RegisterCoreServer(server, &CoreService{grpcOwner: owner})

	// Model a prior generation's exact stop still draining. Pause must publish
	// its own barrier and return; a successor Setup must wait for that barrier.
	previousStop := &grpcServerStopOperation{done: make(chan struct{})}
	grpcServerMu.Lock()
	grpcServer[mode] = server
	grpcServerOwn[mode] = owner
	grpcServerStop[mode] = previousStop
	grpcServerMu.Unlock()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	client, err := grpc.NewClient(
		"passthrough:///pause-rpc-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		}),
	)
	if err != nil {
		t.Fatalf("create gRPC client: %v", err)
	}
	defer client.Close()

	callCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	startedAt := time.Now()
	_, pauseErr := NewCoreClient(client).Pause(callCtx, &PauseRequest{Mode: mode})
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("Pause RPC took %s, want it to return before its own stop barrier", elapsed)
	}
	if pauseErr != nil && status.Code(pauseErr) != codes.Unavailable && status.Code(pauseErr) != codes.Canceled {
		t.Fatalf("Pause RPC error = %v, want success or transport closure during Stop", pauseErr)
	}

	grpcServerMu.RLock()
	stop := grpcServerStop[mode]
	currentServer := grpcServer[mode]
	grpcServerMu.RUnlock()
	if currentServer != nil || stop == nil || stop == previousStop {
		t.Fatalf("Pause must retire its owner and publish a successor stop barrier: server=%p stop=%p", currentServer, stop)
	}
	select {
	case <-stop.done:
		t.Fatal("new stop barrier completed before its predecessor")
	default:
	}

	setupDone := make(chan error, 1)
	go func() {
		waitForGrpcServerStop(mode)
		setupDone <- nil
	}()
	select {
	case err := <-setupDone:
		t.Fatalf("successor Setup passed the pending stop barrier: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(previousStop.done)
	select {
	case <-stop.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the exact stop did not complete after its predecessor")
	}
	select {
	case err := <-setupDone:
		if err != nil {
			t.Fatalf("successor stop join returned unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("successor Setup did not recheck owner after joining the stop barrier")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("retired server did not leave its Serve loop")
	}

	// A late Pause from the old generation must not retire a replacement.
	waitForGrpcServerStop(mode)
	replacement := grpc.NewServer()
	replacementOwner := &grpcServerOwner{mode: mode}
	grpcServerMu.Lock()
	grpcServer[mode] = replacement
	grpcServerOwn[mode] = replacementOwner
	grpcServerMu.Unlock()
	if operation := requestGrpcServerStop(mode, owner); operation != nil {
		t.Fatal("a late Pause from the old owner must not stop its replacement")
	}
	grpcServerMu.RLock()
	gotServer, gotOwner := grpcServer[mode], grpcServerOwn[mode]
	grpcServerMu.RUnlock()
	if gotServer != replacement || gotOwner != replacementOwner {
		t.Fatalf("late Pause removed successor: server=%p owner=%p", gotServer, gotOwner)
	}
	staleSetupErr := setupWithAdmission(context.Background(), &SetupRequest{Mode: mode}, nil, owner)
	if status.Code(staleSetupErr) != codes.Unavailable {
		t.Fatalf("stale Setup error = %v, want unavailable", staleSetupErr)
	}
	replacement.Stop()
}
