package zcode

import (
	"encoding/json"
	"testing"
)

func TestSourceListSessionsUsesPersistedDirectory(t *testing.T) {
	db, path := openFixture(t)
	seed(t, db)
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	rows := source.List()
	if len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	row := rows[0]
	if row.MustGet("sessionId") != testSession || row.MustGet("title") != "真实标题" || row.MustGet("cwd") != "/work" {
		t.Fatalf("unexpected row: %v", row)
	}
	updated, _ := row.MustGet("updatedAt").(json.Number)
	if value, _ := updated.Int64(); value <= 0 {
		t.Fatalf("updatedAt=%d", value)
	}
}

func TestSourceListSessionsExcludesSubagentsButKeepsIndependentTasks(t *testing.T) {
	db, path := openFixture(t)
	for _, row := range []struct {
		id       string
		parentID any
		taskType string
		updated  int
	}{
		{"sess-subagent", testSession, "subagent_child", 5},
		{"sess-side-chat", testSession, "selection_side_chat", 4},
		{"sess-workflow-child", testSession, "workflow_child", 3},
		{"sess-fork", testSession, "fork", 2},
		{"sess-workflow-parent", nil, "workflow_parent", 1},
	} {
		insertRow(t, db, `INSERT INTO session (id, title, directory, time_created, time_updated, parent_id, task_type)
			VALUES (?, ?, '/work', 1, ?, ?, ?)`, row.id, row.id, row.updated, row.parentID, row.taskType)
	}
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	rows := source.List()
	if len(rows) != 2 {
		t.Fatalf("rows=%v", rows)
	}
	for index, want := range []string{"sess-fork", "sess-workflow-parent"} {
		if id, _ := rows[index].MustGet("sessionId").(string); id != want {
			t.Fatalf("rows[%d]=%q, want %q", index, id, want)
		}
	}
}
