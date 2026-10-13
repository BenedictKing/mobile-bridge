package timeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity replays conformance/timeline/scenarios against this port
// and byte-compares every step result with the Python oracle's golden.jsonl.
// Regenerate the golden after changing the projection contract:
//
//	python3 conformance/timeline/oracle.py --goldens
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "timeline", "scenarios")
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
		for _, produced := range runScenario(t, scenario.(*jsoncompat.Object)) {
			if lineIndex >= len(goldenLines) {
				t.Fatalf("%s step %d: golden ran out of lines", entry.Name(), lineIndex)
			}
			want, got := goldenLines[lineIndex], string(produced)
			if want != got {
				t.Errorf("parity mismatch (%s line %d):\n want: %s\n  got: %s",
					entry.Name(), lineIndex, excerpt(want), excerpt(got))
			}
			lineIndex++
		}
	}
	if lineIndex != len(goldenLines) {
		t.Errorf("produced %d lines, golden has %d", lineIndex, len(goldenLines))
	}
}

// runScenario mirrors conformance/timeline/oracle.py exactly, including the
// deterministic epoch counter injected in place of uuid4.
func runScenario(t *testing.T, scenario *jsoncompat.Object) [][]byte {
	t.Helper()
	counter := 0
	instance := New(func() string {
		value := fmt.Sprintf("%032x", counter)
		counter++
		return value
	})
	steps := scenario.MustGet("steps").(*jsoncompat.Array)
	name := scenario.MustGet("name")
	results := []any{}
	produced := [][]byte{}
	for index, stepValue := range steps.Items {
		step := stepValue.(*jsoncompat.Object)
		op := step.MustGet("op").(string)
		var out any
		switch op {
		case "update":
			instance.Update(step.MustGet("view").(*jsoncompat.Object))
			ok := jsoncompat.NewObject()
			ok.Set("ok", true)
			out = ok
		case "page":
			before := ""
			if token, has := step.Get("before"); has {
				if resolved, ok := resolve(results, token).(string); ok {
					before = resolved
				}
			}
			out = instance.Page(intNumber(t, step, "limit", 20), before)
		case "changes":
			after := resolve(results, step.MustGet("after")).(json.Number).String()
			epoch := resolve(results, step.MustGet("epoch")).(string)
			start := ""
			if token, has := step.Get("start"); has {
				if resolved, ok := resolve(results, token).(string); ok {
					start = resolved
				}
			}
			out = instance.Changes(after, epoch, start)
		case "detail":
			cursor := resolve(results, step.MustGet("cursor")).(string)
			var version any
			if token, has := step.Get("version"); has && jsoncompat.Truthy(token) {
				version = resolve(results, token)
			}
			result, err := instance.Detail(cursor, intNumber(t, step, "offset", 0), version)
			if err != nil {
				envelope := jsoncompat.NewObject()
				envelope.Set("$error", err.Error())
				out = envelope
			} else {
				out = result
			}
		case "cursor":
			out = resolve(results, step.MustGet("epoch")).(string) + "." +
				resolve(results, step.MustGet("key")).(string)
		case "state":
			state := instance.State()
			envelope := jsoncompat.NewObject()
			envelope.Set("epoch", state.Epoch)
			envelope.Set("sequence", json.Number(state.Sequence))
			envelope.Set("versions", jsoncompat.Num(state.Versions))
			envelope.Set("rows", jsoncompat.Num(state.Rows))
			out = envelope
		default:
			t.Fatalf("unknown op %q", op)
		}
		results = append(results, out)
		record := jsoncompat.NewObject()
		record.Set("scenario", name)
		record.Set("i", jsoncompat.Num(index))
		record.Set("op", op)
		record.Set("out", out)
		produced = append(produced, jsoncompat.EncodeCompact(record))
	}
	return produced
}

func intNumber(t *testing.T, step *jsoncompat.Object, key string, fallback int) int {
	t.Helper()
	value, ok := step.Get(key)
	if !ok || value == nil {
		return fallback
	}
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("step field %q is %T, want number", key, value)
	}
	parsed, err := strconv.Atoi(number.String())
	if err != nil {
		t.Fatalf("step field %q=%s: %v", key, number.String(), err)
	}
	return parsed
}

func excerpt(line string) string {
	if len(line) <= 240 {
		return line
	}
	return line[:240] + "…"
}

func resolve(results []any, token any) any {
	if concat, ok := token.(*jsoncompat.Object); ok {
		if parts, has := concat.Get("$concat"); has {
			var builder strings.Builder
			for _, part := range parts.(*jsoncompat.Array).Items {
				builder.WriteString(jsoncompat.PyStr(resolve(results, part)))
			}
			return builder.String()
		}
	}
	if reference, ok := token.(string); ok && strings.HasPrefix(reference, "$") {
		segments := strings.Split(reference[1:], ".")
		index, _ := strconv.Atoi(segments[0])
		cursor := results[index]
		for _, segment := range segments[1:] {
			switch node := cursor.(type) {
			case *jsoncompat.Array:
				position, _ := strconv.Atoi(segment)
				cursor = node.Items[position]
			case *jsoncompat.Object:
				cursor = node.MustGet(segment)
			default:
				panic(fmt.Sprintf("cannot walk %T through %q", cursor, segment))
			}
		}
		return cursor
	}
	return token
}
