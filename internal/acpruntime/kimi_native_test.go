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

func TestKimiPromptSettlementWaitsForNotificationChain(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	python, path := fakeAgentScript(t)
	manager := NewCommand([]string{python, "-u", path})
	manager.Kind = "kimi"
	reader := kimi.NewTranscriptReader(home)
	manager.Native = reader.Load
	manager.NativeFresh = reader.LoadFresh
	manager.NativeInterval = time.Hour
	seen := make(chan string, 1)
	manager.AutoContinue = func(session *Session) { seen <- stringField(session.Snapshot(), "status") }
	defer manager.Close()
	session, err := manager.Open("sess-1", "/w", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.autoContinueDelay = 20 * time.Millisecond
	session.mu.Unlock()
	appendKimiNativeWire(t, wire, `{"type":"turn.prompt","input":"<notification>任务已停止</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"还在处理"}}}
`)
	if err := session.Prompt("继续", time.Second); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	select {
	case status := <-seen:
		t.Fatalf("notification chain settled early with status %s", status)
	case <-time.After(50 * time.Millisecond):
	}
	appendKimiNativeWire(t, wire, `{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"原生任务的最终结果"}}}
{"type":"turn.ended","reason":"completed"}
`)
	session.syncNative()
	select {
	case status := <-seen:
		if status != "idle" {
			t.Fatalf("native chain settle hook saw status %s", status)
		}
	case <-time.After(time.Second):
		t.Fatal("native notification chain settlement did not run")
	}
}

func TestKimiAutoContinueSettleDelayCoversThreeSecondsAndTwoPolls(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		override time.Duration
		want     time.Duration
	}{
		{name: "default poll", interval: 800 * time.Millisecond, want: 3 * time.Second},
		{name: "1500ms poll", interval: 1500 * time.Millisecond, want: 3 * time.Second},
		{name: "slow poll", interval: 2 * time.Second, want: 4 * time.Second},
		{name: "test override", interval: time.Hour, override: 25 * time.Millisecond, want: 25 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := nativeSession()
			session.nativeInterval = test.interval
			session.autoContinueDelay = test.override
			if got := session.autoContinueSettleDelayLocked(); got != test.want {
				t.Fatalf("settle delay=%s, want %s", got, test.want)
			}
		})
	}
}

func TestKimiAutoContinueRefreshRejectsNewNativeTurn(t *testing.T) {
	session := nativeSession()
	session.kind = "kimi"
	session.kimiAutoChain = true
	settled := nativeTurn("t1", "completed", "原始任务")
	initial := &acpview.NativeSnapshot{TurnOrigins: []string{"user"}, Turns: []*jsoncompat.Object{settled}}
	if !session.View.ApplyNative(initial) {
		t.Fatal("initial native snapshot was not applied")
	}
	session.nativeBootstrapped = true
	session.nativeTurns = 1
	running := &acpview.NativeSnapshot{
		TurnOrigins: []string{"user", "task"},
		Turns: []*jsoncompat.Object{
			nativeTurn("t1", "completed", "原始任务"),
			nativeTurn("t2", "inProgress", "迟到的后台通知"),
		},
	}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return running, nil }
	generation := session.AutoContinueGeneration()
	if err := session.AutoContinuePromptIfCurrent("t1", generation, "继续", time.Hour); err == nil {
		t.Fatal("automatic continuation submitted while native Kimi task was running")
	}
	if session.View.Status != "active" || stringField(session.View.TipTurn(), "id") != "t2" {
		t.Fatalf("latest native turn was not reconciled: status=%s tip=%s", session.View.Status, stringField(session.View.TipTurn(), "id"))
	}
	if session.autoContinueRuns != 0 {
		t.Fatal("rejected continuation consumed the automatic budget")
	}
}

