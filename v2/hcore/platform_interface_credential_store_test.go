package hcore

import "testing"

func TestMobilePlatformCredentialStoreNilContractFailsClosed(t *testing.T) {
	platform := &MobilePlatformInterface{}
	value, err := platform.LoadTurncoatCredential("storage-key")
	if err != nil || value != "" {
		t.Fatalf("nil platform load = (%q, %v), want empty successful cache miss", value, err)
	}
	swapped, err := platform.CompareAndSwapTurncoatCredential("storage-key", "", "replacement")
	if err != nil || swapped {
		t.Fatalf("nil platform CAS = (%v, %v), want false successful fail-closed result", swapped, err)
	}
}
