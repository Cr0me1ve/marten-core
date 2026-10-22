package hcore

/*
#include "stdint.h"
*/

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	"github.com/Cr0me1ve/marten-core/v2/hello"
	hutils "github.com/Cr0me1ve/marten-core/v2/hutils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/Cr0me1ve/marten-core/v2/db"
	_ "google.golang.org/grpc/encoding/gzip"
)

const grpcServerStopTimeout = 3 * time.Second

type CoreService struct {
	UnimplementedCoreServer
	grpcOwner *grpcServerOwner
}

func newRecoveredGrpcServer(options ...grpc.ServerOption) *grpc.Server {
	recoveryOptions := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(recoveryUnaryServerInterceptor),
		grpc.ChainStreamInterceptor(recoveryStreamServerInterceptor),
		// Stop closes transports immediately, while the tracked stop operation
		// remains responsible for every admitted handler. Without this option a
		// cancelled Start RPC can outlive server.Stop and create a Box after the
		// exact Mobile.Close operation has already completed.
		grpc.WaitForHandlers(true),
	}
	return grpc.NewServer(append(recoveryOptions, options...)...)
}

func recoveryUnaryServerInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp = nil
			err = recoveredGrpcPanic(info.FullMethod, recovered)
		}
	}()
	return handler(ctx, req)
}

func recoveryStreamServerInterceptor(
	srv any,
	stream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredGrpcPanic(info.FullMethod, recovered)
		}
	}()
	return handler(srv, stream)
}

func recoveredGrpcPanic(method string, recovered any) error {
	log.Printf("recovered panic in gRPC method %s: %v\n%s", method, recovered, debug.Stack())
	return status.Error(codes.Internal, "internal core error")
}

func StartGrpcServer(listenAddressG string, service string) (*grpc.Server, error) {
	lis, err := net.Listen("tcp", listenAddressG)
	if err != nil {
		log.Printf("failed to listen: %v", err)
		return nil, err
	}
	s := newRecoveredGrpcServer()
	if service == "core" {
		// Setup("./tmp/", "./tmp", "./tmp", 11111, false)
		RegisterCoreServer(s, &CoreService{})
		// pb.RegisterExtensionHostServiceServer(s, &extension.ExtensionHostService{})
	} else if service == "hello" {
		// RegisterHelloServer(s, &hello.HelloService{})
	} else if service == "tunnel" {
		// RegisterTunnelServiceServer(s, &TunnelService{})
	}
	log.Printf("Server listening on %s", listenAddressG)
	go func() {
		if err := s.Serve(lis); err != nil {
			log.Printf("failed to serve: %v", err)
		}
		log.Printf("Server stopped")
		// cancel()
	}()
	return s, nil
}

func StartCoreGrpcServer(listenAddressG string) (*grpc.Server, error) {
	return StartGrpcServer(listenAddressG, "core")
}

func StartHelloGrpcServer(listenAddressG string) (*grpc.Server, error) {
	return StartGrpcServer(listenAddressG, "hello")
}

var (
	certpair       *hutils.CertificatePair
	grpcServer     map[SetupMode]*grpc.Server             = make(map[SetupMode]*grpc.Server)
	grpcServerOwn  map[SetupMode]*grpcServerOwner         = make(map[SetupMode]*grpcServerOwner)
	grpcServerStop map[SetupMode]*grpcServerStopOperation = make(map[SetupMode]*grpcServerStopOperation)
	grpcServerMu   sync.RWMutex
	caCertPool     = x509.NewCertPool()
)

type grpcServerStopOperation struct {
	done chan struct{}
}

// grpcServerOwner is a generation identity for RPC admission. A handler may
// have been accepted just before Close unpublished its server and then block
// behind coreLifecycle. Keeping the identity on CoreService lets that handler
// reject itself after it acquires coreLifecycle instead of waiting for the
// server.Stop operation that is, in turn, waiting for that same handler.
type grpcServerOwner struct {
	mode          SetupMode
	listenAddress string
}

