//go:build !windows

package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/codex"
)

func TestNewCodexChatDefaultsToCLIWithoutOpeningDesktop(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	id := "9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	f.env.bridge.Executable = codexMigrationAppServer(t, id)
	openDir := t.TempDir()
	trace := filepath.Join(openDir, "open-called")
	if err := os.WriteFile(filepath.Join(openDir, "open"), []byte("#!/bin/sh\ntouch '"+trace+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", openDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	body := `{"agent":"codex","cwd":` + jsonText(f.cwd) + `,"title":"空聊天","id":"8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`
	for range 2 {
		response := f.env.request("POST", "/api/sessions", body)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"executionTarget": "cli"`) || !strings.Contains(response.Body.String(), `"opened": false`) {
			t.Fatalf("new CLI chat failed: %d %s", response.Code, response.Body.String())
		}
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("CLI creation opened the desktop App")
	}
	entry := f.env.server.Creations.lookup("8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if entry.Execution != "cli" || entry.ProjectCwd != f.cwd || !f.env.bridge.IsCLI(id) || f.resumes.Load() != 0 || f.sends.Load() != 0 {
		t.Fatalf("creation acquired a writer or lost execution routing: %+v", entry)
	}
	conflict := strings.Replace(body, `"title":"空聊天"`, `"title":"空聊天","execution":"desktop"`, 1)
	if response := f.env.request("POST", "/api/sessions", conflict); response.Code != http.StatusBadRequest {
		t.Fatalf("creation retry changed execution: %d %s", response.Code, response.Body.String())
	}
}

func TestDesktopFailureNeverSelectsCLI(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	base := "/api/sessions/" + f.env.threadID
	for _, action := range []string{"reconnect", "send"} {
		response := f.env.request("POST", base+"/"+action, `{"activate":true,"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"保留草稿"}`)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "显式交给网关") {
			t.Fatalf("missing desktop error: %d %s", response.Code, response.Body.String())
		}
	}
	if f.env.bridge.IsCLI(f.env.threadID) || f.resumes.Load() != 0 || f.sends.Load() != 0 {
		t.Fatal("connection failure acquired a different execution owner")
	}
}

