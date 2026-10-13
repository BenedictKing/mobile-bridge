package lifecycle

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/lifecycle/scenarios and byte-compares
// control-file payloads and validation outcomes with the Python oracle.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "lifecycle", "scenarios")
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
			case "control_record":
				pid := intOf(t, step, "pid")
				record := ControlRecord(pid, stringOf(step, "token_hex"), stringOf(step, "pairing_dir"),
					step.MustGet("instance_id"), boolOf(step, "device"), boolOf(step, "accounts"),
					boolOf(step, "account"), boolOf(step, "notifications"))
				out = hexEnvelope(DumpASCII(record))
			case "dump_ascii":
				out = hexEnvelope(DumpASCII(step.MustGet("value")))
			case "stop_validate":
				result := jsoncompat.NewObject()
				if err := StopRequestValid(step.MustGet("record")); err != nil {
					result.Set("valid", false)
					result.Set("error", err.Error())
				} else {
					result.Set("valid", true)
				}
				out = result
			case "read_record":
				out = ParseRecord(stringOf(step, "text"))
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
				t.Errorf("parity mismatch (%s line %d):\n want: %s\n  got: %s",
					entry.Name(), lineIndex, goldenLines[lineIndex], produced)
			}
			lineIndex++
		}
	}
	if lineIndex != len(goldenLines) {
		t.Errorf("produced %d lines, golden has %d", lineIndex, len(goldenLines))
	}
}

func hexEnvelope(payload []byte) *jsoncompat.Object {
	envelope := jsoncompat.NewObject()
	envelope.Set("bytes_hex", hex.EncodeToString(payload))
	return envelope
}

func stringOf(step *jsoncompat.Object, key string) string {
	value, _ := step.MustGet(key).(string)
	return value
}

func boolOf(step *jsoncompat.Object, key string) bool {
	value, _ := step.MustGet(key).(bool)
	return value
}

func intOf(t *testing.T, step *jsoncompat.Object, key string) int {
	t.Helper()
	number, ok := step.MustGet(key).(json.Number)
	if !ok {
		t.Fatalf("field %q is not a number", key)
	}
	parsed, err := strconv.Atoi(number.String())
	if err != nil {
		t.Fatalf("field %q=%s: %v", key, number.String(), err)
	}
	return parsed
}