// A registry entry alone is not readiness after iOS suspends the foreground
// runtime. Probe outside lifecycle/registry locks: draining RPC handlers may
// need those locks before the exact old server can finish stopping.
func repairForegroundGrpcReadiness(ctx context.Context, mode SetupMode, listenAddress string) error {
	if mode != SetupMode_GRPC_NORMAL_INSECURE {
		return nil
	}
	ctx, cancelRepair := context.WithTimeout(ctx, grpcServerStopTimeout+700*time.Millisecond)
	defer cancelRepair()
	if err := ctx.Err(); err != nil {
		return err
	}
	grpcServerMu.RLock()
	server, owner := grpcServer[mode], grpcServerOwn[mode]
	grpcServerMu.RUnlock()
	if server == nil || owner == nil {
		return nil
	}

	healthy := false
	if owner.listenAddress == listenAddress {
		probeCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
		client, err := grpc.NewClient(listenAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			response, probeErr := hello.NewHelloClient(client).SayHello(probeCtx, &hello.HelloRequest{Name: "marten-foreground-readiness"})
			healthy = probeErr == nil && response.GetMessage() == "Hello, marten-foreground-readiness"
			client.Close()
		}
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if healthy {
		return nil
	}

	grpcServerMu.Lock()
	if grpcServer[mode] != server || grpcServerOwn[mode] != owner {
		grpcServerMu.Unlock()
		return nil
	}
	delete(grpcServer, mode)
	delete(grpcServerOwn, mode)
	previous := grpcServerStop[mode]
	operation := &grpcServerStopOperation{done: make(chan struct{})}
	grpcServerStop[mode] = operation
	grpcServerMu.Unlock()
	Log(LogLevel_WARNING, LogType_CORE, "foreground gRPC readiness failed; retiring exact control server")
	go runGrpcServerStop(mode, server, previous, operation)
	select {
	case <-operation.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StartGrpcServerByMode starts a gRPC server on the specified address with mTLS.
func StartGrpcServerByMode(listenAddressG string, mode SetupMode) (*grpc.Server, error) {
	// This also protects direct callers outside Setup. The old server's Stop
	// owns every admitted RPC handler and must finish before a replacement can
	// observe or mutate process-global core state.
	for {
		waitForGrpcServerStop(mode)
		grpcServerMu.Lock()
		if grpcServerStop[mode] == nil {
			break
		}
		grpcServerMu.Unlock()
	}
	defer grpcServerMu.Unlock()

	if server := grpcServer[mode]; server != nil {
		Log(LogLevel_WARNING, LogType_CORE, "grpcServer already started")
		return server, nil
	}
	if !strings.Contains(listenAddressG, ":") {
		return nil, fmt.Errorf("invalid listen address (no port): %s", listenAddressG)
	}
	// Convert the port from string to uint16
	portStr := strings.Split(listenAddressG, ":")[1]
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("failed to convert port %s to uint16: %v", portStr, err)
	}
	if hutils.IsPortInUse(uint16(port)) {
		return nil, fmt.Errorf("port %s is already in use", portStr)
	}
	var server *grpc.Server
	if mode == SetupMode_GRPC_BACKGROUND_INSECURE || mode == SetupMode_GRPC_NORMAL_INSECURE {
		server = newRecoveredGrpcServer()
	} else {
		table := db.GetTable[hcommon.AppSettings]()
		Log(LogLevel_DEBUG, LogType_CORE, table)
		grpcServerPrivateKey, err := table.Get("grpc_server_private_key")
		grpcServerPublicKey, err2 := table.Get("grpc_server_public_key")
		if err != nil || err2 != nil {
			Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("failed to get grpc_server_private_key and grpc_server_public_key from database: %v %v\n", err, err2))
			certpair, err = hutils.GenerateCertificatePair()
			if err != nil {
				Log(LogLevel_ERROR, LogType_CORE, fmt.Sprintf("failed to generate certificate pair: %v", err))

				return nil, err
			}
			table.UpdateInsert(
				&hcommon.AppSettings{Id: "grpc_server_public_key", Value: certpair.Certificate},
				&hcommon.AppSettings{Id: "grpc_server_private_key", Value: certpair.PrivateKey},
			)
		} else {
			certpair = &hutils.CertificatePair{
				Certificate: grpcServerPublicKey.Value.([]byte),
				PrivateKey:  grpcServerPrivateKey.Value.([]byte),
			}
		}
		// Load server certificate and private key
		serverCert, err := tls.X509KeyPair(certpair.Certificate, certpair.PrivateKey)
		if err != nil {
			Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("failed to load server certificate and key: %v\n", err))

			return nil, err
		}

		// Create TLS credentials for the gRPC server
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert, // Enforce mutual TLS (mTLS)
			ClientCAs:    caCertPool,                     // Client CAs to verify client certificates
		}

		// Create a new gRPC server with TLS credentials
		creds := credentials.NewTLS(tlsConfig)
		server = newRecoveredGrpcServer(grpc.Creds(creds))
	}
	// Register each server with an immutable generation identity. This is used
	// by Setup admission to distinguish a current handler from a handler owned
	// by a server whose exact Stop is already draining.
	owner := &grpcServerOwner{mode: mode, listenAddress: listenAddressG}
	RegisterCoreServer(server, &CoreService{grpcOwner: owner})
	hello.RegisterHelloServer(server, &hello.HelloService{})
	// Listen on the provided address
	lis, err := net.Listen("tcp", listenAddressG)
	if err != nil {
		Log(LogLevel_ERROR, LogType_CORE, fmt.Sprintf("failed to listen on %s: %v\n", listenAddressG, err))
		return nil, err
	}
	grpcServer[mode] = server
	grpcServerOwn[mode] = owner
	Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("grpcServer started on %s\n", listenAddressG))
	log.Printf("Server listening on %s", listenAddressG)

	// Run the server in a goroutine
	go func() {
		defer config.DeferPanicToError("grpcsetup", func(err error) {
			Log(LogLevel_FATAL, LogType_CORE, err.Error())
			<-time.After(5 * time.Second)
		})
		serveErr := server.Serve(lis)
		if serveErr != nil {
			Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("failed to serve: %v\n", serveErr))
		}
		if retireGrpcServerAfterServe(mode, server, owner) {
			Log(LogLevel_WARNING, LogType_CORE, "gRPC serve loop ended while still registered; retired stale server")
		}
		Log(LogLevel_DEBUG, LogType_CORE, "Server stopped")
	}()

	return server, nil
}

