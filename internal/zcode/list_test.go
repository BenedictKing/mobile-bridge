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
