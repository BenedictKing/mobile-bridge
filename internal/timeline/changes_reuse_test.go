package timeline

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// A later generation that only adds structured changes or an exitCode to an
// otherwise identical message must invalidate incremental row reuse: text
// and status are unchanged in exactly the tool_call_update-with-diff and
// command-finished cases, so a fingerprint that ignored these fields would
// keep the pre-diff encoding forever.
func TestIncrementalReuseInvalidatesOnChangesAndExitCode(t *testing.T) {
	tl := New(func() string { return "epoch1" })
	edit := `{"id": "m1", "kind": "toolCall", "role": "activity", "title": "编辑", "text": "edit"}`
	command := `{"id": "m2", "kind": "toolCall", "role": "activity", "title": "执行命令", "text": "go test"}`
	updateWith(t, tl, `{"sequence": 1, "turns": [{"id": "t1", "status": "inProgress", "messages": [`+edit+`,`+command+`]}]}`)
	first := tl.Page(20, "")
	if row := rowByTitle(first, "编辑"); row == nil {
		t.Fatal("edit row missing")
	} else if _, has := row.Get("changes"); has {
		t.Fatal("changes present before they exist")
	}

	editWithChanges := `{"id": "m1", "kind": "toolCall", "role": "activity", "title": "编辑", "text": "edit",
	 "changes": [{"path": "/work/a.go", "oldText": "o", "newText": "n"}]}`
	commandWithExit := `{"id": "m2", "kind": "toolCall", "role": "activity", "title": "执行命令", "text": "go test", "exitCode": 0}`
	updateWith(t, tl, `{"sequence": 2, "turns": [{"id": "t1", "status": "inProgress", "messages": [`+editWithChanges+`,`+commandWithExit+`]}]}`)
	second := tl.Page(20, "")
	editRow := rowByTitle(second, "编辑")
	if editRow == nil {
		t.Fatal("edit row missing after update")
	}
	if changes, ok := editRow.MustGet("changes").(*jsoncompat.Array); !ok || len(changes.Items) != 1 {
		t.Fatalf("changes not projected after reuse: %s", jsoncompat.EncodeCompact(editRow))
	}
	commandRow := rowByTitle(second, "执行命令")
	if commandRow == nil {
		t.Fatal("command row missing after update")
	}
	if _, has := commandRow.Get("exitCode"); !has {
		t.Fatalf("exitCode not projected after reuse: %s", jsoncompat.EncodeCompact(commandRow))
	}
	// Incremental readers must see both rows as changed.
	changed := tl.Changes("1", "epoch1", "")
	rows, _ := changed.MustGet("rows").(*jsoncompat.Array)
	if rows == nil || len(rows.Items) != 2 {
		t.Fatalf("changes frame should carry both rows: %s", jsoncompat.EncodeCompact(changed))
	}
}
