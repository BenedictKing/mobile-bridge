package timeline

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// TestDeltaChainPropertyAndMissedFrames pins the weak-network recovery
// contract of the delta stream:
//  1. consecutive pure-append frames chain exactly — each row's prevVersion
//     is the version the previous frame published, so applying
//     text += append reconstructs what a fresh Page reader sees;
//  2. a single missed frame is replayable verbatim on reconnect (the since
//     cursor still names the immediately previous generation);
//  3. once two or more generations pass, Deltas refuses to bridge the gap and
//     Changes compensates with full rows — a lost frame can never silently
//     corrupt the client-side text.
func TestDeltaChainPropertyAndMissedFrames(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	text := "开头"
	base := deltaView(1, text)
	instance.Update(base)
	nextView := func(sequence int, current string) *jsoncompat.Object {
		view := updateTextAt(base, 0, current)
		view.Set("sequence", jsoncompat.Num(sequence))
		return view
	}

	page := instance.Page(20, "")
	row, _ := page.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	clientKey, _ := row.MustGet("key").(string)
	clientText, _ := row.MustGet("text").(string)
	clientVersion, _ := row.MustGet("version").(string)
	clientSequence := "1"

	for step := 2; step <= 9; step++ {
		text += "，第" + strconv.Itoa(step) + "段"
		instance.Update(nextView(step, text))
		delta, ok := instance.Deltas(clientSequence)
		if !ok {
			t.Fatalf("step %d: consecutive append must produce a delta", step)
		}
		rows, _ := delta.MustGet("rows").(*jsoncompat.Array)
		if len(rows.Items) != 1 {
			t.Fatalf("step %d: delta rows=%d want 1", step, len(rows.Items))
		}
		item, _ := rows.Items[0].(*jsoncompat.Object)
		if key, _ := item.MustGet("key").(string); key != clientKey {
			t.Fatalf("step %d: delta row key drifted", step)
		}
		if prev, _ := item.MustGet("prevVersion").(string); prev != clientVersion {
			t.Fatalf("step %d: chain broken, prevVersion %q != client version %q",
				step, prev, clientVersion)
		}
		appended, _ := item.MustGet("append").(string)
		clientText += appended
		clientVersion, _ = item.MustGet("version").(string)
		sequence, _ := delta.MustGet("sequence").(json.Number)
		clientSequence = sequence.String()

		fresh, _ := instance.Page(20, "").MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
		freshText, _ := fresh.MustGet("text").(string)
		freshVersion, _ := fresh.MustGet("version").(string)
		if freshText != clientText || freshVersion != clientVersion {
			t.Fatalf("step %d: chained client state diverged from the page view", step)
		}
	}

	// One missed frame: the client never saw step 10, but its cursor still
	// names the previous generation, so the delta must replay exactly.
	text += "，丢一段前"
	instance.Update(nextView(10, text))
	replay, ok := instance.Deltas(clientSequence)
	if !ok {
		t.Fatal("a single missed frame must be replayable via the since cursor")
	}
	replayRows, _ := replay.MustGet("rows").(*jsoncompat.Array)
	replayItem, _ := replayRows.Items[0].(*jsoncompat.Object)
	if prev, _ := replayItem.MustGet("prevVersion").(string); prev != clientVersion {
		t.Fatalf("replay prevVersion %q != held version %q", prev, clientVersion)
	}

	// Two missed generations: the bridge must refuse, and Changes must hand
	// back the full row so the gap closes with absolute text.
	text += "，丢两段"
	instance.Update(nextView(11, text))
	if _, bridged := instance.Deltas(clientSequence); bridged {
		t.Fatal("delta must never bridge two generations")
	}
	compensated := instance.Changes(clientSequence, "epoch-1", "")
	rows, _ := compensated.MustGet("rows").(*jsoncompat.Array)
	if len(rows.Items) != 1 {
		t.Fatalf("changes compensation rows=%d want 1", len(rows.Items))
	}
	full, _ := rows.Items[0].(*jsoncompat.Object)
	fullText, _ := full.MustGet("text").(string)
	fullVersion, _ := full.MustGet("version").(string)
	if fullText != text {
		t.Fatalf("compensation must carry the complete text: %q", fullText)
	}
	if fullVersion != clientVersionOf(t, instance) || fullVersion == clientVersion {
		t.Fatal("compensation must carry the fresh version")
	}
}

func clientVersionOf(t *testing.T, instance *Timeline) string {
	t.Helper()
	row, _ := instance.Page(20, "").MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	version, _ := row.MustGet("version").(string)
	return version
}
