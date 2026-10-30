package acpruntime

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// creatingAgent answers session/new with a fixed id and (optionally) lists it
// later — the shape of a chat the gateway created but never prompted.
const creatingAgent = `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-created"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`

func TestCreatedStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created-sessions.json")
	store := LoadCreated(path)
	store.Remember("fake", "sess-1", "/work", "团队 · 成员", 1700000000000)
	store.Remember("other", "sess-1", "/other", "", 1700000000001)

	reloaded := LoadCreated(path)
	record, ok := reloaded.Lookup("fake", "sess-1")
	if !ok || record.Cwd != "/work" || record.Title != "团队 · 成员" {
		t.Fatalf("record = %v ok=%v", record, ok)
	}
	// Session ids are only unique per agent: the namespace must be respected.
	if _, ok := reloaded.Lookup("missing", "sess-1"); ok {
		t.Fatal("created session leaked across agents")
	}
	rows := reloaded.Rows("fake")
	if len(rows) != 1 || rows[0].MustGet("sessionId") != "sess-1" ||
		rows[0].MustGet("cwd") != "/work" || rows[0].MustGet("title") != "团队 · 成员" {
		t.Fatalf("rows = %v", rows)
	}

	reloaded.Forget("fake", "sess-1")
	if _, ok := LoadCreated(path).Lookup("fake", "sess-1"); ok {
		t.Fatal("forgotten session survived a reload")
	}
}

func TestCreatedSessionStaysKnownAfterRestart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "created-sessions.json")
	manager := NewCommand(writeFakeAgent(t, creatingAgent))
	manager.Created = LoadCreated(storePath)
	manager.Kind = "fake"
	created, err := manager.Create("req-1", "/work", "团队 · 成员", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "sess-created" {
		t.Fatalf("created id = %s", created.ID)
	}
	manager.Close()

	// Restart: no live handle and no session/list row (the agent lists a chat
	// only after its first turn), yet the created ledger still knows the chat.
	restarted := NewCommand(writeFakeAgent(t, creatingAgent))
	restarted.Created = LoadCreated(storePath)
	restarted.Kind = "fake"
	defer restarted.Close()
	if !restarted.Knows("sess-created") {
		t.Fatal("created session lost its ownership after restart")
	}
	if cwd := restarted.CwdFor("sess-created"); cwd != "/work" {
		t.Fatalf("cwd = %q", cwd)
	}
	rows := restarted.MergePending(nil)
	if len(rows) != 1 {
		t.Fatalf("pending rows = %v", rows)
	}
	if rows[0].MustGet("sessionId") != "sess-created" ||
		rows[0].MustGet("title") != "团队 · 成员" || rows[0].MustGet("cwd") != "/work" {
		t.Fatalf("pending row = %v", rows[0])
	}
	if _, ok := restarted.Created.Lookup("fake", "sess-created"); !ok {
		t.Fatal("created record disappeared")
	}
	// Reopening the chat gives it a live handle without a creation timestamp;
	// the created row must still be the one carrying its list entry.
	restarted.sessions["sess-created"] = &Session{
		ID: "sess-created", Cwd: "/work",
		View: acpview.NewView("sess-created", nil, "/work", func() float64 { return 1 }),
	}
	if rows := restarted.MergePending(nil); len(rows) != 1 ||
		rows[0].MustGet("title") != "团队 · 成员" {
		t.Fatalf("reopened chat lost its created row: %v", rows)
	}
	// The phone's list projection dedupes once the agent lists the session.
	listed := []*jsoncompat.Object{jsoncompat.NewObject()}
	listed[0].Set("sessionId", "sess-created")
	listed[0].Set("title", "已列出")
	listed[0].Set("cwd", "/work")
	if rows := restarted.MergePending(listed); len(rows) != 1 {
		t.Fatalf("listed session duplicated: %v", rows)
	}
}

func TestCreatedStoreIgnoresEmptyIDs(t *testing.T) {
	store := LoadCreated(filepath.Join(t.TempDir(), "created-sessions.json"))
	store.Remember("fake", "", "/work", "", 1)
	if rows := store.Rows("fake"); len(rows) != 0 {
		t.Fatalf("rows = %v", rows)
	}
	if _, ok := store.Lookup("fake", ""); ok {
		t.Fatal("empty id was stored")
	}
	var decoded createdLedger
	if err := json.Unmarshal([]byte(`{"version":99,"sessions":{"fake:sess":{"id":"sess"}}}`), &decoded); err != nil {
		t.Fatal(err)
	}
}
