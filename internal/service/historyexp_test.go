package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

// historyAdapter mirrors conformance/historyexp/oracle.py's StubAdapter.
type historyAdapter struct {
	total int
}

func (a *historyAdapter) Label() string                      { return "Stub Agent" }
func (a *historyAdapter) HasCapability(name string) bool     { return name == "chat" }
func (a *historyAdapter) IsActive(session *LiveSession) bool { return false }

func (a *historyAdapter) DispatchTurn(session *LiveSession, submissionID string,
	entry *jsoncompat.Object) (any, error) {
	return nil, nil
}

func (a *historyAdapter) SavedHistory(session *LiveSession) *jsoncompat.Object {
	limit := session.HistoryLimit
	start := a.total - limit
	if start < 0 {
		start = 0
	}
	turns := &jsoncompat.Array{}
	for i := start; i < a.total; i++ {
		turns.Items = append(turns.Items, historyTurn(fmt.Sprintf("t%02d", i),
			fmt.Sprintf("历史 %02d", i)))
	}
	turns.Items = append(turns.Items, historyTurn("live", "实时尾巴"))
	state := jsoncompat.NewObject()
	state.Set("id", session.ID)
	state.Set("turns", turns)
	pagination := jsoncompat.NewObject()
	pagination.Set("hasLoadedOldest", limit >= a.total)
	state.Set("turnsPagination", pagination)
	return state
}

func historyTurn(id, text string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("turnId", id)
	item := jsoncompat.NewObject()
	item.Set("type", "agentMessage")
	item.Set("id", id)
	item.Set("text", text)
	turn.Set("items", &jsoncompat.Array{Items: []any{item}})
	return turn
}

// TestHistoryExpansionParity replays conformance/historyexp/scenarios: the
// saved-history window grows by 50 when paging reaches the loaded edge.
func TestHistoryExpansionParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "historyexp", "scenarios")
	golden, err := os.ReadFile(filepath.Join(scenarioDir, "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	const threadID = "sess-hist"
	lineIndex := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(scenarioDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		scenario, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		root := scenario.(*jsoncompat.Object)
		name := root.MustGet("name")
		for index, stepValue := range root.MustGet("steps").(*jsoncompat.Array).Items {
			step := stepValue.(*jsoncompat.Object)
			total, _ := step.MustGet("total").(json.Number)
			totalValue, _ := total.Float64()
			adapter := &historyAdapter{total: int(totalValue)}
			bridge := NewBridge("local", "stub", []string{"chat"})
			bridge.Adapter = adapter
			counter := 0
			session := &LiveSession{ID: threadID, Connected: true, HistoryLimit: 20,
				Timeline: timeline.New(func() string {
					value := fmt.Sprintf("%032x", counter)
					counter++
					return value
				})}
			bridge.Live[threadID] = session
			session.SetHistory(adapter.SavedHistory(session))
			state := jsoncompat.NewObject()
			state.Set("id", threadID)
			state.Set("turns", &jsoncompat.Array{})
			pagination := jsoncompat.NewObject()
			pagination.Set("hasLoadedOldest", false)
			state.Set("turnsPagination", pagination)
			session.State = state

			var pages []*jsoncompat.Object
			records := &jsoncompat.Array{}
			for _, actionValue := range step.MustGet("steps").(*jsoncompat.Array).Items {
				action := actionValue.(*jsoncompat.Object)
				spec := action.MustGet("read_page").(*jsoncompat.Object)
				limit := 20
				if raw, ok := spec.Get("limit"); ok && raw != nil {
					fmt.Sscanf(raw.(json.Number).String(), "%d", &limit)
				}
				before := ""
				if flag, _ := spec.MustGet("before_last").(bool); flag && len(pages) > 0 {
					before, _ = pages[len(pages)-1].MustGet("before").(string)
				}
				result, err := bridge.TimelineRead("page", session, ReadOptions{
					Limit: limit, Before: before})
				if err != nil {
					t.Fatalf("read_page: %v", err)
				}
				pages = append(pages, result)
				rowTexts := &jsoncompat.Array{}
				for _, rowValue := range result.MustGet("rows").(*jsoncompat.Array).Items {
					text, _ := rowValue.(*jsoncompat.Object).MustGet("text").(string)
					rowTexts.Items = append(rowTexts.Items, text)
				}
				out := jsoncompat.NewObject()
				out.Set("rows", rowTexts)
				out.Set("hasMore", result.MustGet("hasMore"))
				out.Set("historyLimit", jsoncompat.Num(session.HistoryLimit))
				record := jsoncompat.NewObject()
				record.Set("op", "read_page")
				record.Set("out", out)
				records.Items = append(records.Items, record)
			}
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", "historyexp")
			out := jsoncompat.NewObject()
			out.Set("records", records)
			record.Set("out", out)
			produced := string(jsoncompat.EncodeCompact(record))
			if lineIndex >= len(goldenLines) {
				t.Fatalf("%s step %d: golden ran out of lines", entry.Name(), lineIndex)
			}
			if produced != goldenLines[lineIndex] {
				offset := firstDifference(goldenLines[lineIndex], produced)
				start := max(0, offset-70)
				t.Errorf("parity mismatch (%s line %d) at byte %d:\n want: …%s…\n  got: …%s…",
					entry.Name(), lineIndex, offset,
					goldenLines[lineIndex][start:min(len(goldenLines[lineIndex]), offset+110)],
					produced[start:min(len(produced), offset+110)])
			}
			lineIndex++
		}
	}
	if lineIndex != len(goldenLines) {
		t.Errorf("produced %d lines, golden has %d", lineIndex, len(goldenLines))
	}
}
