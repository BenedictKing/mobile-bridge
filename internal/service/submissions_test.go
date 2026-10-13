package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// stubSubmissionAdapter mirrors conformance/submissions/oracle.py's
// StubAdapter dispatch scripting.
type stubSubmissionAdapter struct {
	active       bool
	nextDispatch map[string]any
}

func (a *stubSubmissionAdapter) Label() string { return "Stub Agent" }
func (a *stubSubmissionAdapter) HasCapability(name string) bool {
	return name == "resume" || name == "chat" || name == "steer"
}
func (a *stubSubmissionAdapter) IsActive(session *LiveSession) bool { return a.active }
func (a *stubSubmissionAdapter) DispatchTurn(session *LiveSession, submissionID string,
	entry *jsoncompat.Object) (any, error) {
	if message, ok := a.nextDispatch["error"].(string); ok && message != "" {
		return nil, valueError(message)
	}
	if message, ok := a.nextDispatch["not_dispatched"].(string); ok && message != "" {
		return nil, notDispatched(message)
	}
	if timeout, ok := a.nextDispatch["timeout"].(bool); ok && timeout {
		return nil, acpTimeout("Agent 请求超时：session/prompt")
	}
	accepted := jsoncompat.NewObject()
	accepted.Set("accepted", true)
	return accepted, nil
}

// TestSubmissionsParity replays conformance/submissions/scenarios and
// byte-compares the ledger state machine with the Python oracle golden.
func TestSubmissionsParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "submissions", "scenarios")
	golden, err := os.ReadFile(filepath.Join(scenarioDir, "golden.jsonl"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	goldenLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	const threadID = "sess-sub"
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
			records := runSubmissions(t, threadID, step.MustGet("steps").(*jsoncompat.Array))
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", "submissions")
			out := jsoncompat.NewObject()
			out.Set("records", records)
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

func runSubmissions(t *testing.T, threadID string, steps *jsoncompat.Array) *jsoncompat.Array {
	t.Helper()
	adapter := &stubSubmissionAdapter{nextDispatch: map[string]any{"ok": true}}
	bridge := NewBridge("local", "stub", []string{"resume", "chat"})
	bridge.Adapter = adapter
	session := &LiveSession{ID: threadID, Connected: true}
	state := jsoncompat.NewObject()
	state.Set("id", threadID)
	state.Set("latestModel", "stub-model")
	state.Set("turns", &jsoncompat.Array{})
	state.Set("requests", &jsoncompat.Array{})
	session.State = state
	bridge.Live[threadID] = session

	records := &jsoncompat.Array{}
	attempt := func(operation func() (any, error)) any {
		result, err := operation()
		if err != nil {
			envelope := jsoncompat.NewObject()
			if kinded, ok := err.(*KindedError); ok {
				envelope.Set("$error", kinded.Kind+": "+kinded.Message)
			} else {
				envelope.Set("$error", err.Error())
			}
			return envelope
		}
		if result == nil {
			return nil
		}
		return result
	}
	emit := func(op string, out any) {
		record := jsoncompat.NewObject()
		record.Set("op", op)
		record.Set("out", out)
		records.Items = append(records.Items, record)
	}

	for _, actionValue := range steps.Items {
		action := actionValue.(*jsoncompat.Object)
		if value, ok := action.Get("set_active"); ok {
			adapter.active, _ = value.(bool)
			continue
		}
		if value, ok := action.Get("dispatch"); ok {
			adapter.nextDispatch = map[string]any{}
			if script := objectOf(value); script != nil {
				for _, key := range script.Keys() {
					entry, _ := script.Get(key)
					adapter.nextDispatch[key] = entry
				}
			}
			continue
		}
		name, _ := action.MustGet("op").(string)
		value, _ := action.Get("value")
		spec := objectOf(value)
		switch name {
		case "send":
			spec := objectOf(jsoncompat.Clone(spec))
			text, _ := spec.MustGet("text").(string)
			submissionID, _ := spec.MustGet("submission_id").(string)
			mode := "send"
			if raw, ok := spec.Get("mode"); ok && raw != nil {
				mode, _ = raw.(string)
			}
			var skills []any
			if list, ok := spec.MustGet("skills").(*jsoncompat.Array); ok {
				skills = list.Items
			}
			var workMode any
			if raw, ok := spec.Get("work_mode"); ok {
				workMode = raw
			}
			emit(name, attempt(func() (any, error) {
				return bridge.Send(session, text, submissionID, mode, skills, workMode)
			}))
		case "meta":
			emit(name, bridge.SubmissionMeta(session))
		case "mark_failed":
			submissionID, _ := spec.MustGet("id").(string)
			message, _ := spec.MustGet("error").(string)
			emit(name, attempt(func() (any, error) {
				bridge.MarkSubmissionFailed(threadID, submissionID, message)
				return nil, nil
			}))
		case "queue_action":
			submissionID, _ := spec.MustGet("id").(string)
			actionName, _ := spec.MustGet("action").(string)
			var text any
			if raw, ok := spec.Get("text"); ok {
				text = raw
			}
			emit(name, attempt(func() (any, error) {
				return bridge.QueueAction(session, submissionID, actionName, text)
			}))
		case "ignore_submission":
			submissionID, _ := spec.MustGet("id").(string)
			emit(name, attempt(func() (any, error) {
				return bridge.IgnoreSubmission(session, submissionID)
			}))
		default:
			t.Fatalf("unknown submissions action %q", name)
		}
	}
	return records
}

var _ = json.Marshal
