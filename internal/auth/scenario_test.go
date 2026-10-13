package auth

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

// TestPythonParity replays conformance/auth/scenarios and byte-compares each
// result with the Python oracle golden.
func TestPythonParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "auth", "scenarios")
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
			case "session_hours":
				hours, err := SessionHours(step.MustGet("value"))
				out = resultOr(hours, err)
			case "canonical_ip":
				ip, err := CanonicalIP(stringOf(step, "value"))
				out = resultOr(ip, err)
			case "access_policy":
				policy, err := AccessPolicy(objectOf(step, "value"))
				out = resultOr(policy, err)
			case "password_record":
				salt, err := hex.DecodeString(stringOf(step, "salt_hex"))
				if err != nil {
					t.Fatalf("bad salt: %v", err)
				}
				out = PasswordRecord(stringOf(step, "password"), salt)
			case "key":
				out = SessionKey(stringOf(step, "token"))
			case "valid":
				session := objectOf(step, "session")
				out = SessionValid(floatOf(t, session, "expires"), floatOf(t, step, "now"))
			case "cookie_age":
				age := CookieAge(objectOf(step, "session"), floatOf(t, step, "now"))
				if age == nil {
					out = nil
				} else {
					out = jsoncompat.Num(*age)
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

// resultOr wraps a (value, error) pair as either the value or the oracle's
// {"$error": message} envelope.
func resultOr(value any, err error) any {
	if err != nil {
		envelope := jsoncompat.NewObject()
		envelope.Set("$error", err.Error())
		return envelope
	}
	switch v := value.(type) {
	case int:
		return jsoncompat.Num(v)
	case string:
		return v
	default:
		return v
	}
}

func stringOf(step *jsoncompat.Object, key string) string {
	value, _ := step.MustGet(key).(string)
	return value
}

func objectOf(step *jsoncompat.Object, key string) *jsoncompat.Object {
	value, _ := step.MustGet(key).(*jsoncompat.Object)
	return value
}

func floatOf(t *testing.T, container *jsoncompat.Object, key string) float64 {
	t.Helper()
	number, ok := container.MustGet(key).(json.Number)
	if !ok {
		t.Fatalf("field %q is not a number", key)
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		t.Fatalf("field %q=%s: %v", key, number.String(), err)
	}
	return parsed
}