func TestDesktopHistoryGetWithoutOwnerRemainsReadable(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	response := f.env.request("GET", "/api/sessions/"+f.env.threadID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"executionTarget": "desktop"`) || f.resumes.Load() != 0 {
		t.Fatalf("offline desktop history was not readable: %d %s", response.Code, response.Body.String())
	}
}

func TestCLIWriterConflictKeepsDesktopRoute(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	f.writerBusy.Store(true)
	response := f.env.request("POST", "/api/sessions/"+f.env.threadID+"/execution", `{"target":"cli"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "另一个 Codex 应用占用") {
		t.Fatalf("missing writer conflict: %d %s", response.Code, response.Body.String())
	}
	if f.env.bridge.IsCLI(f.env.threadID) || f.env.server.Creations.executionForTarget(f.env.threadID) != "desktop" || f.sends.Load() != 0 {
		t.Fatal("failed handoff changed execution routing or sent a message")
	}
}

func TestCLIHistoryDoesNotResumeAndReportsTargetSeparately(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	if err := f.env.server.Creations.setTargetExecution(f.env.threadID, f.cwd, "cli"); err != nil {
		t.Fatal(err)
	}
	f.env.server.RegisterCodexCLIThreads()
	response := f.env.request("GET", "/api/sessions/"+f.env.threadID+"/timeline", "")
	if response.Code != http.StatusOK || f.resumes.Load() != 0 || !strings.Contains(response.Body.String(), `"executionTarget": "cli"`) || !strings.Contains(response.Body.String(), `"executionOwner": ""`) {
		t.Fatalf("history acquired a writer or confused target and owner: %d %s", response.Code, response.Body.String())
	}
	result := (&handler{server: f.env.server}).createResponse(f.env.threadID, "codex", "空聊天", f.cwd)
	if result.MustGet("opened") != false || result.MustGet("executionTarget") != "cli" {
		t.Fatalf("CLI creation should not open the desktop: %v", result)
	}
}

func TestHandoffRejectsQueuedAndUnknownMessages(t *testing.T) {
	for _, status := range []string{SubQueued, SubUnknown} {
		t.Run(status, func(t *testing.T) {
			f := newOrdinaryCLIFixture(t, false, false)
			q := f.env.server.Queue
			key := submissionKey(f.env.threadID, "pending")
			q.entries[key] = &submissionEntry{ID: "pending", ThreadID: f.env.threadID, Status: status}
			q.order = append(q.order, key)
			response := f.env.request("POST", "/api/sessions/"+f.env.threadID+"/execution", `{"target":"cli"}`)
			if response.Code != http.StatusConflict || f.resumes.Load() != 0 {
				t.Fatalf("pending message was handed off: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestExplicitDesktopRouteOverridesLegacyCLIRecords(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	ledger := f.env.server.Creations
	_, _, err := ledger.begin("legacy", &creationEntry{Agent: "codex", ID: f.env.threadID, ProjectCwd: f.cwd, Execution: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	f.env.server.RegisterCodexCLIThreads()
	if _, err := f.env.bridge.WatchForWrite(f.env.threadID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.env.bridge.HandOffCLI(f.env.threadID, f.cwd, func() error { return ledger.setTargetExecution(f.env.threadID, f.cwd, "desktop") }); err != nil {
		t.Fatal(err)
	}
	if f.env.bridge.IsCLI(f.env.threadID) || f.unsubscribes.Load() != 1 {
		t.Fatal("CLI writer was not released")
	}
	response := f.env.request("GET", "/api/sessions/"+f.env.threadID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "原对话的回答") || !strings.Contains(response.Body.String(), `"executionTarget": "desktop"`) {
		t.Fatalf("handoff lost the readable conversation: %d %s", response.Code, response.Body.String())
	}
	f.env.bridge.Close()
	next := codex.New(f.env.bridge.Home, "unused-desktop")
	t.Cleanup(next.Close)
	f.env.server.Codex = next
	f.env.server.Creations = mustLoadCreations(t, ledger.path)
	f.env.server.RegisterCodexCLIThreads()
	if next.IsCLI(f.env.threadID) || f.env.server.Creations.executionForTarget(f.env.threadID) != "desktop" {
		t.Fatal("restart restored stale CLI route")
	}
}

func TestFailedDesktopPersistenceKeepsCLIRouteWithoutWriter(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	f.env.bridge.RegisterCLI(f.env.threadID, f.cwd)
	if _, err := f.env.bridge.WatchForWrite(f.env.threadID, 0); err != nil {
		t.Fatal(err)
	}
	ledger := f.env.server.Creations
	ledger.path = filepath.Join(t.TempDir(), "missing", "creations.json")
	err := f.env.bridge.HandOffCLI(f.env.threadID, f.cwd, func() error { return ledger.setTargetExecution(f.env.threadID, f.cwd, "desktop") })
	if err == nil || !f.env.bridge.IsCLI(f.env.threadID) || f.env.bridge.ExecutionOwner(f.env.threadID) != "" || f.unsubscribes.Load() != 1 {
		t.Fatalf("failed persistence lost the old route or retained a writer: %v", err)
	}
}

func TestHandoffRejectsNativeBackgroundWork(t *testing.T) {
	for _, kind := range []string{"terminal", "subagent"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrdinaryCLIFixture(t, false, false)
			f.env.bridge.RegisterCLI(f.env.threadID, f.cwd)
			if _, err := f.env.bridge.WatchForWrite(f.env.threadID, 0); err != nil {
				t.Fatal(err)
			}
			if kind == "terminal" {
				f.background.Store(true)
			} else {
				f.busyDescendant.Store(true)
			}
			persisted := false
			err := f.env.bridge.HandOffCLI(f.env.threadID, f.cwd, func() error { persisted = true; return nil })
			if err == nil || persisted || !f.env.bridge.IsCLI(f.env.threadID) || f.unsubscribes.Load() != 0 {
				t.Fatalf("busy native work was released: error=%v persisted=%v", err, persisted)
			}
		})
	}
}
