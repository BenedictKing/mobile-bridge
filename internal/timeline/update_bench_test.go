package timeline

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// benchView builds a 25-turn, 500-message view; the last assistant message
// streams via lastText.
func benchView(sequence int, lastText string) *jsoncompat.Object {
	turns := &jsoncompat.Array{}
	for turn := 0; turn < 25; turn++ {
		turnObject := jsoncompat.NewObject()
		turnObject.Set("id", fmt.Sprintf("turn-%d", turn))
		status := "completed"
		if turn == 24 {
			status = "inProgress"
		}
		turnObject.Set("status", status)
		messages := &jsoncompat.Array{}
		for i := 0; i < 20; i++ {
			message := jsoncompat.NewObject()
			message.Set("id", fmt.Sprintf("m-%d-%d", turn, i))
			if i == 0 {
				message.Set("kind", "userMessage")
				message.Set("role", "user")
				message.Set("text", "请继续处理第"+strconv.Itoa(turn)+"批任务并汇总结果")
			} else {
				message.Set("kind", "agentMessage")
				message.Set("role", "assistant")
				text := strings.Repeat("这是第"+strconv.Itoa(i)+"段稳定的回复正文，用于撑起投影规模。", 8)
				if turn == 24 && i == 19 {
					text = lastText
				}
				message.Set("text", text)
			}
			messages.Items = append(messages.Items, message)
		}
		turnObject.Set("messages", messages)
		turns.Items = append(turns.Items, turnObject)
	}
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("status", "active")
	view.Set("turns", turns)
	return view
}

// benchAppendView clones the 500-row template and appends one chunk to the
// streaming tail row. Cloning sits outside the measured window: the gateway
// builds views from live snapshots, and both variants must time Update alone.
func benchAppendView(base *jsoncompat.Object, sequence int, lastText string) *jsoncompat.Object {
	view := jsoncompat.Clone(base).(*jsoncompat.Object)
	view.Set("sequence", jsoncompat.Num(sequence))
	tail := view.MustGet("turns").(*jsoncompat.Array).Items[24].(*jsoncompat.Object).
		MustGet("messages").(*jsoncompat.Array).Items[19].(*jsoncompat.Object)
	tail.Set("text", lastText)
	return view
}

// BenchmarkUpdateColdAppend times a full 500-row rebuild: a fresh timeline
// sees the view once. This is the per-Update cost before the reuse fast
// path existed.
func BenchmarkUpdateColdAppend(b *testing.B) {
	b.ReportAllocs()
	base := benchView(0, "")
	instance := New(nil)
	last := "流式尾部"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		last += "追加一段文本"
		b.StopTimer()
		view := benchAppendView(base, i+1, last)
		instance = New(nil)
		b.StartTimer()
		instance.Update(view)
	}
}

// BenchmarkUpdateWarmAppend times the reuse fast path on the same workload:
// one timeline, every Update appends ~6 runes to the last row.
func BenchmarkUpdateWarmAppend(b *testing.B) {
	b.ReportAllocs()
	base := benchView(0, "")
	instance := New(nil)
	last := "流式尾部"
	instance.Update(benchAppendView(base, 1, last))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		last += "追加一段文本"
		b.StopTimer()
		view := benchAppendView(base, i+2, last)
		b.StartTimer()
		instance.Update(view)
	}
}
