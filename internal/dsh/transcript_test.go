package dsh

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const fixtureSession = "session-fixture"

func encodeLines(t *testing.T, lines []string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := zstd.NewWriter(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if _, err := writer.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeFixture(t *testing.T, root string, frames ...[]string) string {
	t.Helper()
	dir := filepath.Join(root, "sessions", "--work--", fixtureSession)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	for _, frame := range frames {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(encodeLines(t, frame)); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestTranscriptReaderBuildsTurns(t *testing.T) {
	root := t.TempDir()
	hash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	attachment := filepath.Join(root, "attachments", "v1", "objects", hash[:2], hash)
	if err := os.MkdirAll(filepath.Dir(attachment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attachment, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, []string{
		`{"type":"session","version":4,"id":"` + fixtureSession + `","createdAt":1000,"cwd":"/work"}`,
		`{"type":"session/title","seq":1,"time":1000,"data":{"title":"真实标题"}}`,
		`{"type":"turn/start","seq":2,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":3,"time":1100,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第一问"},{"type":"image","attachment":{"attachmentId":"sha256:` + hash + `","name":"shot.png"}}]}}`,
		`{"type":"user/message","seq":4,"time":1100,"data":{"id":"runtime","role":"user","source":{"kind":"runtime-context"},"content":[{"type":"text","text":"Current runtime context"}]}}`,
		`{"type":"assistant/message","seq":5,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"reasoning","text":"想一想"},{"type":"text","text":"答一"},{"type":"tool-call","id":"call-1","name":"bash","arguments":"{\"command\":\"ls\"}"}]}}}`,
		`{"type":"tool/call","seq":6,"time":1200,"data":{"callId":"call-1","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		`{"type":"tool/result","seq":7,"time":1300,"data":{"message":{"toolCallId":"call-1","content":[{"type":"text","text":"file-a\nfile-b"}],"isError":false}}}`,
		`{"type":"todo/write","seq":8,"time":1300,"data":{"todos":[{"content":"做 A","status":"completed"},{"content":"做 B","status":"in_progress"}]}}`,
		`{"type":"turn/end","seq":9,"time":1400,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
		`{"type":"turn/start","seq":10,"time":2000,"data":{"turn":2}}`,
		`{"type":"user/message","seq":11,"time":2100,"data":{"id":"u2","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第二问"}]}}`,
		`{"type":"assistant/message","seq":12,"time":2200,"data":{"turn":2,"step":1,"message":{"role":"assistant","content":[{"type":"reasoning","text":"还在想"}]}}}`,
	})
	reader := TranscriptReader(root)
	snapshot, err := reader(fixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == nil {
		t.Fatal("no snapshot")
	}
	if snapshot.Title != "真实标题" {
		t.Fatalf("title = %q", snapshot.Title)
	}
	if len(snapshot.Turns) != 2 {
		t.Fatalf("turns = %d", len(snapshot.Turns))
	}
	first := snapshot.Turns[0]
	if status, _ := first.MustGet("status").(string); status != "completed" {
		t.Fatalf("first status = %q", status)
	}
	messages := turnMessages(t, first)
	if len(messages) != 4 { // user, thought, answer, tool — runtime context filtered
		t.Fatalf("first messages = %d", len(messages))
	}
	user := messages[0]
	if text, _ := user.MustGet("text").(string); text != "第一问" {
		t.Fatalf("user text = %q", text)
	}
	attachments, _ := user.MustGet("attachments").(*jsoncompat.Array)
	if attachments == nil || len(attachments.Items) != 1 {
		t.Fatalf("attachments = %v", attachments)
	}
	image, _ := attachments.Items[0].(*jsoncompat.Object)
	if path, _ := image.MustGet("path").(string); path != attachment {
		t.Fatalf("attachment path = %q", path)
	}
	tool := messages[3]
	for field, want := range map[string]string{
		"kind": "toolCall", "title": "bash", "text": "ls", "output": "file-a\nfile-b", "status": "completed",
	} {
		if value, _ := tool.MustGet(field).(string); value != want {
			t.Fatalf("tool %s = %q, want %q", field, value, want)
		}
	}
	second := snapshot.Turns[1]
	if status, _ := second.MustGet("status").(string); status != "inProgress" {
		t.Fatalf("second status = %q", status)
	}
	secondMessages := turnMessages(t, second)
	if len(secondMessages) != 3 { // user, thought, plan
		t.Fatalf("second messages = %d", len(secondMessages))
	}
	if kind, _ := secondMessages[2].MustGet("kind").(string); kind != "plan" {
		t.Fatalf("plan kind = %q", kind)
	}

	// Unchanged file: the poller must not rebuild.
	if again, err := reader(fixtureSession); err != nil || again != nil {
		t.Fatalf("unchanged load = %v/%v", again, err)
	}
}

func TestTranscriptReaderPlainJSONLMatchesZstd(t *testing.T) {
	lines := []string{
		`{"type":"session/title","seq":1,"time":1000,"data":{"title":"明文会话"}}`,
		`{"type":"turn/start","seq":2,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":3,"time":1100,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第一问"}]}}`,
		`{"type":"assistant/message","seq":4,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"答一"}]}}}`,
		`{"type":"turn/end","seq":5,"time":1300,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	}
	compressedRoot := t.TempDir()
	writeFixture(t, compressedRoot, lines)
	compressed, err := TranscriptReader(compressedRoot)(fixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	plainRoot := t.TempDir()
	dir := filepath.Join(plainRoot, "sessions", "--work--", fixtureSession)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	plain, err := TranscriptReader(plainRoot)(fixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	if plain == nil || compressed == nil {
		t.Fatalf("missing snapshot: plain=%v zstd=%v", plain != nil, compressed != nil)
	}
	if plain.Title != compressed.Title || len(plain.Turns) != len(compressed.Turns) {
		t.Fatal("plain and zstd snapshot metadata differ")
	}
	for index := range plain.Turns {
		if string(jsoncompat.EncodeCompact(plain.Turns[index])) != string(jsoncompat.EncodeCompact(compressed.Turns[index])) {
			t.Fatalf("plain and zstd turn %d differ", index)
		}
	}
}

func TestTranscriptReaderTailsAppendedFrames(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, []string{
		`{"type":"turn/start","seq":1,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":2,"time":1100,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第一问"}]}}`,
		`{"type":"turn/end","seq":3,"time":1200,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	})
	reader := TranscriptReader(root)
	first, err := reader(fixtureSession)
	if err != nil || first == nil {
		t.Fatalf("first = %v/%v", first, err)
	}
	// A later turn is appended as another zstd frame; the previous snapshot
	// must not be served from the size cache.
	writeFixture(t, root, []string{
		`{"type":"turn/start","seq":4,"time":2000,"data":{"turn":2}}`,
		`{"type":"user/message","seq":5,"time":2100,"data":{"id":"u2","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第二问"}]}}`,
	})
	second, err := reader(fixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || len(second.Turns) != 2 {
		t.Fatalf("second = %v", second)
	}
	if status, _ := second.Turns[1].MustGet("status").(string); status != "inProgress" {
		t.Fatalf("appended turn status = %q", status)
	}
}

func TestTranscriptReaderMissingSession(t *testing.T) {
	reader := TranscriptReader(t.TempDir())
	if snapshot, err := reader("missing"); err != nil || snapshot != nil {
		t.Fatalf("missing = %v/%v", snapshot, err)
	}
}

func turnMessages(t *testing.T, turn *jsoncompat.Object) []*jsoncompat.Object {
	t.Helper()
	list, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if list == nil {
		t.Fatal("no messages")
	}
	messages := []*jsoncompat.Object{}
	for _, item := range list.Items {
		if message, ok := item.(*jsoncompat.Object); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

func TestTranscriptReaderStaleOpenTurn(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, []string{
		`{"type":"turn/start","seq":1,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":2,"time":1100,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第一问"}]}}`,
		`{"type":"assistant/message","seq":3,"time":1200,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"reasoning","text":"还在想"}]}}}`,
	})
	reader := newReader(root)
	first, err := reader.Load(fixtureSession)
	if err != nil || first == nil {
		t.Fatalf("first = %v/%v", first, err)
	}
	if status, _ := first.Turns[0].MustGet("status").(string); status != "inProgress" {
		t.Fatalf("fresh status = %q", status)
	}
	reader.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	stale, err := reader.Load(fixtureSession)
	if err != nil || stale == nil {
		t.Fatalf("stale = %v/%v", stale, err)
	}
	if status, _ := stale.Turns[0].MustGet("status").(string); status != "interrupted" {
		t.Fatalf("stale status = %q", status)
	}
	if again, _ := reader.Load(fixtureSession); again != nil {
		t.Fatal("stale rebuild repeated")
	}
}

func TestTranscriptReaderToleratesTruncatedTail(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "--work--", fixtureSession)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	firstFrame := encodeLines(t, []string{
		`{"type":"turn/start","seq":1,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":2,"time":1100,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第一问"}]}}`,
		`{"type":"turn/end","seq":3,"time":1200,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	})
	secondFrame := encodeLines(t, []string{
		`{"type":"turn/start","seq":4,"time":2000,"data":{"turn":2}}`,
		`{"type":"user/message","seq":5,"time":2100,"data":{"id":"u2","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"第二问"}]}}`,
	})
	// Simulate a read while the app is appending: the newest frame is cut.
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	if err := os.WriteFile(path, append(firstFrame, secondFrame[:len(secondFrame)-3]...), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := newReader(root).Load(fixtureSession)
	if err != nil {
		t.Fatalf("truncated tail must keep the decoded prefix: %v", err)
	}
	if snapshot == nil || len(snapshot.Turns) == 0 {
		t.Fatalf("snapshot = %v", snapshot)
	}
	if text, _ := turnMessages(t, snapshot.Turns[0])[0].MustGet("text").(string); text != "第一问" {
		t.Fatalf("first user text = %q", text)
	}
}

func TestTranscriptReaderIgnoresTraversalAttachment(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, []string{
		`{"type":"turn/start","seq":1,"time":1000,"data":{"turn":1}}`,
		`{"type":"user/message","seq":2,"time":1100,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"看图"},{"type":"image","attachment":{"attachmentId":"sha256:../../../../etc/passwd","name":"evil.png"}}]}}`,
		`{"type":"turn/end","seq":3,"time":1200,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	})
	snapshot, err := newReader(root).Load(fixtureSession)
	if err != nil || snapshot == nil {
		t.Fatalf("load = %v/%v", snapshot, err)
	}
	user := turnMessages(t, snapshot.Turns[0])[0]
	if attachments, _ := user.MustGet("attachments").(*jsoncompat.Array); attachments != nil && len(attachments.Items) > 0 {
		t.Fatalf("traversal attachment was mapped: %v", attachments)
	}
}
