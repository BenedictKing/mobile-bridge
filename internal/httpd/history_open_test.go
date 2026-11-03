package httpd

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestDSHReadRoutesUseNativeHistoryWithoutStartingAgent(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	marker := filepath.Join(t.TempDir(), "agent-started")
	t.Setenv("ACP_HTTP_START_MARKER", marker)
	manager := acpruntime.NewCommand([]string{python, "-u", "-c",
		`import os; open(os.environ["ACP_HTTP_START_MARKER"], "w").write("started")`})
	manager.NativeListExclusive = true
	manager.NativeList = func() []*jsoncompat.Object {
		row := jsoncompat.NewObject()
		row.Set("sessionId", "session-history")
		row.Set("cwd", "/work")
		return []*jsoncompat.Object{row}
	}
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		turn := jsoncompat.NewObject()
		turn.Set("id", "turn-history")
		turn.Set("status", "completed")
		message := jsoncompat.NewObject()
		message.Set("id", "user-history")
		message.Set("kind", "userMessage")
		message.Set("role", "user")
		message.Set("text", "只读历史问题")
		turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
		return &acpview.NativeSnapshot{Title: "DSH 原生历史", Turns: []*jsoncompat.Object{turn}}, nil
	}
	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"dsh": manager}
	t.Cleanup(manager.Close)
	server.PreloadRecentSessions()

	view := featureRequest(server, token, csrf, http.MethodGet,
		"/api/sessions/session-history?agent=dsh&cwd=/work", "")
	if view.Code != http.StatusOK {
		t.Fatalf("view status=%d body=%s", view.Code, view.Body.String())
	}
	for _, needle := range []string{"DSH 原生历史", "只读历史问题", `"readOnly": true`} {
		if !strings.Contains(view.Body.String(), needle) {
			t.Fatalf("view missing %q: %s", needle, view.Body.String())
		}
	}
	timeline := featureRequest(server, token, csrf, http.MethodGet,
		"/api/sessions/session-history/timeline?limit=20", "")
	if timeline.Code != http.StatusOK || !strings.Contains(timeline.Body.String(), "只读历史问题") {
		t.Fatalf("timeline status=%d body=%s", timeline.Code, timeline.Body.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read routes started the agent: %v", err)
	}
}
