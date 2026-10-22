package hcore

import (
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// saveGrpcServerRegistryEntry isolates tests which inspect the process-global
// registry without disturbing a server owned by another test.
func saveGrpcServerRegistryEntry(t *testing.T, mode SetupMode) {
	t.Helper()
	grpcServerMu.Lock()
	previousServer, hadServer := grpcServer[mode]
	previousOwner, hadOwner := grpcServerOwn[mode]
	previousStop, hadStop := grpcServerStop[mode]
	grpcServerMu.Unlock()
	t.Cleanup(func() {
		grpcServerMu.Lock()
		defer grpcServerMu.Unlock()
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
	})
}

func TestGrpcServerServeExitRetiresCurrentGeneration(t *testing.T) {
	const mode = SetupMode_GRPC_BACKGROUND_INSECURE
	saveGrpcServerRegistryEntry(t, mode)

	server := grpc.NewServer()
	owner := &grpcServerOwner{mode: mode}
	grpcServerMu.Lock()
	grpcServer[mode] = server
	grpcServerOwn[mode] = owner
	delete(grpcServerStop, mode)
	grpcServerMu.Unlock()

	retireGrpcServerAfterServe(mode, server, owner)

	grpcServerMu.RLock()
	currentServer := grpcServer[mode]
	currentOwner := grpcServerOwn[mode]
	grpcServerMu.RUnlock()
	if currentServer != nil || currentOwner != nil {
		t.Fatalf("Serve exit retained stale generation: server=%p owner=%p", currentServer, currentOwner)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen after retire: %v", err)
	}
	defer listener.Close()
	if err := server.Serve(listener); err != grpc.ErrServerStopped {
		t.Fatalf("retired server Serve error = %v, want %v", err, grpc.ErrServerStopped)
	}
}

func TestGrpcServerServeExitDoesNotRetireReplacement(t *testing.T) {
	const mode = SetupMode_GRPC_NORMAL_INSECURE
	saveGrpcServerRegistryEntry(t, mode)

	staleServer := grpc.NewServer()
	staleOwner := &grpcServerOwner{mode: mode}
	replacement := grpc.NewServer()
	replacementOwner := &grpcServerOwner{mode: mode}
	t.Cleanup(replacement.Stop)
	grpcServerMu.Lock()
	grpcServer[mode] = replacement
	grpcServerOwn[mode] = replacementOwner
	delete(grpcServerStop, mode)
	grpcServerMu.Unlock()

	retireGrpcServerAfterServe(mode, staleServer, staleOwner)

	grpcServerMu.RLock()
	currentServer := grpcServer[mode]
	currentOwner := grpcServerOwn[mode]
	grpcServerMu.RUnlock()
	if currentServer != replacement || currentOwner != replacementOwner {
		t.Fatalf("stale Serve exit removed replacement: server=%p want %p owner=%p want %p", currentServer, replacement, currentOwner, replacementOwner)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for replacement: %v", err)
	}
	defer listener.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- replacement.Serve(listener) }()
	select {
	case err := <-serveDone:
		t.Fatalf("replacement was stopped by stale Serve cleanup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}
