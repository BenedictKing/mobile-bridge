package notifications

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/notifications/scenarios and
// byte-compares URL validation, settings persistence (token preservation)
// and channel derivation with the Python oracle golden.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "notifications", "scenarios")
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
		dataDir := filepath.Join(t.TempDir(), strings.ReplaceAll(entry.Name(), ".json", ""))
		var saved *jsoncompat.Object
		for index, stepValue := range root.MustGet("steps").(*jsoncompat.Array).Items {
			step := stepValue.(*jsoncompat.Object)
			op := step.MustGet("op").(string)
			var out any
			switch op {
			case "valid_url":
				value, _ := step.MustGet("value").(string)
				allowPath := false
				if flag, ok := step.MustGet("allow_path").(bool); ok {
					allowPath = flag
				}
				if result, err := ValidURL(value, allowPath); err != nil {
					out = Describe(err)
				} else {
					out = result
				}
			case "settings_read":
				out = ReadSettings(dataDir)
			case "settings_save":
				result, err := SaveSettings(dataDir, step.MustGet("value").(*jsoncompat.Object))
				if err != nil {
					out = Describe(err)
				} else {
					saved = result
					out = result
				}
			case "destination":
				channel, _ := step.MustGet("channel").(string)
				config := saved
				if config == nil {
					config = jsoncompat.NewObject()
				}
				if result, err := Destination(config, channel); err != nil {
					out = Describe(err)
				} else {
					out = result
				}
			case "channels":
				config := saved
				if raw, ok := step.Get("config"); ok && raw != nil {
					config = raw.(*jsoncompat.Object)
				}
				if config == nil {
					config = jsoncompat.NewObject()
				}
				out = Channels(config)
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
