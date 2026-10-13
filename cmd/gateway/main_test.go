package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestFreshWorkerSnapshotSeedsConfigWithoutReturningPasswordHash(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "gateway")
	snapshot := workerSnapshot(dataDir)
	authValue, ok := snapshot.MustGet("auth").(*jsoncompat.Object)
	if !ok || authValue.MustGet("username") != "admin" || authValue.MustGet("mode") != "password" {
		t.Fatalf("missing public auth snapshot: %s", jsoncompat.EncodePythonDefault(snapshot))
	}
	if _, exists := authValue.Get("hash"); exists {
		t.Fatal("password hash leaked in desktop snapshot")
	}
	if _, exists := authValue.Get("salt"); exists {
		t.Fatal("password salt leaked in desktop snapshot")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "首次登录.txt")); err != nil {
		t.Fatalf("first-run credential file missing: %v", err)
	}
}
