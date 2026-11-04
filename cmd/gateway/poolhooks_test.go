package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func writePoolHookAgent(t *testing.T, script string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, path}
}

func waitPoolTurnSettled(t *testing.T, session *acpruntime.Session) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for session.PromptRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if session.PromptRunning() {
		t.Fatal("turn did not settle")
	}
}

func waitPoolPrompts(t *testing.T, path string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(path)
		if text := strings.TrimSpace(string(raw)); text != "" {
			lines := strings.Split(text, "\n")
			if len(lines) >= want {
				return lines
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("prompt count did not reach %d", want)
	return nil
}

func TestResumePoolSessionsAfterSwitchAssessesAndContinues(t *testing.T) {
	var assessments atomic.Int32
	var recoveryNote atomic.Bool
	jevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assessments.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		state, _ := payload["state"].(map[string]any)
		recoveryNote.Store(state["recoveryNote"] != "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"complete":{"noul":0.1},"stop_kind":{"choice":"quota"},"continue":{"choice":"continue","confidence":0.9}}}`))
	}))
	defer jevServer.Close()

	settings := jev.New(filepath.Join(t.TempDir(), "jev.json"))
	config := jsoncompat.NewObject()
	config.Set("apiKey", "test-key-123456")
	config.Set("baseUrl", jevServer.URL)
	config.Set("autoContinue", true)
	if _, err := settings.Update(config); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	promptsPath := filepath.Join(dir, "prompts.jsonl")
	script := fmt.Sprintf(`import json,sys
prompts=%q
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load":
  result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/prompt":
  blocks=m.get("params",{}).get("prompt",[])
  text="".join(block.get("text","") for block in blocks)
  with open(prompts,"a") as f: f.write(json.dumps({"text":text})+"\n")
  with open(prompts) as f: count=len(f.readlines())
  if count == 1:
   print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32603,"message":"usage limit reached"}}),flush=True)
   continue
  result={"stopReason":"end_turn"}
 else:
  result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`, promptsPath)
	manager := acpruntime.NewCommand(writePoolHookAgent(t, script))
	manager.Kind = "zcode"
	defer manager.Close()
	session, err := manager.Open("sess-switch", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Prompt("完成任务", time.Hour); err != nil {
		t.Fatal(err)
	}
	waitPoolTurnSettled(t, session)

	resumePoolSessionsAfterSwitch(manager, jevContinueAfterSwitchHook(settings, "zcode"))

	if assessments.Load() != 1 {
		t.Fatalf("Jev assessments = %d, want 1", assessments.Load())
	}
	if !recoveryNote.Load() {
		t.Fatal("Jev did not receive the resolved-account context")
	}
	lines := waitPoolPrompts(t, promptsPath, 2)
	var resumed struct{ Text string }
	if err := json.Unmarshal([]byte(lines[1]), &resumed); err != nil || resumed.Text != "继续" {
		t.Fatalf("unexpected switch resume prompt: %s", lines[1])
	}
}
