package acpadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

var magicBytes = map[string][]byte{
	"png":     append([]byte("\x89PNG\r\n\x1a\n"), []byte("png-bytes")...),
	"jpeg":    append([]byte("\xff\xd8\xff"), []byte("jpeg-bytes")...),
	"gif":     append([]byte("GIF89a"), []byte("gif-bytes")...),
	"webp":    append(append([]byte("RIFF"), []byte("----")...), append([]byte("WEBP"), []byte("webp-bytes")...)...),
	"unknown": {0x00, 0x01, 'p', 'l', 'a', 'i', 'n', '-', 'b', 'i', 'n', 'a', 'r', 'y'},
	"text":    []byte("文本内容 📎\n"),
}

// TestPythonParity replays conformance/acpadapter/scenarios and byte-compares
// attachment blocks and mode switching with the Python oracle. Both sides
// mask their own temp workspace to <WS>.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "acpadapter", "scenarios")
	golden, err := os.ReadFile(filepath.Join(scenarioDir, "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	workspace := t.TempDir()
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		resolved = workspace
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
			case "attachment_blocks":
				capabilities, _ := step.MustGet("capabilities").(*jsoncompat.Object)
				var files []FileSpec
				for _, itemValue := range step.MustGet("files").(*jsoncompat.Array).Items {
					item := itemValue.(*jsoncompat.Object)
					fileKind, _ := item.MustGet("kind").(string)
					fileName, _ := item.MustGet("name").(string)
					image, _ := item.MustGet("image").(bool)
					path := filepath.Join(workspace, fileName)
					if err := os.WriteFile(path, magicBytes[fileKind], 0o600); err != nil {
						t.Fatalf("write fixture: %v", err)
					}
					files = append(files, FileSpec{Image: image, Name: fileName, Path: path})
				}
				blocks, err := AttachmentBlocks(capabilities, files)
				if err != nil {
					out = errorEnvelope(err.Error())
				} else {
					out = envelope("blocks", blocks)
				}
			case "set_mode":
				modes, _ := jsoncompat.Clone(step.MustGet("modes")).(*jsoncompat.Object)
				requests := &jsoncompat.Array{}
				view := acpview.NewView("sess-1", nil, nil, func() float64 { return 0 })
				modeID, _ := step.MustGet("set").(string)
				if err := SetMode("sess-1", modeID, modes,
					func(params *jsoncompat.Object) {
						entry := jsoncompat.NewObject()
						entry.Set("method", "session/set_mode")
						entry.Set("params", params)
						requests.Items = append(requests.Items, entry)
					}, view); err != nil {
					out = errorEnvelope(err.Error())
				} else {
					current := any(nil)
					if modes != nil {
						current = modes.MustGet("currentModeId")
					}
					result := jsoncompat.NewObject()
					result.Set("requests", requests)
					result.Set("currentModeId", current)
					result.Set("viewMode", view.Snapshot().MustGet("collaborationMode"))
					out = result
				}
			default:
				t.Fatalf("unknown op %q", op)
			}
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", op)
			record.Set("out", out)
			produced := strings.ReplaceAll(string(jsoncompat.EncodeCompact(record)),
				resolved, "<WS>")
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

func envelope(key string, value any) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set(key, value)
	return result
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

var _ = json.Marshal
