package model

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

func TestContextCompactionUsesCompletionFlagAndParentTurn(t *testing.T) {
	for _, scenario := range []struct {
		name, turn, fields, status, label string
	}{
		{"active", "inProgress", `"completed":false,"source":"automatic"`, "inProgress", "压缩中"},
		{"done", "inProgress", `"completed":true,"source":"manual"`, "completed", "已完成"},
		{"history", "completed", `"source":"automatic"`, "completed", "已完成"},
		{"historyActiveTurn", "inProgress", `"source":"automatic"`, "completed", "已完成"},
		{"completedTurn", "completed", `"completed":false`, "completed", "已完成"},
		{"interrupted", "interrupted", `"completed":false`, "interrupted", "已中断"},
		{"failed", "failed", `"completed":false`, "interrupted", "已中断"},
		{"finishedBeforeFailure", "failed", `"completed":true`, "completed", "已完成"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			value, err := jsoncompat.Unmarshal([]byte(`{"type":"contextCompaction","id":"internal-compaction-id","startedAtMs":123,` + scenario.fields + `}`))
			if err != nil {
				t.Fatal(err)
			}
			item := value.(*jsoncompat.Object)
			turn := jsoncompat.NewObject()
			turn.Set("turnId", "turn")
			turn.Set("status", scenario.turn)
			turn.Set("items", &jsoncompat.Array{Items: []any{item}})
			state := jsoncompat.NewObject()
			state.Set("turns", &jsoncompat.Array{Items: []any{turn}})
			view := NormalizeState(state, true)
			view.Set("sequence", jsoncompat.Num(1))
			projection := timeline.New(func() string { return "compaction" })
			projection.Update(view)
			row := projection.Page(10, "").MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
			text := row.MustGet("text").(string)
			if row.MustGet("status") != scenario.status || row.MustGet("title") != "上下文压缩" || !strings.Contains(text, "压缩状态："+scenario.label) {
				t.Fatalf("压缩状态投影错误: %s", jsoncompat.EncodeCompact(row))
			}
			for _, internal := range []string{"contextCompaction", "internal-compaction-id", "startedAtMs", "{"} {
				if strings.Contains(text, internal) {
					t.Fatalf("压缩正文仍在显示原始字段: %s", text)
				}
			}
			if item.MustGet("source") == "manual" && !strings.Contains(text, "触发方式：手动") {
				t.Fatalf("缺少手动触发来源: %s", text)
			}
			if item.MustGet("source") == "automatic" && !strings.Contains(text, "触发方式：自动") {
				t.Fatalf("缺少自动触发来源: %s", text)
			}
		})
	}
}
