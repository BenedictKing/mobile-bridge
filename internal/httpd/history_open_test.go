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
	for _, needle := range []string{"DSH 原生历史", "只读历史问题"} {
		if !strings.Contains(view.Body.String(), needle) {
			t.Fatalf("view missing %q: %s", needle, view.Body.String())
		}
	}
	// An attachable history view is NOT flagged read-only: the composer stays
	// live and the first mutation attaches in place. Only a desktop-owned
	// view carries the read-only banner.
	if strings.Contains(view.Body.String(), `"readOnly": true`) {
		t.Fatalf("history view flagged read-only: %s", view.Body.String())
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

func TestNativeAgentsReadRoutesNeverStartProcess(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	for _, kind := range []string{"zcode", "claude", "workbuddy", "kimi", "mcode", "opencode"} {
		t.Run(kind, func(t *testing.T) {
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
				return &acpview.NativeSnapshot{Title: kind + " 原生历史", Turns: []*jsoncompat.Object{turn}}, nil
			}
			server, token, csrf := newFeatureServer(t, t.TempDir())
			server.ACPAgents = map[string]*acpruntime.Manager{kind: manager}
			t.Cleanup(manager.Close)
			server.PreloadRecentSessions()

			view := featureRequest(server, token, csrf, http.MethodGet,
				"/api/sessions/session-history?agent="+kind+"&cwd=/work", "")
			if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), kind+" 原生历史") {
				t.Fatalf("view status=%d body=%s", view.Code, view.Body.String())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read route started %s: %v", kind, err)
			}
		})
	}
}

// Reading opens pure history (no process); the first send attaches in place,
// so the phone can intervene in any listed chat without a manual upgrade.
func TestNativeHistorySendAttachesInPlace(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	marker := filepath.Join(t.TempDir(), "agent-started")
	t.Setenv("ACP_HTTP_START_MARKER", marker)
	script := `import json,os,sys
open(os.environ["ACP_HTTP_START_MARKER"], "w").write("started")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/prompt": result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	manager.NativeListExclusive = true
	manager.NativeList = func() []*jsoncompat.Object {
		row := jsoncompat.NewObject()
		row.Set("sessionId", "session-attachable")
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
		message.Set("text", "历史里的问题")
		turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
		return &acpview.NativeSnapshot{Title: "可接入历史", Turns: []*jsoncompat.Object{turn}}, nil
	}
	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"claude": manager}
	t.Cleanup(manager.Close)
	server.PreloadRecentSessions()

	view := featureRequest(server, token, csrf, http.MethodGet,
		"/api/sessions/session-attachable?agent=claude&cwd=/work", "")
	if view.Code != http.StatusOK || strings.Contains(view.Body.String(), `"readOnly": true`) {
		t.Fatalf("view status=%d body=%s", view.Code, view.Body.String())
	}
	// The jump panel rides on timeline meta: every prompt is listed even
	// though the loaded window only holds the newest rows.
	page := featureRequest(server, token, csrf, http.MethodGet,
		"/api/sessions/session-attachable/timeline?limit=1&agent=claude&cwd=/work", "")
	for _, needle := range []string{`"userIndex"`, "历史里的问题"} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), needle) {
			t.Fatalf("timeline status=%d missing %q: %s", page.Code, needle, page.Body.String())
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read route started the agent: %v", err)
	}

	send := featureRequest(server, token, csrf, http.MethodPost,
		"/api/sessions/session-attachable/send?agent=claude&cwd=/work", `{"id":"1f0e1ad4-9a53-4f3a-8b52-3ab6cb9f10d1","text":"手机直接介入","mode":"send"}`)
	if send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("send did not attach the agent: %v", err)
	}
}
