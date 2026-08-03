package hcore

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecoveryUnaryServerInterceptorContainsPanic(t *testing.T) {
	const panicMessage = "sensitive unary panic"

	response, err := recoveryUnaryServerInterceptor(
		context.Background(),
		nil,
		&grpc.UnaryServerInfo{FullMethod: "/hcore.Core/UnaryTest"},
		func(context.Context, any) (any, error) {
			panic(panicMessage)
		},
	)

	if response != nil {
		t.Fatalf("response = %#v, want nil", response)
	}
	assertRecoveredGrpcPanic(t, err, panicMessage)
}

func TestRecoveryStreamServerInterceptorContainsPanic(t *testing.T) {
	const panicMessage = "sensitive stream panic"

	err := recoveryStreamServerInterceptor(
		nil,
		nil,
		&grpc.StreamServerInfo{FullMethod: "/hcore.Core/StreamTest"},
		func(any, grpc.ServerStream) error {
			panic(panicMessage)
		},
	)

	assertRecoveredGrpcPanic(t, err, panicMessage)
}

func assertRecoveredGrpcPanic(t *testing.T, err error, panicMessage string) {
	t.Helper()
	if status.Code(err) != codes.Internal {
		t.Fatalf("status.Code(err) = %v, want Internal; err = %v", status.Code(err), err)
	}
	if strings.Contains(status.Convert(err).Message(), panicMessage) {
		t.Fatalf("client-facing error leaked panic details: %v", err)
	}
}
