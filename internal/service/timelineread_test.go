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

// TestTimelineReadParity replays conformance/timelineread/scenarios against
// the annotated Bridge read core (stub-adapter empty behaviors) and
// byte-compares with the Python oracle golden.
func TestTimelineReadParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "timelineread", "scenarios")
	golden, err := os.ReadFile(filepath.Join(scenarioDir, "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	const threadID = "sess-read"
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
			records := runBridgeRead(t, threadID, step.MustGet("steps").(*jsoncompat.Array))
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", "bridge_session")
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

// runBridgeRead mirrors conformance/timelineread/oracle.py's driver.
func runBridgeRead(t *testing.T, threadID string, steps *jsoncompat.Array) *jsoncompat.Array {
	t.Helper()
	counter := 0
	nextEpoch := func() string {
		value := fmt.Sprintf("%032x", counter)
		counter++
		return value
	}
	bridge := NewBridge("local", "stub", []string{"resume", "chat"})
	// One epoch is consumed by the session's Timeline construction, exactly
	// like Python's LiveSession.__init__.
	session := &LiveSession{ID: threadID, Connected: true,
		Timeline: timeline.New(nextEpoch)}
	bridge.Live[threadID] = session
	var pages []*jsoncompat.Object
	records := &jsoncompat.Array{}
	emit := func(op string, out any) {
		record := jsoncompat.NewObject()
		record.Set("op", op)
		record.Set("out", out)
		records.Items = append(records.Items, record)
	}

	for _, actionValue := range steps.Items {
		action := actionValue.(*jsoncompat.Object)
		for _, key := range action.Keys() {
			value, _ := action.Get(key)
			spec, _ := value.(*jsoncompat.Object)
			switch key {
			case "set_history":
				session.SetHistory(spec)
			case "state":
				session.State = spec
				if connected, ok := action.Get("connected"); ok {
					session.Connected, _ = connected.(bool)
				}
			case "connected":
				session.Connected, _ = value.(bool)
			case "changed":
				session.Changed()
			case "read_page":
				options := ReadOptions{Limit: 20}
				if raw, ok := spec.Get("limit"); ok && raw != nil {
					fmt.Sscanf(raw.(json.Number).String(), "%d", &options.Limit)
				}
				if flag, _ := spec.MustGet("before_last").(bool); flag && len(pages) > 0 {
					options.Before, _ = pages[len(pages)-1].MustGet("before").(string)
				}
				result, err := bridge.TimelineRead("page", session, options)
				if err != nil {
					t.Fatalf("read_page: %v", err)
				}
				pages = append(pages, result)
				emit("read_page", result)
			case "read_changes":
				options := ReadOptions{After: spec.MustGet("after").(json.Number).String(), Start: ""}
				if flag, _ := spec.MustGet("epoch_last").(bool); flag && len(pages) > 0 {
					options.Epoch, _ = pages[len(pages)-1].MustGet("epoch").(string)
				}
				result, err := bridge.TimelineRead("changes", session, options)
				if err != nil {
					t.Fatalf("read_changes: %v", err)
				}
				emit("read_changes", result)
			case "read_detail":
				lastPage := pages[len(pages)-1]
				row := lastPage.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
				epoch, _ := lastPage.MustGet("epoch").(string)
				rowKey, _ := row.MustGet("key").(string)
				options := ReadOptions{Cursor: epoch + "." + rowKey}
				if raw, ok := spec.Get("offset"); ok && raw != nil {
					fmt.Sscanf(raw.(json.Number).String(), "%d", &options.Offset)
				}
				if raw, ok := spec.Get("version"); ok && raw != nil {
					version, _ := raw.(string)
					options.Version = &version
				}
				result, err := bridge.TimelineRead("detail", session, options)
				if err != nil {
					t.Fatalf("read_detail: %v", err)
				}
				emit("read_detail", result)
			default:
				t.Fatalf("unknown bridge action %q", key)
			}
		}
	}
	return records
}
