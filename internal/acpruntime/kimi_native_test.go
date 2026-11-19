package acpruntime

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/kimi"
)

func kimiNativeWire(t *testing.T, content string) (string, string) {
	t.Helper()
	home := t.TempDir()
	wire := filepath.Join(home, "sessions", "workspace", "sess-1", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wire), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wire, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, wire
}

func appendKimiNativeWire(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestKimiTextDoesNotSettleNativeTurn(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"<notification>后台任务已停止</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"tool.call","toolCallId":"c1","name":"Bash","args":{"command":"npm test"}}}
`)
	session := nativeSession()
	session.kind = "kimi"
	session.native = kimi.TranscriptReader(home)
	settled := make(chan struct{}, 1)
	session.nativeSettle = func(*Session) { settled <- struct{}{} }
	session.syncNative()
	if session.View.Status != "active" {
		t.Fatalf("initial status=%s", session.View.Status)
	}
	appendKimiNativeWire(t, wire, `{"type":"context.append_loop_event","event":{"type":"tool.result","toolCallId":"c1","result":"ok"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"测试通过，接下来继续修改"}}}
`)
	session.syncNative()
	if session.View.Status != "active" {
		t.Errorf("intermediate text prematurely settled view: status=%s", session.View.Status)
	}
	select {
	case <-settled:
		t.Error("queue settle hook fired before turn.ended")
	case <-time.After(100 * time.Millisecond):
	}
	appendKimiNativeWire(t, wire, `{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"最终答复 FINAL_REVIEW_RESULT"}}}
{"type":"turn.ended","reason":"completed"}
`)
	session.syncNative()
	if !strings.Contains(string(jsoncompat.EncodeCompact(session.View.TipTurn())), "FINAL_REVIEW_RESULT") {
		t.Error("final native response never reached the view")
	}
}

func TestEmptyPromptDoesNotRollBackToOlderRunningStore(t *testing.T) {
	session := nativeSession()
	session.View.ApplyNative(&acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "旧问题")}})
	session.nativeBootstrapped = true
	session.View.BeginTurn("请把端口改成9000")
	session.View.EndTurn("end_turn")
	session.native = func(string) (*acpview.NativeSnapshot, error) {
		return &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "旧问题")}}, nil
	}
	session.syncNative()
	if got := firstUserText(session.View.TipTurn()); got != "请把端口改成9000" {
		t.Fatalf("legitimate user prompt was erased by old running store: %q", got)
	}
}

func TestCompletedNativeNotificationRecoversEmptyContinue(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	session := nativeSession()
	session.kind = "kimi"
	session.native = kimi.TranscriptReader(home)
	session.syncNative()
	settled := make(chan struct{}, 2)
	session.nativeSettle = func(*Session) { settled <- struct{}{} }
	session.View.BeginTurn("继续")
	session.promptRunning = true
	appendKimiNativeWire(t, wire, `{"type":"turn.prompt","input":"<notification>后台任务已停止</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"原生任务的最终结果"}}}
{"type":"turn.ended","reason":"completed"}
`)
	session.syncNative()
	session.promptRunning = false
	session.View.EndTurn("end_turn")
	session.syncNative()
	if len(session.View.Turns) != 2 || firstUserText(session.View.TipTurn()) == "继续" {
		t.Fatal("completed notification remains invisible when the active phase was not sampled")
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("completed notification did not release the queue")
	}
	session.syncNative()
	select {
	case <-settled:
		t.Fatal("completed notification released the queue twice")
	default:
	}
}

func TestKimiNativePreservesRepeatedAndNormalPrompts(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"继续","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	session := nativeSession()
	session.kind = "kimi"
	session.native = kimi.TranscriptReader(home)
	session.syncNative()
	for _, text := range []string{"继续", "继续", "请把端口改成9000"} {
		session.View.BeginTurn(text)
		session.View.EndTurn("end_turn")
	}
	appendKimiNativeWire(t, wire, `{"type":"turn.prompt","input":"<notification>任务完成</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"继续处理"}}}
`)
	session.syncNative()
	assertMessages := func(continues, normal int) {
		t.Helper()
		counts := map[string]int{}
		for _, turn := range session.View.Turns {
			for _, item := range turn.MustGet("messages").(*jsoncompat.Array).Items {
				message := item.(*jsoncompat.Object)
				if message.MustGet("role") == "user" {
					counts[stringField(message, "text")]++
				}
			}
		}
		if counts["继续"] != continues || counts["请把端口改成9000"] != normal {
			t.Fatalf("user messages lost or duplicated: %v", counts)
		}
	}
	if session.View.Status != "active" {
		t.Fatal("notification did not reclaim the busy state")
	}
	assertMessages(3, 1)
	appendKimiNativeWire(t, wire, `{"type":"turn.ended","reason":"completed"}
{"type":"turn.prompt","input":"继续","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	session.syncNative()
	assertMessages(3, 1)
	if len(session.nativePending) != 2 {
		t.Fatalf("pending=%d, want 2", len(session.nativePending))
	}
	appendKimiNativeWire(t, wire, `{"type":"turn.prompt","input":"<notification>后续任务完成</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"继续检查"}}}
`)
	session.syncNative()
	assertMessages(3, 1)
	if len(session.nativePending) != 2 {
		t.Fatal("an earlier native continue acknowledged a second queued continue")
	}
	appendKimiNativeWire(t, wire, `{"type":"turn.ended","reason":"completed"}
{"type":"turn.prompt","input":"继续","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
{"type":"turn.prompt","input":"请把端口改成9000","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	session.syncNative()
	assertMessages(3, 1)
	if len(session.nativePending) != 0 {
		t.Fatal("recorded prompts were not acknowledged")
	}
}

func TestKimiNativeRejectsUnverifiedTaskAdvance(t *testing.T) {
	for _, kind := range []string{"kimi", "zcode"} {
		for _, origin := range []string{"user", "task", ""} {
			t.Run(kind+"/"+origin, func(t *testing.T) {
				session := nativeSession()
				session.kind = kind
				session.native = func(string) (*acpview.NativeSnapshot, error) {
					return &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "原始问题")}}, nil
				}
				session.syncNative()
				session.View.BeginTurn("新的问题")
				session.View.EndTurn("end_turn")
				snapshot := &acpview.NativeSnapshot{TurnOrigins: []string{origin}, Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "原始问题")}}
				if session.shouldApplyNative(snapshot) {
					t.Fatal("older store was allowed to erase a user prompt")
				}
				snapshot.Turns = append(snapshot.Turns, nativeTurn("t2", "inProgress", "未核实来源"))
				snapshot.TurnOrigins = append(snapshot.TurnOrigins, origin)
				want := kind == "kimi" && origin == "task"
				if got := session.shouldApplyNative(snapshot); got != want {
					t.Fatalf("apply=%v, want %v", got, want)
				}
			})
		}
	}
}

func TestNativeSnapshotReadDuringPromptStartIsDeferred(t *testing.T) {
	session := nativeSession()
	session.native = func(string) (*acpview.NativeSnapshot, error) {
		session.mu.Lock()
		session.promptRunning = true
		session.mu.Unlock()
		return &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "桌面任务")}}, nil
	}
	session.syncNative()
	if len(session.View.Turns) != 0 {
		t.Fatal("native fold interrupted an ACP prompt")
	}
	session.promptRunning = false
	session.native = func(string) (*acpview.NativeSnapshot, error) { return nil, nil }
	session.syncNative()
	if session.View.Status != "active" || firstUserText(session.View.TipTurn()) != "桌面任务" {
		t.Fatal("consumed snapshot was lost after the prompt finished")
	}
}

func TestNativeLagGuardRechecksConsumedSnapshot(t *testing.T) {
	session := nativeSession()
	session.native = func(string) (*acpview.NativeSnapshot, error) {
		return &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "同一个问题")}}, nil
	}
	session.syncNative()
	session.native = func(string) (*acpview.NativeSnapshot, error) {
		return &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "同一个问题")}}, nil
	}
	session.syncNative()
	if session.View.Status != "idle" {
		t.Fatal("store lag bypassed the grace period")
	}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return nil, nil }
	session.nativeLagSince = time.Now().Add(-2 * nativeStoreLagGrace)
	session.syncNative()
	if session.View.Status != "active" {
		t.Fatal("unchanged reader cache prevented persistent running state from reclaiming the view")
	}
}

func TestKimiPromptSettlementFoldsNotificationBeforeHook(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	python, path := fakeAgentScript(t)
	manager := NewCommand([]string{python, "-u", path})
	manager.Kind = "kimi"
	manager.Native = kimi.TranscriptReader(home)
	manager.NativeInterval = time.Hour
	seen := make(chan string, 1)
	manager.AutoContinue = func(session *Session) { seen <- stringField(session.Snapshot(), "status") }
	defer manager.Close()
	session, err := manager.Open("sess-1", "/w", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	appendKimiNativeWire(t, wire, `{"type":"turn.prompt","input":"<notification>任务已停止</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"还在处理"}}}
`)
	if err := session.Prompt("继续", time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-seen:
		if status != "active" {
			t.Fatalf("settle hook saw %s before the native notification was folded", status)
		}
	case <-time.After(time.Second):
		t.Fatal("ACP settlement hook did not run")
	}
}

func TestConcurrentNativeReadsCannotRollBackNewerProgress(t *testing.T) {
	session := nativeSession()
	entered := make(chan int32, 2)
	release := make(chan struct{})
	var reads int32
	session.native = func(string) (*acpview.NativeSnapshot, error) {
		read := atomic.AddInt32(&reads, 1)
		entered <- read
		turn := nativeTurn("t1", "inProgress", "同一个问题")
		if read == 1 {
			<-release
		} else {
			message := jsoncompat.NewObject()
			message.Set("role", "assistant")
			message.Set("text", "最新进度")
			turn.MustGet("messages").(*jsoncompat.Array).Items = append(turn.MustGet("messages").(*jsoncompat.Array).Items, message)
		}
		return &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{turn}}, nil
	}
	done := make(chan struct{}, 2)
	go func() { session.syncNative(); done <- struct{}{} }()
	<-entered
	go func() { session.syncNative(); done <- struct{}{} }()
	finished := 0
	select {
	case <-entered:
		<-done // A concurrent newer read landed while the old read was held.
		finished++
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for finished < 2 {
		select {
		case <-done:
			finished++
		case <-time.After(time.Second):
			t.Fatal("native reads did not finish")
		}
	}
	if !strings.Contains(string(jsoncompat.EncodeCompact(session.Snapshot())), "最新进度") {
		t.Fatal("a slower old read overwrote newer native progress")
	}
}

func TestSharedKimiReaderAllowsFreshHistoryAfterLiveClose(t *testing.T) {
	home, _ := kimiNativeWire(t, `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"答复"}}}
{"type":"turn.ended","reason":"completed"}
`)
	reader := kimi.NewTranscriptReader(home)
	live := nativeSession()
	live.native = reader.Load
	live.syncNative()
	live.Close()
	manager := NewCommand(nil)
	manager.Native = reader.Load
	manager.NativeTranscript = reader.Load
	manager.NativeReset = reader.Reset
	defer manager.Close()
	if _, err := manager.OpenHistory("sess-1", "/w"); err != nil {
		t.Fatalf("history unreadable after the live reader consumed the unchanged signature: %v", err)
	}
}
