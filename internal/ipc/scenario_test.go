package ipc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/ipc/scenarios and byte-compares each
// outgoing frame (hex) with the Python oracle golden. It also round-trips
// every golden frame through DecodeFrame/EncodeFrame.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "ipc", "scenarios")
	golden, err := os.ReadFile(filepath.Join(scenarioDir, "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	counter := 0
	newID := func() string {
		text := fmtSprintf32x(counter)
		counter++
		return text
	}
	client := &Client{ClientID: "client-abc", NewID: newID}
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
			out := jsoncompat.NewObject()
			out.Set("frame", nil)
			out.Set("event", nil)
			var frame []byte
			var event any
			switch op {
			case "request":
				params := step.MustGet("params").(*jsoncompat.Object)
				message, err := client.BuildRequest(
					step.MustGet("method").(string), params,
					stringField(step, "target"), stringField(step, "host"),
					floatField(t, step, "timeout", 15))
				if err != nil {
					out.Set("error", err.Error())
				} else {
					frame, err = EncodeFrame(message)
					if err != nil {
						t.Fatalf("encode: %v", err)
					}
				}
			case "follow":
				owner := stringField(step, "owner")
				message := client.BuildFollow(step.MustGet("session").(string),
					owner, boolField(step, "enabled", true), stringField(step, "host"))
				frame, err = EncodeFrame(message)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
			case "incoming":
				message := step.MustGet("message").(*jsoncompat.Object)
				DispatchIncoming(message, client.ClientID,
					func(response *jsoncompat.Object) {
						frame, err = EncodeFrame(response)
						if err != nil {
							t.Fatalf("encode: %v", err)
						}
					},
					func(delivered *jsoncompat.Object) { event = delivered })
			default:
				t.Fatalf("unknown op %q", op)
			}
			if frame != nil {
				out.Set("frame", hex.EncodeToString(frame))
			}
			if event != nil {
				out.Set("event", event)
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
	verifyRoundTrip(t, goldenLines)
}

// verifyRoundTrip decodes every captured frame and re-encodes it; the bytes
// must survive unchanged.
func verifyRoundTrip(t *testing.T, goldenLines []string) {
	t.Helper()
	for _, line := range goldenLines {
		value, err := jsoncompat.Unmarshal([]byte(line))
		if err != nil {
			t.Fatalf("parse golden line: %v", err)
		}
		frameHex, _ := value.(*jsoncompat.Object).MustGet("out").(*jsoncompat.Object).MustGet("frame").(string)
		if frameHex == "" {
			continue
		}
		original, err := hex.DecodeString(frameHex)
		if err != nil {
			t.Fatalf("decode hex: %v", err)
		}
		decoded, err := DecodeFrame(bytes.NewReader(original))
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		reencoded, err := EncodeFrame(decoded)
		if err != nil {
			t.Fatalf("re-encode frame: %v", err)
		}
		if !bytes.Equal(original, reencoded) {
			t.Errorf("frame round-trip changed bytes:\n orig: %x\n  new: %x", original, reencoded)
		}
	}
}

func fmtSprintf32x(n int) string {
	text := make([]byte, 32)
	hexDigits := []byte("0123456789abcdef")
	for i := 31; i >= 0; i-- {
		text[i] = hexDigits[n&0xf]
		n >>= 4
	}
	return string(text[:8]) + "-" + string(text[8:12]) + "-" + string(text[12:16]) + "-" +
		string(text[16:20]) + "-" + string(text[20:])
}

func stringField(step *jsoncompat.Object, key string) string {
	value, _ := step.MustGet(key).(string)
	return value
}

func boolField(step *jsoncompat.Object, key string, fallback bool) bool {
	value, ok := step.MustGet(key).(bool)
	if !ok {
		return fallback
	}
	return value
}

func floatField(t *testing.T, step *jsoncompat.Object, key string, fallback float64) float64 {
	t.Helper()
	value, ok := step.Get(key)
	if !ok || value == nil {
		return fallback
	}
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("step field %q is %T, want number", key, value)
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		t.Fatalf("step field %q=%s: %v", key, number.String(), err)
	}
	return parsed
}

var _ = json.Marshal // keep encoding/json imported for future helpers
