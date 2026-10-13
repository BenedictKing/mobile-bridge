package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/model/scenarios and byte-compares the
// normalized views, patch results and helper outputs with the Python oracle.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "model", "scenarios")
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
			case "normalize_state":
				connected := true
				if value, ok := step.Get("connected"); ok {
					connected, _ = value.(bool)
				}
				out = NormalizeState(obj(step.MustGet("state")), connected)
			case "apply_patches":
				result, err := ApplyPatches(step.MustGet("state"),
					arr(step.MustGet("patches")))
				if err != nil {
					out = errorEnvelope(err.Error())
				} else {
					out = result
				}
			case "text_content":
				out = TextContent(step.MustGet("content"))
			case "user_display_text":
				out = UserDisplayText(stringField(step, "text"))
			case "question_replies":
				out = replyList(QuestionReplies(stringField(step, "text")))
			case "form_supported":
				out = FormSupported(obj(step.MustGet("schema")))
			case "normalize_request":
				out = NormalizeRequest(obj(step.MustGet("request")))
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
				start := offset - 80
				if start < 0 {
					start = 0
				}
				t.Errorf("parity mismatch (%s line %d) at byte %d:\n want: …%s…\n  got: …%s…",
					entry.Name(), lineIndex, offset,
					goldenLines[lineIndex][start:min(len(goldenLines[lineIndex]), offset+80)],
					produced[start:min(len(produced), offset+80)])
			}
			lineIndex++
		}
	}
	if lineIndex != len(goldenLines) {
		t.Errorf("produced %d lines, golden has %d", lineIndex, len(goldenLines))
	}
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

func errorEnvelope(message string) *jsoncompat.Object {
	envelope := jsoncompat.NewObject()
	envelope.Set("$error", message)
	return envelope
}

// replyList mirrors Python: question_replies always returns a list ([] when
// nothing matched, never null).
func replyList(replies []*jsoncompat.Object) any {
	list := &jsoncompat.Array{}
	for _, reply := range replies {
		list.Items = append(list.Items, reply)
	}
	return list
}

func stringField(step *jsoncompat.Object, key string) string {
	value, _ := step.MustGet(key).(string)
	return value
}
