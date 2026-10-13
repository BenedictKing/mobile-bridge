package acpview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/acpview/scenarios and byte-compares
// form validation, flattening helpers and full view snapshots with the
// Python oracle golden.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "acpview", "scenarios")
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
			op := step.MustGet("op").(string)
			var out any
			switch op {
			case "form_supported":
				schema, _ := step.MustGet("schema").(*jsoncompat.Object)
				out = FormSupported(schema)
			case "validate_form":
				schema, _ := step.MustGet("schema").(*jsoncompat.Object)
				result, err := ValidateForm(schema, step.MustGet("content"))
				if err != nil {
					out = errorEnvelope("ValueError: " + err.Error())
				} else {
					out = result
				}
			case "output":
				out = FlattenOutput(step.MustGet("value"))
			case "command_summary":
				out = CommandSummary(step.MustGet("value"))
			case "subagent_meta":
				title := ""
				if value, ok := step.Get("title"); ok {
					title, _ = value.(string)
				}
				out = SubagentMeta(step.MustGet("input"), title)
			case "view":
				out = runView(t, step)
			default:
				t.Fatalf("unknown op %q", op)
			}
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", op)
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

// runView mirrors the oracle's scripted AcpSessionView driver.
func runView(t *testing.T, step *jsoncompat.Object) any {
	t.Helper()
	nowNumber, _ := step.MustGet("now").(json.Number)
	now, _ := strconv.ParseFloat(nowNumber.String(), 64)
	view := NewView("sess-acp", nil, "/工作目录", func() float64 { return now })
	snapshots := &jsoncompat.Array{}
	for _, actionValue := range step.MustGet("steps").(*jsoncompat.Array).Items {
		action := actionValue.(*jsoncompat.Object)
		for _, key := range action.Keys() {
			value, _ := action.Get(key)
			switch key {
			case "snapshot":
				snapshots.Items = append(snapshots.Items, view.Snapshot())
			case "begin_turn":
				view.BeginTurn(value.(string))
			case "update":
				view.OnUpdate(value.(*jsoncompat.Object))
			case "end_turn":
				view.EndTurn(value.(string))
			case "fail_turn":
				view.FailTurn(value)
			case "finish_replay":
				view.FinishReplay()
			case "add_permission":
				entry := value.(*jsoncompat.Object)
				view.AddPermission(stringOf(entry, "id"), entry.MustGet("params").(*jsoncompat.Object))
			case "add_input":
				entry := value.(*jsoncompat.Object)
				view.AddInput(stringOf(entry, "id"), entry.MustGet("params").(*jsoncompat.Object))
			case "remove_permission":
				view.RemovePermission(value.(string))
			case "apply_config":
				config := value.(*jsoncompat.Object)
				options, _ := config.MustGet("options").(*jsoncompat.Array)
				modes, _ := config.MustGet("modes").(*jsoncompat.Object)
				view.ApplyConfig(options, modes)
			case "set_mode":
				view.SetMode(value)
			default:
				t.Fatalf("unknown view action %q", key)
			}
		}
	}
	return snapshots
}

func stringOf(object *jsoncompat.Object, key string) string {
	value, _ := object.MustGet(key).(string)
	return value
}

func errorEnvelope(message string) *jsoncompat.Object {
	envelope := jsoncompat.NewObject()
	envelope.Set("$error", message)
	return envelope
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
