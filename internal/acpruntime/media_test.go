package acpruntime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestPrepareMediaCoversUserReplayAndNestedImageSources(t *testing.T) {
	session := &Session{ID: "user-replay", mediaDir: t.TempDir()}
	png := []byte("\x89PNG\r\n\x1a\nreplayed-user-image")
	encoded := base64.StdEncoding.EncodeToString(png)
	for _, content := range []string{
		`{"type":"image","data":"` + encoded + `","mimeType":"image/png"}`,
		`{"content":[{"type":"text","text":"[Image #2]"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}}]}`,
		`{"type":"image","data":"data:image/png;base64,` + encoded + `"}`,
	} {
		update := mediaUpdate(t, `{"sessionUpdate":"user_message_chunk","content":`+content+`}`)
		session.prepareMedia(update)
		if raw := string(jsoncompat.EncodeCompact(update)); strings.Contains(raw, encoded) {
			t.Fatal("处理后的更新仍含图片编码")
		}
		view := acpview.NewView("user-replay", nil, "/work", func() float64 { return 0 })
		view.OnUpdate(update)
		turns := view.Snapshot().MustGet("turns").(*jsoncompat.Array)
		message := turns.Items[0].(*jsoncompat.Object).MustGet("messages").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
		attachment := message.MustGet("attachments").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
		path, _ := attachment.MustGet("path").(string)
		if raw, err := os.ReadFile(path); err != nil || string(raw) != string(png) {
			t.Fatalf("图片附件未正确落盘: %v", err)
		}
	}
}

func mediaUpdate(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}

func TestPrepareMediaPersistsImageBlocks(t *testing.T) {
	dir := t.TempDir()
	session := &Session{ID: "sess-visual", mediaDir: dir}

	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("tiny-image")...)
	encoded := base64.StdEncoding.EncodeToString(png)
	session.prepareMedia(mediaUpdate(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":`+
			`{"type":"image","data":"`+encoded+`","mimeType":"image/png"}}`))

	sum := sha256.Sum256(png)
	path := filepath.Join(dir, "sess-visual", hex.EncodeToString(sum[:12])+".png")
	if raw, err := os.ReadFile(path); err != nil || string(raw) != string(png) {
		t.Fatalf("media not persisted at %s: %v", path, err)
	}

	// The same block streamed again reuses the file instead of rewriting it.
	session.prepareMedia(mediaUpdate(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":`+
			`{"type":"image","data":"`+encoded+`","mimeType":"image/png"}}`))
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(png)) {
		t.Fatalf("dedup broke the file: %v", err)
	}
}

func TestPrepareMediaRewritesAndStripsData(t *testing.T) {
	dir := t.TempDir()
	session := &Session{ID: "sess-visual", mediaDir: dir}
	png := []byte("\x89PNG\r\n\x1a\nsecond")
	block := jsoncompat.NewObject()
	block.Set("type", "image")
	block.Set("data", base64.StdEncoding.EncodeToString(png))
	block.Set("mimeType", "image/png")
	content := &jsoncompat.Array{Items: []any{block}}
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call")
	update.Set("content", content)

	session.prepareMedia(update)

	if _, has := block.Get("data"); has {
		t.Fatal("base64 data must never reach the view")
	}
	path, _ := block.MustGet("path").(string)
	if path == "" || !filepath.IsAbs(path) {
		t.Fatalf("block not rewritten to a path: %v", block)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != string(png) {
		t.Fatalf("persisted bytes wrong: %v", err)
	}
	// The rewritten block feeds straight into acpview's attachment extraction.
	if block.MustGet("type") != "image" {
		t.Fatalf("type lost: %v", block)
	}
}

func TestPrepareMediaRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	session := &Session{ID: "sess-visual", mediaDir: dir}

	// Invalid base64: data is dropped, no file is written.
	update := mediaUpdate(t,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"!!!not-base64!!!"}}`)
	session.prepareMedia(update)
	block, _ := update.MustGet("content").(*jsoncompat.Object)
	if _, has := block.Get("data"); has {
		t.Fatal("invalid data survived")
	}
	if path, _ := block.MustGet("path").(string); path != "" {
		t.Fatalf("invalid data got a path: %q", path)
	}

	// Text blocks pass through untouched.
	update = mediaUpdate(t,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"普通文本"}}`)
	before := string(jsoncompat.EncodeCompact(update))
	session.prepareMedia(update)
	if after := string(jsoncompat.EncodeCompact(update)); after != before {
		t.Fatalf("text update mutated: %s", after)
	}

	// A session id that cannot become a safe directory never writes.
	weird := &Session{ID: "../escape", mediaDir: dir}
	encoded := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nx"))
	weird.prepareMedia(mediaUpdate(t,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"`+encoded+`"}}`))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("unsafe session id wrote files: %v", entries)
	}
}
