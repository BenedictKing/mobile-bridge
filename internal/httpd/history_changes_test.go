// Saved-rollout structured changes reach the phone: a temp CODEX_HOME whose
// rollout carries FileChange/CommandExecution payloads flows through the
// historicalTimeline equivalent — Store.HistoryFor → buildHistoryView
// (normalize + annotateViewChanges) → timeline.Update → Page rows with the
// structured changes/exitCode the file cards and ✓/✗ badges render.
package httpd

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	_ "modernc.org/sqlite"
)

func TestHistoricalTimelineChangeRows(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, "sessions", "rollout.jsonl")
	raw := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"改一下"}]}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-1","command":["/bin/zsh","-lc","go test ./..."],"status":"failed","aggregated_output":"FAIL","exit_code":2}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"exec-2","status":"completed","changes":{"/work/a.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-old\n+new","move_path":null},"/work/new.txt":{"type":"add","content":"新增全文"}}}}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`
	if err := os.WriteFile(rollout, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO threads VALUES ('desktop-1','历史','/work',1,0,'Codex Desktop','desktop',?)`, rollout)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	history, err := store.NewSessionStore(home).HistoryFor("desktop-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	view := buildHistoryView(history)
	projection := timeline.New(nil)
	projection.Update(view)
	page := projection.Page(20, "")

	var sawChanges, sawExitCode bool
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	for _, rowValue := range rows.Items {
		row, _ := rowValue.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		if changes, ok := row.MustGet("changes").(*jsoncompat.Array); ok {
			sawChanges = true
			if len(changes.Items) != 2 {
				t.Fatalf("changes rows=%s", jsoncompat.EncodeCompact(changes))
			}
			update, _ := changes.Items[0].(*jsoncompat.Object)
			if update.MustGet("path") != "/work/a.go" {
				t.Fatalf("update entry=%s", jsoncompat.EncodeCompact(update))
			}
			if diff, _ := update.MustGet("diff").(string); diff == "" {
				t.Fatalf("unified diff lost: %s", jsoncompat.EncodeCompact(update))
			}
			added, _ := changes.Items[1].(*jsoncompat.Object)
			if added.MustGet("path") != "/work/new.txt" || added.MustGet("newText") != "新增全文" {
				t.Fatalf("added entry=%s", jsoncompat.EncodeCompact(added))
			}
		}
		if value, ok := row.Get("exitCode"); ok {
			sawExitCode = true
			code, isNumber := value.(json.Number)
			if !isNumber || code.String() != "2" {
				t.Fatalf("exitCode=%T %v", value, value)
			}
			if text, _ := row.MustGet("text").(string); !strings.Contains(text, "go test ./...") {
				t.Fatalf("command text=%q", text)
			}
		}
	}
	if !sawChanges || !sawExitCode {
		t.Fatalf("rows missing structured fields: %s", jsoncompat.EncodeCompact(rows))
	}
}
