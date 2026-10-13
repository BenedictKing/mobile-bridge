package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

// TestProjectionParity composes LiveSession.view() with the Timeline
// projection exactly like timeline_read's un-annotated core and byte-compares
// pages, changes and the update trigger against conformance/projection goldens.
func TestProjectionParity(t *testing.T) {
	scenarioDir := filepath.Join("..", "..", "conformance", "projection", "scenarios")
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
	counter := 0
	nextEpoch := func() string {
		value := fmt.Sprintf("%032x", counter)
		counter++
		return value
	}
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
			records := runProjection(t, nextEpoch, step.MustGet("steps").(*jsoncompat.Array))
			record := jsoncompat.NewObject()
			record.Set("scenario", name)
			record.Set("i", jsoncompat.Num(index))
			record.Set("op", "session")
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

// runProjection mirrors conformance/projection/oracle.py's driver.
func runProjection(t *testing.T, nextEpoch func() string,
	steps *jsoncompat.Array) *jsoncompat.Array {
	t.Helper()
	// Python's LiveSession.__init__ constructs its own Timeline, consuming
	// one epoch id; account for it so counters stay aligned.
	_ = nextEpoch()
	session := &LiveSession{ID: "chat"}
	projection := timeline.New(nextEpoch)
	var pages []*jsoncompat.Object
	records := &jsoncompat.Array{}

	emit := func(op string, out any) {
		record := jsoncompat.NewObject()
		record.Set("op", op)
		record.Set("out", out)
		records.Items = append(records.Items, record)
	}

	for _, actionValue := range steps.Items {
		action := actionValue.(*jsoncompat.Object)
		for _, key := range action.Keys() {
			value, _ := action.Get(key)
			switch key {
			case "set_history":
				session.SetHistory(value.(*jsoncompat.Object))
			case "state":
				session.State = value
				if connected, ok := action.Get("connected"); ok {
					session.Connected, _ = connected.(bool)
				}
			case "connected":
				session.Connected, _ = value.(bool)
			case "changed":
				session.Changed()
			case "project":
				view := session.View()
				meta := projection.Meta()
				stale := projection.State().Sequence == jsoncompat.Num(session.Sequence).String() &&
					valueEqual(meta.MustGet("goal"), view.MustGet("goal")) &&
					valueEqual(meta.MustGet("subagentRevision"), view.MustGet("subagentRevision"))
				if !stale {
					projection.Update(view)
				}
				out := jsoncompat.NewObject()
				out.Set("updated", !stale)
				out.Set("sequence", jsoncompat.Num(session.Sequence))
				emit("project", out)
			case "page":
				before := ""
				if flag, _ := value.(*jsoncompat.Object).MustGet("before_last").(bool); flag && len(pages) > 0 {
					before, _ = pages[len(pages)-1].MustGet("before").(string)
				}
				if flag, _ := value.(*jsoncompat.Object).MustGet("before_prev").(bool); flag && len(pages) >= 2 {
					before, _ = pages[len(pages)-2].MustGet("before").(string)
				}
				limit := 20
				if raw, ok := value.(*jsoncompat.Object).Get("limit"); ok && raw != nil {
					number := raw.(json.Number)
					fmt.Sscanf(number.String(), "%d", &limit)
				}
				result := projection.Page(limit, before)
				pages = append(pages, result)
				emit("page", result)
			case "changes":
				spec := value.(*jsoncompat.Object)
				epoch := ""
				if flag, _ := spec.MustGet("epoch_last").(bool); flag && len(pages) > 0 {
					epoch, _ = pages[len(pages)-1].MustGet("epoch").(string)
				}
				after, _ := spec.MustGet("after").(json.Number)
				start, _ := spec.MustGet("start").(string)
				emit("changes", projection.Changes(after.String(), epoch, start))
			default:
				t.Fatalf("unknown projection action %q", key)
			}
		}
	}
	return records
}
