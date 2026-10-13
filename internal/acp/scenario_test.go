package acp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestPythonParity drives this transport against conformance/acp/agent.py
// (the same fake agent the Python oracle uses) and byte-compares each op
// outcome plus the full wire log with the Python golden.
func TestPythonParity(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("..", "..", "conformance", "acp", "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")

	agentPath, _ := filepath.Abs(filepath.Join("..", "..", "conformance", "acp", "agent.py"))
	wirePath := filepath.Join(t.TempDir(), "wire.log")
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var notifications []*jsoncompat.Object
	process, err := Start([]string{python, agentPath, wirePath})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	process.OnNotification = func(message *jsoncompat.Object) {
		mu.Lock()
		notifications = append(notifications, message)
		mu.Unlock()
	}
	process.OnRequest = func(message *jsoncompat.Object, respond func(any)) {
		answer := jsoncompat.NewObject()
		answer.Set("answer", "来自网关")
		respond(answer)
	}

	echoParams := jsoncompat.NewObject()
	echoParams.Set("文本", "你好 🚀")
	echoParams.Set("n", &jsoncompat.Array{Items: []any{jsoncompat.Num(1), json.Number("2.5")}})
	pokeParams := jsoncompat.NewObject()
	pokeParams.Set("x", jsoncompat.Num(1))
	empty := jsoncompat.NewObject()

	type op struct {
		name   string
		method string
		params *jsoncompat.Object
	}
	ops := []op{
		{"initialize", "initialize", nil},
		{"echo", "echo", echoParams},
		{"fail-auth", "fail", empty},
		{"fail-generic", "fail_generic", empty},
		{"null-result", "null", empty},
		{"banner", "banner", empty},
		{"orphan", "orphan", empty},
		{"poke", "poke", pokeParams},
		{"reverse", "reverse", empty},
	}

	var lines []string
	for index, operation := range ops {
		var result any
		var callErr error
		if operation.name == "initialize" {
			result, callErr = process.Initialize(10 * time.Second)
		} else {
			result, callErr = process.Request(operation.method, operation.params, 10*time.Second)
		}
		if operation.name == "poke" {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				count := len(notifications)
				mu.Unlock()
				if count >= 1 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		record := jsoncompat.NewObject()
		record.Set("i", jsoncompat.Num(index))
		if callErr != nil {
			failure := callErr.(*Error)
			envelope := jsoncompat.NewObject()
			envelope.Set("type", string(failure.Kind))
			envelope.Set("message", failure.Message)
			envelope.Set("code", failure.Code)
			envelope.Set("data", failure.Data)
			errorWrap := jsoncompat.NewObject()
			errorWrap.Set("error", envelope)
			record.Set("out", errorWrap)
		} else {
			okWrap := jsoncompat.NewObject()
			okWrap.Set("ok", result)
			record.Set("out", okWrap)
		}
		if index == 0 {
			meta := jsoncompat.NewObject()
			meta.Set("capabilities", process.Capabilities)
			meta.Set("agentInfo", process.AgentInfo)
			meta.Set("authMethods", process.AuthMethods)
			record.MustGet("out").(*jsoncompat.Object).Set("meta", meta)
		} else {
			record.MustGet("out").(*jsoncompat.Object).Set("meta", nil)
		}
		if index == 7 {
			list := &jsoncompat.Array{}
			mu.Lock()
			for _, notification := range notifications {
				list.Items = append(list.Items, notification)
			}
			mu.Unlock()
			record.MustGet("out").(*jsoncompat.Object).Set("notifications", list)
		}
		lines = append(lines, string(jsoncompat.EncodeCompact(record)))
	}
	process.Close()

	wire, err := os.ReadFile(wirePath)
	if err != nil {
		t.Fatalf("wire log: %v", err)
	}
	wireRecord := jsoncompat.NewObject()
	wireRecord.Set("i", "wire")
	wireWrap := jsoncompat.NewObject()
	wireWrap.Set("log", string(wire))
	wireRecord.Set("out", wireWrap)
	lines = append(lines, string(jsoncompat.EncodeCompact(wireRecord)))

	if len(lines) != len(goldenLines) {
		t.Fatalf("produced %d lines, golden has %d", len(lines), len(goldenLines))
	}
	for i, line := range lines {
		if line != goldenLines[i] {
			offset := firstDifference(goldenLines[i], line)
			start := max(0, offset-60)
			t.Errorf("parity mismatch line %d at byte %d:\n want: …%s…\n  got: …%s…",
				i, offset, goldenLines[i][start:min(len(goldenLines[i]), offset+120)],
				line[start:min(len(line), offset+120)])
		}
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
