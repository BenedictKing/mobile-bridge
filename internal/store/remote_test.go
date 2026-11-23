package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	_ "modernc.org/sqlite"
)

const remoteRollout = `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"远端问题"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"远端回答"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}
`

// remoteHome builds a Codex data directory the helper can read: one desktop
// thread, one subagent thread, and one thread whose rollout sits outside.
func remoteHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	rollout := filepath.Join(home, "sessions", "2026", "11", "23", "rollout-remote.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, []byte(remoteRollout), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "escaped.jsonl")
	if err := os.WriteFile(outside, []byte(remoteRollout), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, name TEXT, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES
		('remote-1',NULL,'远端会话','/srv/app',30,0,'Codex Desktop','vscode',?),
		('remote-2','改名',NULL,'/srv/app/api',20,0,'codex-tui','cli',?),
		('child-1',NULL,'子任务','/srv/app',40,0,'Codex Desktop','{"subagent":{}}',?)`,
		rollout, outside, rollout); err != nil {
		t.Fatal(err)
	}
	return home
}

// localPython runs the helper with this machine's python3 in place of ssh, so
// the shipped script itself is exercised end to end.
func localPython(t *testing.T, home string) RemoteRunner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper end-to-end test needs a POSIX python3")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	return func(ctx context.Context, alias, script string) ([]byte, error) {
		command := exec.CommandContext(ctx, python, "-I", "-")
		command.Env = append(os.Environ(), "CODEX_HOME="+home)
		command.Stdin = strings.NewReader(script)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, errors.New(err.Error() + ": " + stderr.String())
		}
		return output, nil
	}
}

func TestRemoteHelperListMatchesLocalStore(t *testing.T) {
	home := remoteHome(t)
	remote := NewRemoteStore("devbox", localPython(t, home))
	for _, query := range []string{"", "改名", "/srv/app"} {
		local, err := NewSessionStore(home).List(query, 100, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := remote.List(query, 100, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		want, got := encodeRows(local), encodeRows(rows)
		if want != got {
			t.Fatalf("query %q:\nlocal  %s\nremote %s", query, want, got)
		}
	}
	rows, _ := remote.List("", 100, 0, false)
	if len(rows) != 2 || rows[0].MustGet("id") != "remote-1" {
		t.Fatalf("subagent rows must stay hidden: %s", encodeRows(rows))
	}
}

func TestRemoteHelperHistoryMatchesLocalStore(t *testing.T) {
	home := remoteHome(t)
	remote := NewRemoteStore("devbox", localPython(t, home))
	local, err := NewSessionStore(home).HistoryFor("remote-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	history, err := remote.HistoryFor("remote-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want, got := string(jsoncompat.EncodeCompact(local)), string(jsoncompat.EncodeCompact(history)); want != got {
		t.Fatalf("history differs:\nlocal  %s\nremote %s", want, got)
	}
	if !strings.Contains(string(jsoncompat.EncodeCompact(history)), "远端回答") {
		t.Fatalf("history lost the answer: %s", jsoncompat.EncodeCompact(history))
	}
	cases := map[string]string{
		"remote-2": "会话记录路径不在 Codex 数据目录中",
		"missing":  "找不到这个会话",
	}
	for id, message := range cases {
		if _, err := remote.HistoryFor(id, nil); err == nil || err.Error() != message {
			t.Fatalf("HistoryFor(%s) err=%v, want %q", id, err, message)
		}
	}
	if _, err := remote.HistoryFor("child-1", nil); err == nil {
		t.Fatal("subagent history must not be readable as a user session")
	}
}

func TestRemoteHelperRefusesWritesAndReportsTruncation(t *testing.T) {
	home := remoteHome(t)
	run := localPython(t, home)
	remote := NewRemoteStore("devbox", run)
	request := jsoncompat.NewObject()
	request.Set("op", "query")
	request.Set("sql", "DELETE FROM threads")
	if _, err := remote.call(context.Background(), request); err == nil || err.Error() != "远端只允许只读查询" {
		t.Fatalf("write query err=%v", err)
	}
	request.Set("sql", "SELECT 1; DELETE FROM threads")
	if _, err := remote.call(context.Background(), request); err == nil {
		t.Fatal("stacked statement must be refused")
	}
	if rows, err := NewSessionStore(home).List("", 100, 0, false); err != nil || len(rows) != 2 {
		t.Fatalf("remote writes reached the database: rows=%d err=%v", len(rows), err)
	}

	// A rollout past the helper cap arrives without its oldest bytes, and the
	// history says older turns are not loaded.
	rollout := filepath.Join(home, "sessions", "2026", "11", "23", "rollout-remote.jsonl")
	filler := strings.Repeat(`{"type":"event_msg","payload":{"type":"token_count"}}`+"\n", 9<<20/52)
	if err := os.WriteFile(rollout, []byte(filler+remoteRollout), 0o600); err != nil {
		t.Fatal(err)
	}
	history, err := NewRemoteStore("devbox", run).HistoryFor("remote-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	pagination, _ := history.MustGet("turnsPagination").(*jsoncompat.Object)
	if pagination == nil || pagination.MustGet("hasLoadedOldest") != false {
		t.Fatalf("truncated history must report older turns missing: %v", pagination)
	}
	if !strings.Contains(string(jsoncompat.EncodeCompact(history)), "远端回答") {
		t.Fatal("truncated history lost the newest turn")
	}
}

var requestLiteral = regexp.MustCompile(`b64decode\("([A-Za-z0-9+/=]+)"\)`)

func decodeRemoteRequest(t *testing.T, script string) *jsoncompat.Object {
	t.Helper()
	match := requestLiteral.FindStringSubmatch(script)
	if match == nil {
		t.Fatalf("script carries no request")
	}
	raw, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}

func TestRemoteStoreCachesListsAndHandsOutCopies(t *testing.T) {
	calls := map[string]int{}
	remote := NewRemoteStore("devbox", func(ctx context.Context, alias, script string) ([]byte, error) {
		op, _ := decodeRemoteRequest(t, script).MustGet("op").(string)
		calls[op]++
		if op == "schema" {
			return []byte(`{"columns":["id","title","cwd","updated_at","originator","source"]}`), nil
		}
		return []byte("Welcome banner\n" + `{"rows":[["r1","远端","/srv",30,"Codex Desktop","vscode"]]}`), nil
	})
	first, err := remote.List("", 100, 0, false)
	if err != nil || len(first) != 1 {
		t.Fatalf("rows=%v err=%v", first, err)
	}
	first[0].Set("host", "mutated")
	second, err := remote.List("", 100, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, mutated := second[0].Get("host"); mutated {
		t.Fatal("cached rows must not share objects with earlier callers")
	}
	if calls["schema"] != 1 || calls["query"] != 1 {
		t.Fatalf("second list should be served from cache: %v", calls)
	}
}

func TestRemoteStoreRejectsBadAliasAndUnreadableReplies(t *testing.T) {
	ran := false
	bad := NewRemoteStore("-oProxyCommand=touch /tmp/x", func(context.Context, string, string) ([]byte, error) {
		ran = true
		return nil, nil
	})
	if _, err := bad.List("", 10, 0, false); err == nil || ran {
		t.Fatalf("invalid alias must be refused before ssh runs: err=%v ran=%v", err, ran)
	}
	garbage := NewRemoteStore("devbox", func(context.Context, string, string) ([]byte, error) {
		return []byte("Traceback (most recent call last)"), nil
	})
	if _, err := garbage.List("", 10, 0, false); !errors.Is(err, ErrRemoteUnavailable) {
		t.Fatalf("garbage reply err=%v", err)
	}
	failing := NewRemoteStore("devbox", func(context.Context, string, string) ([]byte, error) {
		return nil, ErrRemotePython
	})
	if _, err := failing.HistoryFor("r1", nil); !errors.Is(err, ErrRemotePython) {
		t.Fatalf("runner error must pass through: %v", err)
	}
}

func TestSSHRunnerUsesLockedDownInvocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh is a shell script")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	stdin := filepath.Join(dir, "stdin")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + record + "\"\n/bin/cat > \"" + stdin + "\"\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	output, err := SSHRunner(context.Background(), "devbox", "print(1)\n")
	if err != nil || strings.TrimSpace(string(output)) != `{"ok":true}` {
		t.Fatalf("output=%q err=%v", output, err)
	}
	args, _ := os.ReadFile(record)
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "ClearAllForwardings=yes",
		"UpdateHostKeys=no", "devbox", "python3 -"} {
		if !strings.Contains(string(args), want+"\n") {
			t.Fatalf("ssh args missing %q:\n%s", want, args)
		}
	}
	if sent, _ := os.ReadFile(stdin); string(sent) != "print(1)\n" {
		t.Fatalf("script must travel over stdin, got %q", sent)
	}
}

func encodeRows(rows []*jsoncompat.Object) string {
	array := &jsoncompat.Array{}
	for _, row := range rows {
		array.Items = append(array.Items, row)
	}
	return string(jsoncompat.EncodeCompact(array))
}
