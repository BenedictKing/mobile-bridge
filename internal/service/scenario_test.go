package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/service/scenarios and byte-compares
// LiveSession view outputs (saved-prefix merge + status fields) with the
// Python oracle golden.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "service", "scenarios")
	golden, err := os.ReadFile(filepath.Join(scenarioDir, "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
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
			views := runSession(t, step.MustGet("steps").(*jsoncompat.Array))
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", "session")
			out := jsoncompat.NewObject()
			out.Set("views", views)
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

// runSession mirrors the oracle's scripted LiveSession driver.
func runSession(t *testing.T, steps *jsoncompat.Array) *jsoncompat.Array {
	t.Helper()
	session := &LiveSession{ID: "chat"}
	views := &jsoncompat.Array{}
	for _, actionValue := range steps.Items {
		action := actionValue.(*jsoncompat.Object)
		for _, key := range action.Keys() {
			value, _ := action.Get(key)
			switch key {
			case "set_history":
				session.SetHistory(value.(*jsoncompat.Object))
			case "state":
				session.State = value
				if connected, ok := action.Get("connected"); ok {
					session.Connected, _ = connected.(bool)
				}
			case "connected":
				session.Connected, _ = value.(bool)
			case "error":
				session.Error = value
			case "connecting":
				session.Connecting, _ = value.(bool)
			case "activating":
				session.Activating, _ = value.(bool)
			case "changed":
				session.Changed()
			case "set":
				if mode, _ := value.(string); mode == "pagination_complete" {
					if state := objectOf(session.State); state != nil {
						pagination := objectOf(state.MustGet("turnsPagination"))
						if pagination == nil {
							pagination = jsoncompat.NewObject()
							state.Set("turnsPagination", pagination)
						}
						pagination.Set("hasLoadedOldest", true)
					}
				}
			case "view":
				views.Items = append(views.Items, session.View())
			default:
				t.Fatalf("unknown session action %q", key)
			}
		}
	}
	return views
}

func firstDifference(want, got string) int {
	limit := min(len(want), len(got))
	for i := 0; i < limit; i++ {
		if want[i] != got[i] {
			return i
		}
	}
	return limit
}

var _ = json.Marshal
