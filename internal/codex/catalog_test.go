package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// fakeAppServer is a read-only codex app-server: two model pages, chatgpt
// auth with fast-mode requirements, one project skill, thread/name/set.
func fakeAppServer(t *testing.T) string {
	t.Helper()
	script := `#!/usr/bin/env python3
import json,sys
MODELS=[{"model":"gpt-5.3","displayName":"GPT-5.3","description":"flagship","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"}],"defaultReasoningEffort":"high","serviceTiers":[{"id":"fast","name":"Fast"}]},
        {"model":"gpt-5-mini","displayName":"Mini"},{"model":"secret","hidden":True}]
SKILLS=[{"errors":["one skill failed to load"],"skills":[{"name":"pdf","description":"PDF toolkit","path":"/opt/skills/pdf","enabled":True,"scope":"project"},{"name":"disabled","path":"/opt/skipped","enabled":False}]}]
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize": result={"ok":True}
 elif method=="config/read": result={"config":{"model_provider":"openai","model_reasoning_effort":"high","service_tier":"priority"}}
 elif method=="account/read": result={"account":{"type":"chatgpt"},"requiresOpenaiAuth":True}
 elif method=="configRequirements/read": result={"requirements":{"featureRequirements":{"fast_mode":True}}}
 elif method=="model/list":
  cursor=(m.get("params") or {}).get("cursor")
  if cursor is None: result={"data":MODELS[:2],"nextCursor":"page2"}
  else: result={"data":MODELS[2:],"nextCursor":None}
 elif method=="skills/list": result={"data":SKILLS}
 elif method=="thread/name/set": result={"ok":True}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "app-server")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCatalogModelsAndFastMode(t *testing.T) {
	bridge := &Bridge{Home: t.TempDir(), Executable: fakeAppServer(t)}
	value, err := bridge.Catalog().Models(t.TempDir(), "", false)
	if err != nil {
		t.Fatalf("models err=%v", err)
	}
	body := string(jsoncompat.EncodePythonDefault(value))
	if !strings.Contains(body, `"id": "gpt-5.3"`) || !strings.Contains(body, `"id": "gpt-5-mini"`) {
		t.Fatalf("models body=%s", body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("hidden model leaked: %s", body)
	}
	for _, needle := range []string{`"fastTier": "fast"`, `"allowed": true`, `"defaultServiceTier": "priority"`,
		`"currentEffort": "high"`, `"efforts": ["low", "high"]`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("models body missing %s: %s", needle, body)
		}
	}
	// The catalog cache serves the second read without a new process.
	again, err := bridge.Catalog().Models(cwd2(value), "", false)
	if err != nil || len(again.MustGet("models").(*jsoncompat.Array).Items) != 2 {
		t.Fatalf("cached read err=%v", err)
	}
}

func cwd2(_ *jsoncompat.Object) string { return os.TempDir() }

func TestCatalogSkillsAndValidate(t *testing.T) {
	bridge := &Bridge{Home: t.TempDir(), Executable: fakeAppServer(t)}
	cwd := t.TempDir()
	rows, problems, err := bridge.Catalog().Skills(cwd, false)
	if err != nil {
		t.Fatalf("skills err=%v", err)
	}
	if len(rows) != 1 || rows[0].Name != "pdf" || rows[0].Path != "/opt/skills/pdf" || len(problems) != 1 {
		t.Fatalf("rows=%+v problems=%v", rows, problems)
	}
	resolved, err := bridge.Catalog().ValidateSkills(cwd, []string{rows[0].ID}, false)
	if err != nil || len(resolved) != 1 || resolved[0]["name"] != "pdf" || resolved[0]["path"] != "/opt/skills/pdf" {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	if _, err := bridge.Catalog().ValidateSkills(cwd, []string{"missing"}, false); err == nil ||
		err.Error() != "Skill 不可用，请刷新列表" {
		t.Fatalf("missing skill err=%v", err)
	}
}

func TestRenameConversation(t *testing.T) {
	bridge := &Bridge{Home: t.TempDir(), Executable: fakeAppServer(t)}
	if err := bridge.RenameConversation("0d1f2e3e-1111-4222-8333-444455556666", "新名字"); err != nil {
		t.Fatalf("rename err=%v", err)
	}
}
