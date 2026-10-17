package timeline

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func updateWith(t *testing.T, tl *Timeline, raw string) {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	tl.Update(value.(*jsoncompat.Object))
}

func rowByTitle(page *jsoncompat.Object, title string) *jsoncompat.Object {
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	for _, rowValue := range rows.Items {
		row, _ := rowValue.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		if rowTitle, _ := row.MustGet("title").(string); rowTitle == title {
			return row
		}
	}
	return nil
}

// Structured change entries and command exit codes ride through to the row
// projection, bounded; rows without the fields keep their exact shape
// (golden scenarios never set these fields, so their bytes are unchanged).
func TestUpdatePassthroughChanges(t *testing.T) {
	tl := New(func() string { return "epoch1" })
	longText := strings.Repeat("字", DetailChars+500)
	// 22 entries keep the cap test under the page budget; the truncation
	// case lives on its own message.
	changes := make([]string, 0, 22)
	for index := 0; index < 22; index++ {
		changes = append(changes,
			`{"path": "/work/f`+strings.Repeat("x", index)+`.go", "oldText": "o", "newText": "n`+strings.Repeat("!", index)+`"}`)
	}
	updateWith(t, tl, `{
	 "sequence": 1, "cwd": "/work",
	 "turns": [{"id": "t1", "status": "completed", "messages": [
	  {"id": "m1", "kind": "toolCall", "role": "activity", "title": "编辑", "text": "",
	   "changes": [`+strings.Join(changes, ",")+`]},
	  {"id": "m2", "kind": "toolCall", "role": "activity", "title": "执行命令", "text": "go test", "exitCode": 3},
	  {"id": "m3", "kind": "toolCall", "role": "activity", "title": "普通", "text": "无字段"}
	 ]}]
	}`)
	page := tl.Page(20, "")
	edit := rowByTitle(page, "编辑")
	command := rowByTitle(page, "执行命令")
	plain := rowByTitle(page, "普通")
	if edit == nil || command == nil || plain == nil {
		t.Fatal("rows incomplete")
	}

	// At most 20 entries survive the cap.
	bounded, _ := edit.MustGet("changes").(*jsoncompat.Array)
	if len(bounded.Items) != 20 {
		t.Fatalf("changes=%d want 20", len(bounded.Items))
	}
	first, _ := bounded.Items[0].(*jsoncompat.Object)
	if path, _ := first.MustGet("path").(string); path != "/work/f.go" {
		t.Fatalf("path=%q", path)
	}
	if _, exists := first.Get("truncated"); exists {
		t.Fatal("short diff must not be marked truncated")
	}

	if command.MustGet("exitCode") == nil {
		t.Fatal("exitCode dropped")
	}
	if _, exists := command.Get("changes"); exists {
		t.Fatal("command row must not carry changes")
	}
	if _, exists := plain.Get("changes"); exists {
		t.Fatal("plain row must not carry changes")
	}
	if _, exists := plain.Get("exitCode"); exists {
		t.Fatal("plain row must not carry exitCode")
	}

	// Clipping with the truncated marker on a second turn's row.
	updateWith(t, tl, `{
	 "sequence": 2, "cwd": "/work",
	 "turns": [{"id": "t1", "status": "completed", "messages": [
	  {"id": "m1", "kind": "toolCall", "role": "activity", "title": "编辑", "text": "",
	   "changes": [{"path": "/work/big.txt", "oldText": "o", "newText": "`+longText+`"}]},
	  {"id": "m2", "kind": "toolCall", "role": "activity", "title": "执行命令", "text": "go test", "exitCode": 3},
	  {"id": "m3", "kind": "toolCall", "role": "activity", "title": "普通", "text": "无字段"}
	 ]}]
	}`)
	page = tl.Page(20, "")
	edit = rowByTitle(page, "编辑")
	bounded, _ = edit.MustGet("changes").(*jsoncompat.Array)
	if len(bounded.Items) != 1 {
		t.Fatalf("changes=%d want 1", len(bounded.Items))
	}
	entry, _ := bounded.Items[0].(*jsoncompat.Object)
	if diff, _ := entry.MustGet("newText").(string); len([]rune(diff)) != DetailChars {
		t.Fatalf("newText length=%d want %d", len([]rune(diff)), DetailChars)
	}
	if entry.MustGet("truncated") != true {
		t.Fatal("truncated marker missing")
	}

	// Same passthrough on the incremental changes channel after an update.
	updateWith(t, tl, `{
	 "sequence": 3, "cwd": "/work",
	 "turns": [{"id": "t1", "status": "completed", "messages": [
	  {"id": "m1", "kind": "toolCall", "role": "activity", "title": "编辑", "text": "",
	   "changes": [{"path": "/work/late.go", "diff": "-old\n+new"}]},
	  {"id": "m2", "kind": "toolCall", "role": "activity", "title": "执行命令", "text": "go test", "exitCode": 0},
	  {"id": "m3", "kind": "toolCall", "role": "activity", "title": "普通", "text": "无字段"}
	 ]}]
	}`)
	deltas := tl.Changes("2", "epoch1", "")
	late := rowByTitle(deltas, "编辑")
	if late == nil {
		t.Fatal("changes channel missing edited row")
	}
	lateChanges, _ := late.MustGet("changes").(*jsoncompat.Array)
	if len(lateChanges.Items) != 1 {
		t.Fatalf("late changes=%v", lateChanges)
	}
	entry, _ = lateChanges.Items[0].(*jsoncompat.Object)
	if diff, _ := entry.MustGet("diff").(string); diff != "-old\n+new" {
		t.Fatalf("late diff=%q", diff)
	}
}

// No changes and no exit code: nothing new may appear on the encoded row,
// which is what keeps the byte-locked golden scenarios stable.
func TestUpdateWithoutFieldsStaysClean(t *testing.T) {
	tl := New(func() string { return "epoch1" })
	updateWith(t, tl, `{
	 "sequence": 1,
	 "turns": [{"id": "t1", "status": "completed", "messages": [
	  {"id": "m1", "kind": "toolCall", "role": "activity", "title": "普通", "text": "x"}
	 ]}]
	}`)
	row := rowByTitle(tl.Page(20, ""), "普通")
	for _, field := range [...]string{"changes", "exitCode"} {
		if _, exists := row.Get(field); exists {
			t.Fatalf("%s must stay absent", field)
		}
	}
}
