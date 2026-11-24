package httpd

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestMigrationPendingMessagesAcrossScopes(t *testing.T) {
	for _, scope := range []string{"full", "main", "latest"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%v", scope, empty), func(t *testing.T) {
				queue := mustLoadSubmissions(t, filepath.Join(t.TempDir(), "submissions.json"))
				rows := []*submissionEntry{
					{ID: "failed", ThreadID: "source", Status: SubFailed, Text: "FAILED_TASK", Attachments: []string{"private-upload-id"}, AttachmentNames: []string{"diagram.png"}},
					{ID: "second", ThreadID: "source", Status: SubQueued, Text: "SECOND_TASK", Attachments: []string{"missing-name-id"}, AttachmentNames: []string{"missing-name-id"}},
					{ID: "accepted", ThreadID: "source", Status: SubAccepted, Text: "ACCEPTED_TASK"},
					{ID: "cancelled", ThreadID: "source", Status: SubCancelled, Text: "CANCELLED_TASK"},
					{ID: "ignored", ThreadID: "source", Status: SubIgnored, Text: "IGNORED_TASK"},
					{ID: "internal", ThreadID: "source", Status: SubUnknown, InternalTask: true, Text: "INTERNAL_TASK"},
					{ID: "foreign", ThreadID: "other", Status: SubQueued, Text: "FOREIGN_TASK"},
				}
				for _, row := range rows {
					key := submissionKey(row.ThreadID, row.ID)
					queue.entries[key] = row
					queue.order = append(queue.order, key)
				}
				if err := queue.saveLocked(); err != nil {
					t.Fatal(err)
				}
				before := queue.threadEntries("source")
				view := transcriptView()
				if empty {
					view.Set("turns", &jsoncompat.Array{})
				}
				server := &Server{Queue: queue}
				pendingView, err := server.migrationPendingView("source", view, "en-US")
				if err != nil {
					t.Fatal(err)
				}
				text := migrationMessage("Codex", pendingView, "en-US", scope, "CONTINUE_TASK")
				for _, needle := range []string{"FAILED_TASK", "SECOND_TASK", "diagram.png", "Attachments not migrated", "not yet sent"} {
					if strings.Count(text, needle) == 0 {
						t.Fatalf("transcript lost %s: %s", needle, text)
					}
				}
				if strings.Index(text, "FAILED_TASK") > strings.Index(text, "SECOND_TASK") || !strings.HasSuffix(text, "CONTINUE_TASK") {
					t.Fatalf("lost queue or instruction order: %s", text)
				}
				for _, excluded := range []string{"ACCEPTED_TASK", "CANCELLED_TASK", "IGNORED_TASK", "INTERNAL_TASK", "FOREIGN_TASK", "private-upload-id", "missing-name-id"} {
					if strings.Contains(text, excluded) {
						t.Fatalf("copied excluded data %s", excluded)
					}
				}
				if _, exists := view.Get("migrationPendingMessages"); exists || !reflect.DeepEqual(before, queue.threadEntries("source")) {
					t.Fatal("migration changed the source view or queue")
				}
				reloaded := mustLoadSubmissions(t, queue.path)
				if !reflect.DeepEqual(before, reloaded.threadEntries("source")) {
					t.Fatal("migration changed the durable queue")
				}
			})
		}
	}
}

func pendingMigrationHarness(t *testing.T) (*concurrencyHarness, *acpruntime.Manager, string, string) {
	t.Helper()
	env := newConcurrencyHarness(t)
	t.Cleanup(env.bridge.Close)
	cwd := t.TempDir()
	state := jsoncompat.NewObject()
	state.Set("id", env.threadID)
	state.Set("title", "新聊天")
	state.Set("cwd", cwd)
	state.Set("turns", &jsoncompat.Array{})
	env.bridge.Publish(env.threadID, state)
	argv, creates, prompts := migrationFakeAgent(t, `[]`, "zcode")
	manager := acpruntime.NewCommand(argv)
	manager.Label = "ZCode"
	t.Cleanup(manager.Close)
	env.server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	env.server.Creations = mustLoadCreations(t, filepath.Join(t.TempDir(), "creations.json"))
	env.server.Queue = mustLoadSubmissions(t, filepath.Join(t.TempDir(), "submissions.json"))
	return env, manager, creates, prompts
}