// retireGrpcServerAfterServe removes only the exact server generation whose
// Serve loop has ended without first being unpublished by CloseGrpcServer.
// A dead listener must never remain discoverable as an active server: Setup
// would otherwise return the stale registry entry without reopening a port.
// Publish a stop barrier before calling Stop so a subsequent Setup cannot
// overlap any RPC handlers that survived the listener failure.
func retireGrpcServerAfterServe(mode SetupMode, server *grpc.Server, owner *grpcServerOwner) bool {
	grpcServerMu.Lock()
	if grpcServer[mode] != server || grpcServerOwn[mode] != owner {
		grpcServerMu.Unlock()
		return false
	}
	delete(grpcServer, mode)
	delete(grpcServerOwn, mode)
	previous := grpcServerStop[mode]
	operation := &grpcServerStopOperation{done: make(chan struct{})}
	grpcServerStop[mode] = operation
	grpcServerMu.Unlock()

	runGrpcServerStop(mode, server, previous, operation)
	return true
}

func grpcServerForMode(mode SetupMode) *grpc.Server {
	grpcServerMu.RLock()
	defer grpcServerMu.RUnlock()
	return grpcServer[mode]
}

func hasGrpcServer(mode SetupMode) bool {
	return grpcServerForMode(mode) != nil
}

// GetGrpcServerPublicKey returns the gRPC server's public key.
func GetGrpcServerPublicKey() []byte {
	return certpair.Certificate
}

