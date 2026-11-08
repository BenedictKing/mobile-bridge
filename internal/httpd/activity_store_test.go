package httpd

import (
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func activityRow(id, turnID, status string) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("id", id)
	row.Set("host", "local")
	row.Set("connected", true)
	row.Set("turnId", turnID)
	row.Set("turnStatus", status)
	return row
}

func activityRows(rows ...*jsoncompat.Object) *jsoncompat.Array {
	result := &jsoncompat.Array{}
	for _, row := range rows {
		result.Items = append(result.Items, row)
	}
	return result
}

func TestActivityStorePersistsTerminalUnreadState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-activity.json")
	store, err := LoadActivity(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Activity: store}
	row := activityRow("thread-1", "turn-1", "completed")
	if err := server.observeActivity(activityRows(row), ""); err != nil {
		t.Fatal(err)
	}
	if row.MustGet("pending") != true {
		t.Fatalf("new terminal turn should be unread: %s", jsoncompat.EncodeCompact(row))
	}

	reloaded, err := LoadActivity(path)
	if err != nil {
		t.Fatal(err)
	}
	server.Activity = reloaded
	offline := jsoncompat.NewObject()
	offline.Set("id", "thread-1")
	offline.Set("host", "local")
	offline.Set("connected", false)
	if err := server.observeActivity(activityRows(offline), ""); err != nil {
		t.Fatal(err)
	}
	if offline.MustGet("pending") != true || offline.MustGet("turnStatus") != "completed" {
		t.Fatalf("offline row lost unread state: %s", jsoncompat.EncodeCompact(offline))
	}
	if err := server.markActivityRead("local", "thread-1"); err != nil {
		t.Fatal(err)
	}

	reloaded, err = LoadActivity(path)
	if err != nil {
		t.Fatal(err)
	}
	server.Activity = reloaded
	sameTurn := activityRow("thread-1", "turn-1", "completed")
	if err := server.observeActivity(activityRows(sameTurn), ""); err != nil {
		t.Fatal(err)
	}
	if sameTurn.MustGet("pending") != false {
		t.Fatalf("read turn became unread again: %s", jsoncompat.EncodeCompact(sameTurn))
	}

	newTurn := activityRow("thread-1", "turn-2", "failed")
	if err := server.observeActivity(activityRows(newTurn), "local|thread-1"); err != nil {
		t.Fatal(err)
	}
	if newTurn.MustGet("pending") != false {
		t.Fatalf("visible terminal turn should be read: %s", jsoncompat.EncodeCompact(newTurn))
	}
}