func TestMigrationCopiesFailedCodexMessageAndReplaysWithoutResending(t *testing.T) {
	env, target, creates, prompts := pendingMigrationHarness(t)
	id := "bff465c4-5377-402c-91d6-34fa2f3f4579"
	queue := env.server.Queue
	if _, _, err := queue.enqueue(env.threadID, id, "修复历史跳动和已完成子代理的位置", "queue", "default", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := queue.setStatus(env.threadID, id, SubFailed); err != nil {
		t.Fatal(err)
	}
	body := `{"agent":"zcode","scope":"main","id":"44535ceb-a209-42bc-977b-51a58ab8225a"}`
	path := "/api/sessions/" + env.threadID + "/migrate"
	first := env.request(http.MethodPost, path, body)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"sent": true`) {
		t.Fatalf("migration failed: %d %s", first.Code, first.Body.String())
	}
	entry := env.server.Creations.lookup("44535ceb-a209-42bc-977b-51a58ab8225a")
	if !strings.Contains(entry.Message, "修复历史跳动和已完成子代理的位置") || !strings.Contains(entry.Message, "尚未发送") {
		t.Fatalf("empty or misleading transcript: %s", entry.Message)
	}
	snapshot := env.request(http.MethodGet, "/api/sessions/"+entry.ID, "").Body.String()
	if !strings.Contains(snapshot, "修复历史跳动和已完成子代理的位置") {
		t.Fatalf("target did not receive the failed message: %s", snapshot)
	}
	// A later uncertain source send must not invalidate a successful replay.
	if err := queue.setStatus(env.threadID, id, SubUnknown); err != nil {
		t.Fatal(err)
	}
	retry := env.request(http.MethodPost, path, body)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"sent": true`) {
		t.Fatalf("successful replay rejected: %d %s", retry.Code, retry.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for target.Get(entry.ID).Snapshot().MustGet("status") == "active" && time.Now().Before(deadline) {
		target.Get(entry.ID).Wait(10 * time.Millisecond)
	}
	if counterValue(t, creates) != 1 || counterValue(t, prompts) != 1 || env.server.Creations.lookup("44535ceb-a209-42bc-977b-51a58ab8225a").Message != entry.Message {
		t.Fatal("successful replay recreated, resent or rewrote the transcript")
	}
}

func TestMigrationUnknownSourceBlocksBeforeTargetCreation(t *testing.T) {
	env, _, creates, prompts := pendingMigrationHarness(t)
	queue := env.server.Queue
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := queue.enqueue(env.threadID, id, "可能已发送", "send", "default", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepare", ""} {
		body := `{"agent":"zcode","scope":"main","id":"44535ceb-a209-42bc-977b-51a58ab8225a","phase":"` + phase + `"}`
		response := env.request(http.MethodPost, "/api/sessions/"+env.threadID+"/migrate", body)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "发送结果待确认") {
			t.Fatalf("unknown source not guarded: %d %s", response.Code, response.Body.String())
		}
	}
	if counterValue(t, creates) != 0 || counterValue(t, prompts) != 0 || env.server.Creations.lookup("44535ceb-a209-42bc-977b-51a58ab8225a") != nil {
		t.Fatal("unknown source created a target or migration claim")
	}
}

func TestMigrationPendingDeferredRetryKeepsSavedSnapshot(t *testing.T) {
	env, _, _, _ := pendingMigrationHarness(t)
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := env.server.Queue.enqueue(env.threadID, id, "最初排队的任务", "queue", "default", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	requestID := "44535ceb-a209-42bc-977b-51a58ab8225a"
	path := "/api/sessions/" + env.threadID + "/migrate"
	prepare := env.request(http.MethodPost, path, `{"agent":"zcode","scope":"main","id":"`+requestID+`","phase":"prepare"}`)
	if prepare.Code != http.StatusOK {
		t.Fatalf("prepare failed: %d %s", prepare.Code, prepare.Body.String())
	}
	view := (&handler{server: env.server}).migrationSourceView(env.threadID, "main")
	view, err := env.server.migrationPendingView(env.threadID, view, "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	saved := migrationMessage("Codex", view, "zh-CN", "main", "")
	if err := env.server.Creations.setMessage(requestID, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := env.server.Queue.edit(env.threadID, id, "稍后修改的任务"); err != nil {
		t.Fatal(err)
	}
	confirm := env.request(http.MethodPost, path, `{"agent":"zcode","scope":"main","id":"`+requestID+`"}`)
	if confirm.Code != http.StatusOK || env.server.Creations.lookup(requestID).Message != saved {
		t.Fatalf("retry rewrote the durable transcript: %d %s", confirm.Code, confirm.Body.String())
	}
	entry := env.server.Creations.lookup(requestID)
	target := env.request(http.MethodGet, "/api/sessions/"+entry.ID, "").Body.String()
	if !strings.Contains(target, "最初排队的任务") || strings.Contains(target, "稍后修改的任务") {
		t.Fatalf("retry sent a different source snapshot: %s", target)
	}
}

func TestMigrationPendingMessagesShareWireBudget(t *testing.T) {
	for _, scope := range []string{"full", "main", "latest"} {
		view := transcriptView()
		view.Set("migrationPendingMessages", &jsoncompat.Array{Items: []any{"PENDING_START" + strings.Repeat("待处理任务。", 30000) + "PENDING_END"}})
		text := migrationMessage("Codex", view, "zh-CN", scope, "继续测试")
		if len(text) > 100000 || !utf8.ValidString(text) || !strings.Contains(text, "内容已截断") || !strings.Contains(text, "PENDING_START") || !strings.Contains(text, "PENDING_END") {
			t.Fatalf("%s pending message violated budget or lost boundaries: bytes=%d", scope, len(text))
		}
	}
}
