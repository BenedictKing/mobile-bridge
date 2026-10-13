package main

import (
	"path/filepath"
	"testing"
)

func TestResolveGatewayHonorsExplicitPath(t *testing.T) {
	if got := resolveGateway("/custom/gateway"); got != "/custom/gateway" {
		t.Fatalf("gateway=%q", got)
	}
}

func TestResolveDataDirHonorsEnvironment(t *testing.T) {
	want := filepath.Join(t.TempDir(), "data")
	t.Setenv("MOBILE_BRIDGE_DATA_DIR", want)
	if got := resolveDataDir(); got != want {
		t.Fatalf("data directory=%q, want %q", got, want)
	}
}
