package acpruntime

import "testing"

// An agent that advertises neither modes nor config options must make SetMode
// fail cleanly: findCategory used to dereference the nil option list, which
// crashed the HTTP send path and (via the queue worker) the whole gateway.
func TestSetModeWithoutModeSurfaceFailsCleanly(t *testing.T) {
	session := &Session{ID: "sess-nil-mode"}
	err := session.SetMode("default")
	if err == nil {
		t.Fatal("SetMode without any mode surface must fail")
	}
	if err.Error() != "当前 Agent 不支持此工作模式" {
		t.Fatalf("unexpected error: %v", err)
	}
}
