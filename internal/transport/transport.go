// Package transport is the Go port of bridge/transport.py: byte-stream
// transports for desktop IPC without changing the wire format. Endpoint
// resolution is pure and parity-checked (conformance/transport); the Windows
// named-pipe dialer lives behind a build tag.
package transport

import (
	"path/filepath"
	"runtime"
)

// Endpoint mirrors ipc_endpoint: the fixed local pipe name on Windows, the
// codex-home socket everywhere else.
func Endpoint(goos, codexHome string) string {
	if goos == "windows" {
		return `\\.\pipe\codex-ipc`
	}
	return filepath.Join(codexHome, "ipc", "ipc.sock")
}

// CurrentOSEndpoint resolves the endpoint for the running platform.
func CurrentOSEndpoint(codexHome string) string {
	return Endpoint(runtime.GOOS, codexHome)
}
