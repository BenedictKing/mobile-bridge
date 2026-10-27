package jev

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// journalSettings returns auto-approve-enabled settings whose jev.json (and
// therefore the journal) lives in dir.
func journalSettings(t *testing.T, dir, serverURL string) *Settings {
	t.Helper()
	settings := New(filepath.Join(dir, "jev.json"))
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", serverURL)
	value.Set("autoApprove", true)
	if _, err := settings.Update(value); err != nil {
		t.Fatal(err)
	}
	return settings
}

func journalLines(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "jev-decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func parseJournalLine(t *testing.T, line string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(line))
	if err != nil {
		t.Fatalf("journal line is not JSON: %v (%s)", err, line)
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		t.Fatalf("journal line is %T, want object (%s)", value, line)
	}
	return object
}

func TestRecordDecisionWritesFullLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work",
			fullProbabilities(map[string]string{"routine_work": "0.9"}, ""),
			"accept_once", 0.95, 0.96)))
	}))
	defer server.Close()
	dir := t.TempDir()
	settings := journalSettings(t, dir, server.URL)
	// Title-less tool call: the subject falls through to the 200-rune
	// command, so the journal exercises the 60-rune truncation.
	params := jsoncompat.NewObject()
	toolCall := jsoncompat.NewObject()
	rawInput := jsoncompat.NewObject()
	rawInput.Set("command", strings.Repeat("a", 200))
	toolCall.Set("rawInput", rawInput)
	params.Set("toolCall", toolCall)
	options := &jsoncompat.Array{}
	allow := jsoncompat.NewObject()
	allow.Set("optionId", "opt-1")
	allow.Set("kind", "allow_once")
	options.Items = append(options.Items, allow)
	params.Set("options", options)

	if decision := settings.EvaluateSession("kimi", "sess-1", "req-1", "session/request_permission", params); decision.Reason != "auto_approved" {
		t.Fatalf("first decision: %+v", decision)
	}
	if decision := settings.EvaluateSession("kimi", "sess-1", "req-2", "session/request_permission", params); decision.Reason != "cache_hit" {
		t.Fatalf("second decision: %+v", decision)
	}
	lines := journalLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d, want 2: %v", len(lines), lines)
	}
	wantKeys := []string{"time", "agent", "session", "request", "method", "reason",
		"categories", "negative_sum", "threshold", "reversible", "confidence", "recommend", "subject"}
	wantReason := map[int]string{0: "auto_approved", 1: "cache_hit"}
	for index, text := range lines {
		line := parseJournalLine(t, text)
		if keys := line.Keys(); len(keys) != len(wantKeys) {
			t.Fatalf("line %d keys = %v", index, keys)
		} else {
			for i, key := range keys {
				if key != wantKeys[i] {
					t.Fatalf("line %d keys = %v, want %v", index, keys, wantKeys)
				}
			}
		}
		if _, err := time.Parse(time.RFC3339, jsoncompat.PyStr(line.MustGet("time"))); err != nil {
			t.Fatalf("line %d time: %v", index, err)
		}
		if line.MustGet("agent") != "kimi" || line.MustGet("session") != "sess-1" ||
			line.MustGet("request") != fmt.Sprintf("req-%d", index+1) ||
			line.MustGet("method") != "session/request_permission" {
			t.Fatalf("line %d identity fields: %s", index, jsoncompat.EncodeCompact(line))
		}
		if line.MustGet("reason") != wantReason[index] {
			t.Fatalf("line %d reason = %v", index, line.MustGet("reason"))
		}
		categories, ok := line.MustGet("categories").(*jsoncompat.Object)
		if !ok || categories.Len() != len(defaultPolicy) {
			t.Fatalf("line %d categories = %v", index, line.MustGet("categories"))
		}
		for _, name := range categories.Keys() {
			if _, isNumber := categories.MustGet(name).(json.Number); !isNumber {
				t.Fatalf("line %d category %s is not a number", index, name)
			}
		}
		if number(categories.MustGet("routine_work"), 0) != 0.9 {
			t.Fatalf("line %d routine_work = %v", index, categories.MustGet("routine_work"))
		}
		if number(line.MustGet("negative_sum"), 0) != 0 ||
			number(line.MustGet("threshold"), 0) != defaultThreshold ||
			number(line.MustGet("reversible"), 0) != 0.95 ||
			number(line.MustGet("confidence"), 0) != 0.96 {
			t.Fatalf("line %d numeric fields: %s", index, jsoncompat.EncodeCompact(line))
		}
		if line.MustGet("recommend") != "accept_once" {
			t.Fatalf("line %d recommend = %v", index, line.MustGet("recommend"))
		}
		subject, _ := line.MustGet("subject").(string)
		runes := []rune(subject)
		if len(runes) != 61 || !strings.HasSuffix(subject, "…") || strings.TrimRight(string(runes[:60]), "a") != "" {
			t.Fatalf("line %d subject not the 60-rune prefix: %q", index, subject)
		}
	}
}

