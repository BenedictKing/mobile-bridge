package httpd

import (
	"errors"
	"strings"
	"testing"
)

func TestACPSessionMessageTranslatesOwnership(t *testing.T) {
	raw := errors.New(`Internal error: cannot restore session session-x: session "session-x" is already owned by an active write handle`)
	message := acpSessionMessage(raw)
	if !strings.Contains(message, "桌面 App") || !strings.Contains(message, "切换") {
		t.Fatalf("ownership message = %q", message)
	}
	if plain := acpSessionMessage(errors.New("boom")); plain != "boom" {
		t.Fatalf("pass-through = %q", plain)
	}
	if empty := acpSessionMessage(nil); empty != "" {
		t.Fatalf("nil = %q", empty)
	}
}
