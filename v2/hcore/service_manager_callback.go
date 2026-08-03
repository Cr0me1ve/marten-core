package hcore

import (
	"github.com/Cr0me1ve/marten-core/v2/service_manager"
	"github.com/sagernet/sing-box/adapter"
)

type martenMainServiceManager struct{}

var _ adapter.Service = (*martenMainServiceManager)(nil)

func (h *martenMainServiceManager) Start() error {
	return service_manager.OnMainServiceStart()
}

func (h *martenMainServiceManager) Close() error {
	return service_manager.OnMainServiceClose()
}
