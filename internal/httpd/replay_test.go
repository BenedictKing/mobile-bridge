// Deterministic concurrency replay: a seeded (env MOBILE_BRIDGE_RACE_SEED)
// interleaving of live view updates — out-of-order tool_call_update, repeated
// stream chunks, cancelled turns, plan set/clear — against concurrent
// timeline Page/Changes/Detail readers, replaying exactly the fold/read
// boundary liveTimeline uses (foldProjection under the per-thread slot lock, clones out).
// A failing base seed is printed; MOBILE_BRIDGE_RACE_SEED=<n> replays it.
package httpd

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// replayMirror tracks the row content the projection must expose after the
// same update sequence, so the replay can assert content equality, not just
// "did not crash": row counts, plan texts, chunk merging, output updates.
type replayMirror struct {
	turns [][][]string // turns → messages → [kind, id, text, output]
}

func (m *replayMirror) appendCurrent(message []string) {
	if len(m.turns) == 0 {
		m.turns = append(m.turns, nil)
	}
	last := len(m.turns) - 1
	m.turns[last] = append(m.turns[last], message)
}

func (m *replayMirror) count() int {
	total := 0
	for _, turn := range m.turns {
		total += len(turn)
	}
	return total
}

type replayHarness struct {
	t         *testing.T
	server    *Server
	view      *acpview.View
	mirror    *replayMirror
	threadID  string
	sequence  int
	callIndex map[string][2]int // toolCallId → turn/message in the mirror
}

func newReplayHarness(t *testing.T, threadID string) *replayHarness {
	h := &replayHarness{t: t,
		server:    New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance"),
		mirror:    &replayMirror{},
		threadID:  threadID,
		callIndex: map[string][2]int{}}
	h.resetView()
	return h
}

// resetView swaps in a fresh view whose first row is a tool call — a first
// identity that cannot match the previous history, forcing the projection to
// rotate its epoch (the history-replaced path).
func (h *replayHarness) resetView() {
	h.view = acpview.NewView(h.threadID, "回放会话", "/work",
		func() float64 { return 1700000000 })
	h.mirror = &replayMirror{}
	h.callIndex = map[string][2]int{}
	h.toolCall("t0", "开路", "种子输出")
}

// fold pushes the current view through the production fold path (the same
// acpViewState → foldProjection boundary the live routes use).
func (h *replayHarness) fold() {
	h.sequence++
	snapshot := h.view.Snapshot()
	snapshot.Set("sequence", jsoncompat.Num(h.sequence))
	if _, _, ok := h.server.foldProjection(h.threadID, snapshot); !ok {
		h.t.Fatalf("foldProjection rejected the snapshot")
	}
}

func (h *replayHarness) beginTurn(text string) {
	h.view.BeginTurn(text)
	// BeginTurn opens a new turn; plan and merge bookkeeping are scoped to
	// the current turn, so the mirror must move with it.
	h.mirror.turns = append(h.mirror.turns,
		[][]string{{"userMessage", "user", text, ""}})
}

// agentChunk mirrors assistantMessage: chunks merge only with the previous
// message of the same kind carrying the same identity (repeated chunks).
func (h *replayHarness) agentChunk(kind, id, text string) {
	h.view.OnUpdate(chunkUpdate(kind, id, text))
	mirrorKind := "agentMessage"
	if kind == "agent_thought_chunk" {
		mirrorKind = "agentThought"
	}
	if len(h.mirror.turns) > 0 {
		turn := h.mirror.turns[len(h.mirror.turns)-1]
		if len(turn) > 0 {
			if last := turn[len(turn)-1]; last[0] == mirrorKind && last[1] == id {
				last[2] += text
				return
			}
		}
	}
	h.mirror.appendCurrent([]string{mirrorKind, id, text, ""})
}

func chunkUpdate(kind, id, text string) *jsoncompat.Object {
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", kind)
	content := jsoncompat.NewObject()
	content.Set("type", "text")
	content.Set("text", text)
	update.Set("content", content)
	update.Set("messageId", id)
	return update
}

func (h *replayHarness) toolCall(callID, title, output string) {
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call")
	update.Set("toolCallId", callID)
	update.Set("title", title)
	update.Set("rawInput", "输入 "+callID)
	content := jsoncompat.NewObject()
	content.Set("type", "text")
	content.Set("text", output)
	update.Set("content", content)
	h.view.OnUpdate(update)
	h.mirror.appendCurrent([]string{"toolCall", callID, title, output})
	h.callIndex[callID] = [2]int{len(h.mirror.turns) - 1,
		len(h.mirror.turns[len(h.mirror.turns)-1]) - 1}
}