func TestKimiNativeNotificationCancelsPendingSettle(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`)
	python, path := fakeAgentScript(t)
	manager := NewCommand([]string{python, "-u", path})
	manager.Kind = "kimi"
	reader := kimi.NewTranscriptReader(home)
	manager.Native = reader.Load
	manager.NativeFresh = reader.LoadFresh
	manager.NativeInterval = time.Hour
	seen := make(chan struct{}, 1)
	manager.AutoContinue = func(*Session) { seen <- struct{}{} }
	defer manager.Close()
	session, err := manager.Open("sess-1", "/w", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.autoContinueDelay = 40 * time.Millisecond
	session.mu.Unlock()
	if err := session.Prompt("继续", time.Second); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	deadline := time.Now().Add(time.Second)
	for {
		session.mu.Lock()
		pending := session.autoContinueTimer != nil
		session.mu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACP settlement was not scheduled")
		}
		time.Sleep(time.Millisecond)
	}
	appendKimiNativeWire(t, wire, `{"type":"turn.prompt","input":"<notification>迟到的后台任务</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"继续处理"}}}
`)
	session.syncNative()
	select {
	case <-seen:
		t.Fatal("late native notification did not cancel the stale settlement")
	case <-time.After(80 * time.Millisecond):
	}
	appendKimiNativeWire(t, wire, `{"type":"turn.ended","reason":"completed"}
`)
	session.syncNative()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("final native notification did not settle the chain")
	}
}

func TestKimiDesktopNativeSettleDoesNotAutoContinue(t *testing.T) {
	session := nativeSession()
	session.kind = "kimi"
	session.readOnly = true
	session.desktopOwned = true
	seen := make(chan struct{}, 1)
	session.autoContinue = func(*Session) { seen <- struct{}{} }
	running := &acpview.NativeSnapshot{TurnOrigins: []string{"task"}, Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "后台通知")}}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return running, nil }
	session.syncNative()
	done := &acpview.NativeSnapshot{TurnOrigins: []string{"task"}, Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "后台通知")}}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return done, nil }
	session.syncNative()
	select {
	case <-seen:
		t.Fatal("desktop native turn triggered automatic continuation")
	default:
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

func TestKimiPromptOnlyNotificationChainStaysVisible(t *testing.T) {
	home, wire := kimiNativeWire(t, `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
{"type":"turn.prompt","input":"<notification>第一个后台任务</notification>","origin":{"kind":"task"}}
`)
	session := nativeSession()
	session.kind = "kimi"
	session.native = kimi.TranscriptReader(home)
	session.syncNative()
	if session.View.Status != "active" {
		t.Fatal("initial native task is not active")
	}
	session.View.BeginTurn("继续")
	session.View.EndTurn("end_turn")
	appendKimiNativeWire(t, wire, `{"type":"turn.ended","reason":"completed"}
{"type":"turn.prompt","input":"继续","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed","stopReason":"prompt_only"}
{"type":"turn.prompt","input":"<notification>第二个后台任务</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"正在处理第二个任务"}}}
`)
	session.syncNative()
	if session.View.Status != "active" || !strings.Contains(string(jsoncompat.EncodeCompact(session.View.TipTurn())), "正在处理第二个任务") {
		t.Fatalf("first prompt_only chain did not follow native task: status=%s tip=%s", session.View.Status, jsoncompat.EncodeCompact(session.View.TipTurn()))
	}
	session.View.BeginTurn("继续")
	session.View.EndTurn("end_turn")
	appendKimiNativeWire(t, wire, `{"type":"turn.ended","reason":"completed"}
{"type":"turn.prompt","input":"继续","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed","stopReason":"prompt_only"}
{"type":"turn.prompt","input":"<notification>第三个后台任务</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"正在处理第三个任务"}}}
`)
	session.syncNative()
	if session.View.Status != "active" || !strings.Contains(string(jsoncompat.EncodeCompact(session.View.TipTurn())), "正在处理第三个任务") {
		t.Fatalf("second prompt_only chain did not follow native task: status=%s tip=%s", session.View.Status, jsoncompat.EncodeCompact(session.View.TipTurn()))
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
