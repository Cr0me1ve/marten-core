package hcore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupPlaintextRuntimeConfig(t *testing.T) {
	workingDir := t.TempDir()
	dataDir := filepath.Join(workingDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dataDir, "current-config.json")
	if err := os.WriteFile(configPath, []byte(`{"outbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	previousWorkingPath := sWorkingPath
	sWorkingPath = workingDir
	t.Cleanup(func() { sWorkingPath = previousWorkingPath })

	cleanupPlaintextRuntimeConfig()
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("plaintext runtime config still exists: %v", err)
	}

	// Cleanup is intentionally idempotent across stop/recovery/crash paths.
	cleanupPlaintextRuntimeConfig()
}
