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
	"google.golang.org/grpc/status"

	"github.com/Cr0me1ve/marten-core/v2/db"
	_ "google.golang.org/grpc/encoding/gzip"
)

type CoreService struct {
	UnimplementedCoreServer
}

func newRecoveredGrpcServer(options ...grpc.ServerOption) *grpc.Server {
	recoveryOptions := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(recoveryUnaryServerInterceptor),
		grpc.ChainStreamInterceptor(recoveryStreamServerInterceptor),
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
	certpair     *hutils.CertificatePair
	grpcServer   map[SetupMode]*grpc.Server = make(map[SetupMode]*grpc.Server)
	grpcServerMu sync.RWMutex
	caCertPool   = x509.NewCertPool()
)

// StartGrpcServerByMode starts a gRPC server on the specified address with mTLS.
func StartGrpcServerByMode(listenAddressG string, mode SetupMode) (*grpc.Server, error) {
	grpcServerMu.Lock()
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
	// Register your gRPC service here
	RegisterCoreServer(server, &CoreService{})
	hello.RegisterHelloServer(server, &hello.HelloService{})
	// Listen on the provided address
	lis, err := net.Listen("tcp", listenAddressG)
	if err != nil {
		Log(LogLevel_ERROR, LogType_CORE, fmt.Sprintf("failed to listen on %s: %v\n", listenAddressG, err))
		return nil, err
	}
	grpcServer[mode] = server
	Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("grpcServer started on %s\n", listenAddressG))
	log.Printf("Server listening on %s", listenAddressG)

	// Run the server in a goroutine
	go func() {
		defer config.DeferPanicToError("grpcsetup", func(err error) {
			Log(LogLevel_FATAL, LogType_CORE, err.Error())
			<-time.After(5 * time.Second)
		})
		if err := server.Serve(lis); err != nil {
			Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf("failed to serve: %v\n", err))
		}
		Log(LogLevel_DEBUG, LogType_CORE, "Server stopped")
	}()

	return server, nil
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
	grpcServerMu.Lock()
	server := grpcServer[mode]
	delete(grpcServer, mode)
	grpcServerMu.Unlock()

	if server != nil {
		server.Stop()
	}
}