// toolCallUpdate may reference an id the view never saw (the agent's updates
// can race its own tool_call announcements): unknown ids are dropped.
func (h *replayHarness) toolCallUpdate(callID, output string) {
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call_update")
	update.Set("toolCallId", callID)
	content := jsoncompat.NewObject()
	content.Set("type", "text")
	content.Set("text", output)
	update.Set("content", content)
	h.view.OnUpdate(update)
	if slot, known := h.callIndex[callID]; known {
		h.mirror.turns[slot[0]][slot[1]][3] = output
	}
}

// planSet mirrors the plan panel: one plan message per turn, updated in place
// by every non-empty update; an empty entry list clears it.
func (h *replayHarness) planSet(entries [][2]string) {
	list := &jsoncompat.Array{}
	for _, entry := range entries {
		item := jsoncompat.NewObject()
		item.Set("content", entry[0])
		item.Set("status", entry[1])
		list.Items = append(list.Items, item)
	}
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "plan")
	update.Set("entries", list)
	h.view.OnUpdate(update)
	if len(h.mirror.turns) == 0 {
		return
	}
	turnIndex := len(h.mirror.turns) - 1
	turn := h.mirror.turns[turnIndex]
	if len(entries) == 0 {
		for index, message := range turn {
			if message[0] == "plan" {
				h.mirror.turns[turnIndex] = append(turn[:index:index], turn[index+1:]...)
				return
			}
		}
		return
	}
	text := ""
	marks := map[string]string{"completed": "✓", "in_progress": "▸", "pending": "○"}
	for index, entry := range entries {
		if index > 0 {
			text += "\n"
		}
		mark, ok := marks[entry[1]]
		if !ok {
			mark = "○"
		}
		text += mark + " " + entry[0]
	}
	for _, message := range turn {
		if message[0] == "plan" {
			message[2] = text
			return
		}
	}
	h.mirror.appendCurrent([]string{"plan", "plan-now", text, ""})
}

func (h *replayHarness) endTurnCancelled() {
	h.view.EndTurn("cancelled")
}

// readPage/readChanges/readDetail replicate the locked tail of liveTimeline:
// projection methods are only ever called under the thread's slot lock.
func (h *replayHarness) readPage(limit int, before string) *jsoncompat.Object {
	slot := h.server.lookupProjection(h.threadID)
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.timeline.Page(limit, before)
}

func (h *replayHarness) readChanges(after, epoch, start string) *jsoncompat.Object {
	slot := h.server.lookupProjection(h.threadID)
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.timeline.Changes(after, epoch, start)
}