// AddGrpcClientPublicKey adds a client's public key to the CA pool for verification.
func AddGrpcClientPublicKey(clientPublicKey []byte) error {
	block, _ := pem.Decode(clientPublicKey)
	if block == nil || block.Type != "PUBLIC KEY" {
		return fmt.Errorf("failed to decode client public key")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		pubKey, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("failed to parse client public key: %v", err)
		}
		cert = &x509.Certificate{
			PublicKey: pubKey,
		}
	}
	caCertPool.AddCert(cert)

	return nil
}

func CloseGrpcServer(mode SetupMode) {
	operation := requestGrpcServerStop(mode, nil)
	if operation == nil {
		return
	}
	timer := time.NewTimer(grpcServerStopTimeout)
	defer timer.Stop()
	select {
	case <-operation.done:
		Log(LogLevel_DEBUG, LogType_CORE, "gRPC server stop completed")
	case <-timer.C:
		Log(LogLevel_WARNING, LogType_CORE, "gRPC server stop remains pending after bounded close")
	}
}

// Publish the exact stop barrier without joining it. An RPC handler that
// belongs to this server must return before WaitForHandlers can finish.
func requestGrpcServerStop(mode SetupMode, expected *grpcServerOwner) *grpcServerStopOperation {
	grpcServerMu.Lock()
	defer grpcServerMu.Unlock()
	if expected != nil && grpcServerOwn[mode] != expected {
		return nil
	}
	server := grpcServer[mode]
	delete(grpcServer, mode)
	delete(grpcServerOwn, mode)
	operation := grpcServerStop[mode]
	if server != nil {
		previous := operation
		operation = &grpcServerStopOperation{done: make(chan struct{})}
		grpcServerStop[mode] = operation
		go runGrpcServerStop(mode, server, previous, operation)
	}
	return operation
}

func runGrpcServerStop(
	mode SetupMode,
	server *grpc.Server,
	previous *grpcServerStopOperation,
	operation *grpcServerStopOperation,
) {
	if previous != nil {
		<-previous.done
	}
	server.Stop()

	grpcServerMu.Lock()
	if grpcServerStop[mode] == operation {
		delete(grpcServerStop, mode)
	}
	close(operation.done)
	grpcServerMu.Unlock()
}

// waitForGrpcServerStop is intentionally exact and unbounded. Callers place
// their own bounded UI timeout around Setup; returning here while an old RPC
// handler is alive would permit two core generations to share global state.
func waitForGrpcServerStop(mode SetupMode) {
	for {
		grpcServerMu.RLock()
		operation := grpcServerStop[mode]
		grpcServerMu.RUnlock()
		if operation == nil {
			return
		}
		Log(LogLevel_DEBUG, LogType_CORE, "waiting for exact previous gRPC server stop")
		<-operation.done
	}
}

// The gRPC registry is mode-specific, but the core instance, runtime settings,
// platform interface and Start admission are process-global. A foreground
// Setup therefore also has to join a pending background-server stop (and vice
// versa) before it may reopen the next generation.
func waitForAllGrpcServerStops() {
	for {
		grpcServerMu.RLock()
		operations := make([]*grpcServerStopOperation, 0, len(grpcServerStop))
		for _, operation := range grpcServerStop {
			operations = append(operations, operation)
		}
		grpcServerMu.RUnlock()
		if len(operations) == 0 {
			return
		}
		Log(LogLevel_DEBUG, LogType_CORE, "waiting for all exact previous gRPC server stops")
		for _, operation := range operations {
			<-operation.done
		}
	}
}

// grpcSetupAdmissionSnapshot atomically checks the serving generation and
// snapshots all exact server-stop barriers. Callers must wait for the returned
// operations without holding coreLifecycle, then recheck under coreLifecycle
// before mutating process-global setup state.
func grpcSetupAdmissionSnapshot(owner *grpcServerOwner) (bool, []*grpcServerStopOperation) {
	grpcServerMu.RLock()
	defer grpcServerMu.RUnlock()

	if owner != nil && grpcServerOwn[owner.mode] != owner {
		return false, nil
	}
	operations := make([]*grpcServerStopOperation, 0, len(grpcServerStop))
	for _, operation := range grpcServerStop {
		operations = append(operations, operation)
	}
	return true, operations
}
