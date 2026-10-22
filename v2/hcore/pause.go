package hcore

import (
	"context"

	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
)

func (s *CoreService) Pause(ctx context.Context, pauseReq *PauseRequest) (*hcommon.Empty, error) {
	if pauseReq == nil {
		return nil, nil
	}
	// Registry retirement is atomic and does not mutate core state. Do not
	// hold coreLifecycle or join the stop operation from its own RPC handler.
	requestGrpcServerStop(pauseReq.Mode, s.grpcOwner)
	return &hcommon.Empty{}, nil
}
