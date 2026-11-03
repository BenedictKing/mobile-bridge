package dsh

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestListSessionsReadsNativeHeaderAndTitle(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, []string{
		`{"type":"session","version":4,"id":"` + fixtureSession + `","createdAt":1000,"cwd":"/work"}`,
		`{"type":"turn/start","seq":1,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":2,"time":1000,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第一问"}]}}`,
		`{"type":"turn/end","seq":3,"time":1000,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	})
	writeFile(t, filepath.Join(root, "storages", "session_projcache", "sessions", fixtureSession+".json"),
		`{"record":{"identity":{"createdAt":1000,"cwd":"/work"},"rows":{"title":{"val":"原生标题"}}}}`)

	rows := ListSessions(root)
	if len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	row := rows[0]
	if row.MustGet("sessionId") != fixtureSession || row.MustGet("title") != "原生标题" || row.MustGet("cwd") != "/work" {
		t.Fatalf("row=%s", jsoncompat.EncodeCompact(row))
	}
	updated, _ := row.MustGet("updatedAt").(json.Number)
	if value, _ := updated.Int64(); value <= 0 {
		t.Fatalf("updatedAt=%d", value)
	}
}