func (h *replayHarness) readDetail(cursor string, offset int, version any) (*jsoncompat.Object, error) {
	slot := h.server.lookupProjection(h.threadID)
	if slot == nil {
		return nil, nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.timeline.Detail(cursor, offset, version)
}

// TestReplayCursorSemantics pins the deterministic cursor contract before the
// randomized interleavings: changes carry only moved rows, epoch rotation
// forces resets, and stale versions rewind detail offsets.
func TestReplayCursorSemantics(t *testing.T) {
	h := newReplayHarness(t, "7eplay000-1111-4222-8333-444455556666")
	h.beginTurn("第一问")
	h.agentChunk("agent_message_chunk", "m1", "回答")
	h.toolCall("t1", "工具一", "工具输出")
	h.fold()

	page := h.readPage(100, "")
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	// resetView seeds one tool row, then the seed turn adds user/agent/tool.
	if len(rows.Items) != 4 {
		t.Fatalf("expected 4 rows after the seed turn: %v", page)
	}
	epoch, _ := page.MustGet("epoch").(string)
	agentRow, _ := rows.Items[2].(*jsoncompat.Object)
	after := page.MustGet("sequence").(json.Number).String()

	// One tool output update → exactly that row changes.
	h.toolCallUpdate("t1", "更新输出")
	h.fold()
	changes := h.readChanges(after, epoch, "")
	changed, _ := changes.MustGet("rows").(*jsoncompat.Array)
	if _, reset := changes.Get("reset"); reset || len(changed.Items) != 1 {
		t.Fatalf("expected exactly the tool row to change: %v", changes)
	}
	if row, _ := changed.Items[0].(*jsoncompat.Object); !strings.Contains(
		row.MustGet("text").(string), "更新输出") {
		t.Fatalf("changed row lost the new output: %v", row)
	}
	after = changes.MustGet("sequence").(json.Number).String()

	// No further change → empty diff.
	if changes := h.readChanges(after, epoch, ""); len(
		changes.MustGet("rows").(*jsoncompat.Array).Items) != 0 {
		t.Fatalf("expected an empty diff: %v", changes)
	}

	// start= keeps the prefix out of the diff.
	start := epoch + "." + agentRow.MustGet("key").(string)
	if changes := h.readChanges(after, epoch, start); len(
		changes.MustGet("rows").(*jsoncompat.Array).Items) != 0 {
		t.Fatalf("start cursor did not scope the diff: %v", changes)
	}

	// Plan rows join and leave again.
	h.planSet([][2]string{{"步骤一", "pending"}, {"步骤二", "in_progress"}})
	h.fold()
	page = h.readPage(100, "")
	rows, _ = page.MustGet("rows").(*jsoncompat.Array)
	if len(rows.Items) != 5 {
		t.Fatalf("plan row missing: %v", page)
	}
	planRow, _ := rows.Items[4].(*jsoncompat.Object)
	if text, _ := planRow.MustGet("text").(string); text != "○ 步骤一\n▸ 步骤二" {
		t.Fatalf("plan text mismatch: %q", text)
	}
	h.planSet(nil)
	h.fold()
	page = h.readPage(100, "")
	rows, _ = page.MustGet("rows").(*jsoncompat.Array)
	if len(rows.Items) != 4 {
		t.Fatalf("cleared plan row still present: %v", page)
	}

	// Detail: version equality pins the chunk; a stale version rewinds the
	// offset to 0. The cursor comes from the fresh page — removing the plan
	// row rewrote history and legitimately rotated the epoch.
	epoch, _ = page.MustGet("epoch").(string)
	rows, _ = page.MustGet("rows").(*jsoncompat.Array)
	agentRow, _ = rows.Items[2].(*jsoncompat.Object)
	cursor := epoch + "." + agentRow.MustGet("key").(string)
	detail, err := h.readDetail(cursor, 0, agentRow.MustGet("version"))
	if err != nil || detail.MustGet("text") != "回答" {
		t.Fatalf("detail mismatch: %v %v", detail, err)
	}
	stale, err := h.readDetail(cursor, 500, "outdated-version")
	if err != nil || stale.MustGet("offset") != jsoncompat.Num(0) {
		t.Fatalf("stale version did not rewind the offset: %v %v", stale, err)
	}

	// A cancelled turn flips every row of that turn (turnStatus moves). The
	// diff anchors on the cleared page: the epoch rotation wiped older
	// versions, so the pre-plan cursor can no longer resolve.
	after = page.MustGet("sequence").(json.Number).String()
	h.endTurnCancelled()
	h.fold()
	changes = h.readChanges(after, epoch, "")
	changed, _ = changes.MustGet("rows").(*jsoncompat.Array)
	if len(changed.Items) != 3 {
		t.Fatalf("cancel should surface all turn rows: %v", changes)
	}
	for _, item := range changed.Items {
		if row, _ := item.(*jsoncompat.Object); row.MustGet("turnStatus") != "interrupted" {
			t.Fatalf("turnStatus not propagated: %v", row)
		}
	}

	// History replacement rotates the epoch: old cursors get a reset frame.
	oldEpoch := epoch
	h.resetView()
	h.fold()
	reset := h.readChanges("0", oldEpoch, "")
	if _, hasReset := reset.Get("reset"); !hasReset {
		t.Fatalf("epoch rotation must answer with a reset: %v", reset)
	}
	stalePage := h.readPage(20, oldEpoch+".whatever")
	if _, hasReset := stalePage.Get("reset"); !hasReset {
		t.Fatalf("stale cursor page must reset: %v", stalePage)
	}
}

// TestDeterministicConcurrentReplay interleaves the seeded update storm with
// concurrent Page/Changes/Detail readers; the settled projection must equal
// the mirror. The update stream is deterministic per seed — only the reader
// interleaving varies — so content assertions stay exact.
func TestDeterministicConcurrentReplay(t *testing.T) {
	baseSeed := int64(20260101)
	if custom := os.Getenv("MOBILE_BRIDGE_RACE_SEED"); custom != "" {
		parsed, err := strconv.ParseInt(custom, 10, 64)
		if err != nil {
			t.Fatalf("MOBILE_BRIDGE_RACE_SEED=%q is not an integer", custom)
		}
		baseSeed = parsed
	}
	for round := 0; round < 3; round++ {
		seed := baseSeed + int64(round)
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			t.Logf("replay seed %d (replay with MOBILE_BRIDGE_RACE_SEED=%d)", seed, seed)
			runReplayRound(t, seed)
		})
	}
}

