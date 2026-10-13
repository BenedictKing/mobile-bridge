package transport

import "testing"

// TestEndpoint mirrors bridge/transport.py's ipc_endpoint contract: the fixed
// local pipe name on Windows, the codex-home socket elsewhere.
func TestEndpoint(t *testing.T) {
	if got := Endpoint("windows", "/unused"); got != `\\.\pipe\codex-ipc` {
		t.Errorf("windows endpoint = %q", got)
	}
	if got := Endpoint("darwin", "/Users/x/.codex"); got != "/Users/x/.codex/ipc/ipc.sock" {
		t.Errorf("darwin endpoint = %q", got)
	}
	if got := Endpoint("linux", "/home/x/.codex"); got != "/home/x/.codex/ipc/ipc.sock" {
		t.Errorf("linux endpoint = %q", got)
	}
}