func TestJournalSkipsPreEvaluationReasons(t *testing.T) {
	dir := t.TempDir()
	settings := New(filepath.Join(dir, "jev.json"))
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", "http://127.0.0.1:1")
	if _, err := settings.Update(value); err != nil {
		t.Fatal(err)
	}
	if reason := settings.EvaluateSession("kimi", "s", "r", "session/request_permission", jsoncompat.NewObject()).Reason; reason != "auto_approve_off" {
		t.Fatalf("reason=%s", reason)
	}
	if _, err := os.Stat(filepath.Join(dir, "jev-decisions.jsonl")); err == nil {
		t.Fatal("auto-approve-off decision must not create the journal")
	}

	keyless := t.TempDir()
	enabled := jsoncompat.NewObject()
	enabled.Set("baseUrl", "http://127.0.0.1:1")
	enabled.Set("autoApprove", true)
	settings = New(filepath.Join(keyless, "jev.json"))
	if _, err := settings.Update(enabled); err != nil {
		t.Fatal(err)
	}
	if reason := settings.EvaluateSession("kimi", "s", "r", "session/request_permission", jsoncompat.NewObject()).Reason; reason != "no_api_key" {
		t.Fatalf("reason=%s", reason)
	}
	if _, err := os.Stat(filepath.Join(keyless, "jev-decisions.jsonl")); err == nil {
		t.Fatal("key-less decision must not create the journal")
	}
}

func TestJournalFileMode0600(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work", fullProbabilities(nil, ""), "accept_once", 0.99, 0.99)))
	}))
	defer server.Close()
	dir := t.TempDir()
	settings := journalSettings(t, dir, server.URL)
	settings.EvaluateSession("kimi", "s", "r", "session/request_permission",
		permissionCard("Bash", "npm test", "allow_once", "opt-1"))
	info, err := os.Stat(filepath.Join(dir, "jev-decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("journal mode = %o, want 600", perm)
	}
}

func TestJournalRotatesAt20MB(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work", fullProbabilities(nil, ""), "accept_once", 0.99, 0.99)))
	}))
	defer server.Close()
	dir := t.TempDir()
	journal := filepath.Join(dir, "jev-decisions.jsonl")
	if err := os.WriteFile(journal, bytes.Repeat([]byte("A"), journalMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := journalSettings(t, dir, server.URL)
	params := permissionCard("Bash", "npm test", "allow_once", "opt-1")

	settings.EvaluateSession("kimi", "s1", "req-1", "session/request_permission", params)
	info, err := os.Stat(journal)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= journalMaxBytes {
		t.Fatalf("journal not rotated: %d bytes", info.Size())
	}
	rotated, err := os.Stat(journal + ".1")
	if err != nil {
		t.Fatal("rotated side file missing")
	}
	if rotated.Size() != journalMaxBytes+1 {
		t.Fatalf("rotated size = %d, want %d", rotated.Size(), journalMaxBytes+1)
	}

	// Grow the fresh journal past the limit again and rotate once more: the
	// old main becomes .1 (overwriting the previous side file), and no .2
	// ever appears.
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(bytes.Repeat([]byte("B"), journalMaxBytes+1)); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	settings.EvaluateSession("kimi", "s1", "req-2", "session/request_permission", params)

	side, err := os.Open(journal + ".1")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 2048)
	read, _ := side.Read(head)
	_ = side.Close()
	if read == 0 || head[0] != '{' || !strings.Contains(string(head[:read]), "req-1") {
		t.Fatal("second rotation did not replace .1 with the previous main file")
	}
	matches, _ := filepath.Glob(journal + ".*")
	if len(matches) != 1 {
		t.Fatalf("side files = %v, want exactly one .1", matches)
	}
	main, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "req-2") || strings.Contains(string(main), "req-1") {
		t.Fatalf("main file after rotation = %q", string(main))
	}
}

func TestJournalFailureNeverBlocksApproval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work", fullProbabilities(nil, ""), "accept_once", 0.99, 0.99)))
	}))
	defer server.Close()
	dir := t.TempDir()
	// A directory where the journal file should be makes every open fail.
	if err := os.Mkdir(filepath.Join(dir, "jev-decisions.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := journalSettings(t, dir, server.URL)
	decision := settings.EvaluateSession("kimi", "s1", "req-1", "session/request_permission",
		permissionCard("Bash", "npm test", "allow_once", "opt-1"))
	if !decision.Approved || decision.Response == nil || decision.Reason != "auto_approved" {
		t.Fatalf("journal failure must not block approval: %+v", decision)
	}
}

func TestJournalEncodesFloatsWithoutPanic(t *testing.T) {
	dir := t.TempDir()
	settings := New(filepath.Join(dir, "jev.json"))
	decision := Decision{
		Approved:    true,
		Assessed:    true,
		Reason:      "auto_approved",
		Categories:  map[string]float64{"routine_work": 0.95, "secret_exposure": 0.02},
		NegativeSum: 0.02,
		Threshold:   0.3,
		Reversible:  0.95,
		Confidence:  0.96,
		Recommend:   "accept_once",
		TopCategory: "routine_work",
	}
	settings.recordDecision("kimi", "s1", "req-1", "session/request_permission", decision, nil)
	lines := journalLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("journal lines = %d, want 1", len(lines))
	}
	line := parseJournalLine(t, lines[0])
	if number(line.MustGet("negative_sum"), 0) != 0.02 ||
		number(line.MustGet("threshold"), 0) != 0.3 ||
		number(line.MustGet("reversible"), 0) != 0.95 {
		t.Fatalf("float fields lost precision: %s", lines[0])
	}
	if len(jsoncompat.EncodeCompact(line)) == 0 {
		t.Fatal("journal line failed to re-encode")
	}
}
