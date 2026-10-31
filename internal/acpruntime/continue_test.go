package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func fakeAgentScript(t *testing.T) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
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
	return python, path
}

func waitSettled(t *testing.T, session *Session) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		running := session.promptRunning
		session.mu.Unlock()
		if !running {
			return
		}
		session.Wait(300 * time.Millisecond)
	}
	t.Fatal("turn never settled")
}

func TestAutoContinuePromptEnforcesBudget(t *testing.T) {
	python, path := fakeAgentScript(t)
	manager := NewCommand([]string{python, "-u", path})
	defer manager.Close()
	session, err := manager.Open("sess-continue", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Prompt("开始", time.Hour); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	for i := 0; i < MaxAutoContinue; i++ {
		if err := session.AutoContinuePrompt("继续", time.Hour); err != nil {
			t.Fatalf("auto continue %d failed: %v", i+1, err)
		}
		waitSettled(t, session)
	}
	if err := session.AutoContinuePrompt("继续", time.Hour); err == nil || err.Error() != "连续自动继续已达上限" {
		t.Fatalf("budget must be exhausted, got %v", err)
	}
	// A human message re-arms the budget.
	if err := session.Prompt("人工接手", time.Hour); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if err := session.AutoContinuePrompt("继续", time.Hour); err != nil {
		t.Fatalf("human message should re-arm the budget: %v", err)
	}
	waitSettled(t, session)
}

func TestAutoContinueHookFiresOnSettledTurns(t *testing.T) {
	python, path := fakeAgentScript(t)
	manager := NewCommand([]string{python, "-u", path})
	settled := make(chan *Session, 8)
	manager.AutoContinue = func(session *Session) {
		settled <- session
	}
	session, err := manager.Open("sess-hook", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The hook must be installed on opened sessions by construction time.
	session.mu.Lock()
	hook := session.autoContinue
	session.mu.Unlock()
	if hook == nil {
		t.Fatal("auto-continue hook not copied onto the session")
	}
	if err := session.Prompt("开始", time.Hour); err != nil {
		t.Fatal(err)
	}
	select {
	case hooked := <-settled:
		if hooked != session {
			t.Fatal("hook received a different session")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("settle hook never fired")
	}
	waitSettled(t, session)
}

func TestNoteHumanActionRearmsWithoutPrompt(t *testing.T) {
	session := &Session{ID: "sess-budget", View: nil, changed: make(chan struct{})}
	for i := 0; i < MaxAutoContinue; i++ {
		session.mu.Lock()
		session.autoContinueRuns++
		session.mu.Unlock()
	}
	session.NoteHumanAction()
	session.mu.Lock()
	runs := session.autoContinueRuns
	session.mu.Unlock()
	if runs != 0 {
		t.Fatalf("human action should re-arm budget, runs=%d", runs)
	}
}

func TestAutoApproveReceivesSessionContext(t *testing.T) {
	session := &Session{ID: "sess-approve", View: acpview.NewView("sess-approve", nil, "/work", func() float64 { return 1 }), pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{})}
	seen := make(chan [3]string, 1)
	session.autoApprove = func(sessionID, requestID, method string, params *jsoncompat.Object) *ApprovalVerdict {
		seen <- [3]string{sessionID, requestID, method}
		return nil
	}
	request := jsoncompat.NewObject()
	request.Set("id", "perm-7")
	request.Set("method", "session/request_permission")
	request.Set("params", jsoncompat.NewObject())
	responded := make(chan struct{}, 1)
	session.onRequest(request, func(any) { responded <- struct{}{} })
	select {
	case got := <-seen:
		if got[0] != "sess-approve" || got[1] != "perm-7" || got[2] != "session/request_permission" {
			t.Fatalf("auto-approve context missing: session=%q request=%q method=%q", got[0], got[1], got[2])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("auto-approve evaluation never ran")
	}
	select {
	case <-responded:
		t.Fatal("request should stay pending without an auto response")
	default:
	}
}

func TestAutoApproveVerdictAnnotatesPendingCard(t *testing.T) {
	session := &Session{ID: "sess-note", View: acpview.NewView("sess-note", nil, "/work", func() float64 { return 1 }), pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{})}
	assessment := jsoncompat.NewObject()
	assessment.Set("source", "jev")
	assessment.Set("reason", "low_confidence")
	session.autoApprove = func(sessionID, requestID, method string, params *jsoncompat.Object) *ApprovalVerdict {
		return &ApprovalVerdict{Assessment: assessment, RecommendOption: "approve_once"}
	}
	request := jsoncompat.NewObject()
	request.Set("id", "perm-8")
	request.Set("method", "session/request_permission")
	request.Set("params", jsoncompat.NewObject())
	session.onRequest(request, func(any) { t.Error("annotated card must stay pending") })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		var card *jsoncompat.Object
		if len(session.View.Requests) == 1 {
			card = session.View.Requests[0]
		}
		annotated := card != nil && card.MustGet("assessment") != nil
		session.mu.Unlock()
		if annotated {
			if card.MustGet("recommend") != "approve_once" {
				t.Fatalf("recommend not set: %s", jsoncompat.EncodeCompact(card))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verdict never reached the pending card")
}