func runReplayRound(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	h := newReplayHarness(t, "7eplayrnd-1111-4222-8333-444455556666")
	h.beginTurn("随机问")
	h.fold()

	stop := make(chan struct{})
	var readers sync.WaitGroup
	var failures sync.Map
	fail := func(format string, args ...any) {
		failures.LoadOrStore(fmt.Sprintf(format, args...), true)
	}
	for id := 0; id < 3; id++ {
		readers.Add(1)
		go func(id int) {
			defer readers.Done()
			// Each reader owns its rng: the harness never shares rand state
			// across goroutines, keeping the update stream deterministic.
			readerRNG := rand.New(rand.NewSource(seed*100 + int64(id)))
			lastEpoch, lastSequence := "", ""
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch readerRNG.Intn(3) {
				case 0:
					page := h.readPage(1+readerRNG.Intn(20), "")
					if page == nil {
						continue
					}
					if epoch, _ := page.MustGet("epoch").(string); epoch != "" {
						lastEpoch = epoch
						lastSequence = page.MustGet("sequence").(json.Number).String()
					}
					assertRowsWellFormed(fail, page)
				case 1:
					changes := h.readChanges(lastSequence, lastEpoch, "")
					if changes == nil {
						continue
					}
					if _, reset := changes.Get("reset"); !reset && lastEpoch != "" {
						if epoch, _ := changes.MustGet("epoch").(string); epoch != lastEpoch {
							fail("changes epoch moved without reset")
						}
					}
				default:
					page := h.readPage(20, "")
					if page == nil {
						continue
					}
					rows, _ := page.MustGet("rows").(*jsoncompat.Array)
					if len(rows.Items) == 0 {
						continue
					}
					row, _ := rows.Items[readerRNG.Intn(len(rows.Items))].(*jsoncompat.Object)
					cursor, _ := page.MustGet("epoch").(string)
					cursor += "." + row.MustGet("key").(string)
					detail, err := h.readDetail(cursor, 0, row.MustGet("version"))
					if err != nil {
						// A stale cursor is the contract, not a failure: the
						// epoch rotates when history is rewritten (plan rows
						// removed mid-history) between the page and the read.
						if !strings.Contains(err.Error(), "请重新打开聊天") {
							fail("detail error: %v", err)
						}
						continue
					}
					// Version equality means identical content by
					// construction (versions are content hashes).
					if detail.MustGet("version") == row.MustGet("version") &&
						detail.MustGet("text") != row.MustGet("text") {
						fail("detail text diverged from the page row")
					}
				}
			}
		}(id)
	}

	// The seeded update storm.
	steps := 50 + rng.Intn(30)
	chunkIDs := []string{"m1", "m2", "m3"}
	knownTools := []string{"t1", "t2"}
	lastChunk := ""
	for step := 0; step < steps; step++ {
		switch rng.Intn(10) {
		case 0, 1:
			kind := "agent_message_chunk"
			if rng.Intn(4) == 0 {
				kind = "agent_thought_chunk"
			}
			lastChunk = fmt.Sprintf("片段%d", step)
			h.agentChunk(kind, chunkIDs[rng.Intn(len(chunkIDs))], lastChunk+" ")
		case 2:
			h.toolCall(fmt.Sprintf("t%d", step), "工具", fmt.Sprintf("输出%d", step))
		case 3, 4:
			// Half the updates target ids that do not exist (out-of-order or
			// lost announcements): they must be dropped, not crash.
			if rng.Intn(2) == 0 {
				h.toolCallUpdate("missing-"+strconv.Itoa(step), "孤儿")
			} else {
				h.toolCallUpdate(knownTools[rng.Intn(len(knownTools))],
					fmt.Sprintf("刷新%d", step))
			}
		case 5:
			h.planSet([][2]string{{fmt.Sprintf("计划%d", step), "pending"}})
		case 6:
			h.planSet(nil)
		case 7:
			h.endTurnCancelled()
		default:
			h.beginTurn(fmt.Sprintf("追问%d", step))
		}
		h.fold()
	}

	close(stop)
	readers.Wait()

	// The settled projection must equal the mirror: same row count, and the
	// final chunk text present exactly once the mirror expects.
	page := h.readPage(100, "")
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	if len(rows.Items) != h.mirror.count() {
		t.Fatalf("row count %d != mirrored %d", len(rows.Items), h.mirror.count())
	}
	found := false
	for _, item := range rows.Items {
		if row, _ := item.(*jsoncompat.Object); strings.Contains(
			row.MustGet("text").(string), lastChunk) {
			found = true
		}
	}
	if lastChunk != "" && !found {
		t.Fatalf("final chunk text %q missing from the projection", lastChunk)
	}
	failures.Range(func(key, _ any) bool {
		t.Errorf("replay reader failure: %v", key)
		return true
	})
}

func assertRowsWellFormed(fail func(string, ...any), page *jsoncompat.Object) {
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	previous := -1
	keys := map[string]bool{}
	for _, item := range rows.Items {
		row, _ := item.(*jsoncompat.Object)
		number, _ := row.MustGet("order").(json.Number)
		order, _ := number.Int64()
		key, _ := row.MustGet("key").(string)
		if int(order) <= previous {
			fail("row order not ascending: %v then %v", previous, order)
		}
		if keys[key] {
			fail("duplicate row key %q", key)
		}
		keys[key] = true
		previous = int(order)
	}
}
