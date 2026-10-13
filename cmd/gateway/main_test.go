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

func TestGatewayListenAddressesHonorsLanSelection(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), []byte(`{"lan":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gatewayListenAddresses(dataDir); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("lan disabled addresses=%v", got)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), []byte(`{"lan":true,"localAccess":true,"lanAddresses":["192.0.2.10","192.0.2.10"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := gatewayListenAddresses(dataDir)
	if len(got) != 2 || got[0] != "127.0.0.1" || got[1] != "192.0.2.10" {
		t.Fatalf("selected addresses=%v", got)
	}
}
