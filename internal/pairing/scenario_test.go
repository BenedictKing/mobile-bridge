package pairing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/pairing/scenarios and byte-compares
// grant lifecycle outcomes with the Python oracle.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "pairing", "scenarios")
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
		instance := New([]string{"https://example.com", "http://192.168.1.5:8787"})
		for index, stepValue := range root.MustGet("steps").(*jsoncompat.Array).Items {
			step := stepValue.(*jsoncompat.Object)
			op := step.MustGet("op").(string)
			now := floatField(t, step, "now")
			var out any
			switch op {
			case "phone_origin":
				origin, err := PhoneOrigin(stringOf(step, "value"))
				out = resultOr(origin, err)
			case "create":
				value := jsoncompat.NewObject()
				value.Set("action", "create")
				value.Set("url", stringOf(step, "url"))
				out = resultOr(controlOf(t, instance, value, step, now))
			case "status_single":
				value := jsoncompat.NewObject()
				value.Set("action", "status")
				value.Set("id", stringOf(step, "id"))
				out = resultOr(controlOf(t, instance, value, step, now))
			case "status_list":
				value := jsoncompat.NewObject()
				value.Set("action", "status")
				value.Set("ids", step.MustGet("ids"))
				out = resultOr(controlOf(t, instance, value, step, now))
			case "revoke":
				value := jsoncompat.NewObject()
				value.Set("action", "revoke")
				value.Set("id", stringOf(step, "id"))
				out = resultOr(controlOf(t, instance, value, step, now))
			case "bogus":
				value := jsoncompat.NewObject()
				value.Set("action", "nope")
				out = resultOr(controlOf(t, instance, value, step, now))
			case "exchange":
				result, err := instance.Exchange(stringOf(step, "token"), stringOf(step, "origin"),
					stringOf(step, "address"), now,
					func(address string) any {
						session := jsoncompat.NewObject()
						session.Set("csrf", "<CSRF>")
						session.Set("label", stringOf(step, "session"))
						return &jsoncompat.Array{Items: []any{"<token>", session}}
					})
				if err != nil {
					out = errorEnvelope(err.Error())
				} else {
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

func controlOf(t *testing.T, instance *Pairing, value *jsoncompat.Object,
	step *jsoncompat.Object, now float64) (*jsoncompat.Object, error) {
	t.Helper()
	return instance.Control(value, now,
		func() string { return stringOf(step, "token") },
		func() string { return stringOf(step, "id") })
}

func resultOr(value any, err error) any {
	if err != nil {
		return errorEnvelope(err.Error())
	}
	if text, isString := value.(string); isString {
		return text
	}
	return value
}

func errorEnvelope(message string) *jsoncompat.Object {
	envelope := jsoncompat.NewObject()
	envelope.Set("$error", message)
	return envelope
}

func stringOf(step *jsoncompat.Object, key string) string {
	value, _ := step.MustGet(key).(string)
	return value
}

func floatField(t *testing.T, step *jsoncompat.Object, key string) float64 {
	t.Helper()
	number, ok := step.MustGet(key).(json.Number)
	if !ok {
		return 0
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		t.Fatalf("field %q=%s: %v", key, number.String(), err)
	}
	return parsed
}
