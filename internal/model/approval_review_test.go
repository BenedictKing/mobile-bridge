package model

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

func TestApprovalReviewIsReadableBeforeTimelinePreviewTruncation(t *testing.T) {
	item := jsoncompat.NewObject()
	item.Set("id", "automatic-approval-review:internal-id")
	item.Set("type", "automaticApprovalReview")
	item.Set("targetItemId", "exec-internal-id")
	item.Set("turnId", "internal-turn-id")
	item.Set("status", "approved")
	item.Set("riskLevel", "low")
	item.Set("userAuthorization", "medium")
	item.Set("rationale", strings.Repeat("已确认是隔离测试进程，可以安全停止。", 30))
	action := jsoncompat.NewObject()
	action.Set("type", "command")
	action.Set("source", "unifiedExec")
	action.Set("command", "kill -INT 123")
	action.Set("cwd", "/workspace")
	item.Set("action", action)
	item.Set("startedAtMs", jsoncompat.Num(12345))
	row := NormalizeItem(item)
	text := row.MustGet("text").(string)
	for _, want := range []string{"审批状态：已批准", "风险等级：低", "用户授权：中", "审查依据：", "命令：kill -INT 123"} {
		if !strings.Contains(text, want) {
			t.Fatalf("审查正文缺少 %q: %s", want, text)
		}
	}
	for _, internal := range []string{"internal-id", "internal-turn-id", "startedAtMs", "unifiedExec"} {
		if strings.Contains(text, internal) {
			t.Fatalf("审查正文不应泄漏内部协议字段 %s", internal)
		}
	}
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(1))
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn")
	turn.Set("status", "completed")
	turn.Set("messages", &jsoncompat.Array{Items: []any{row}})
	view.Set("turns", &jsoncompat.Array{Items: []any{turn}})
	projection := timeline.New(func() string { return "epoch" })
	projection.Update(view)
	page := projection.Page(10, "")
	preview := page.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	short := preview.MustGet("text").(string)
	if !strings.HasPrefix(short, "审批状态：已批准") || !strings.Contains(short, "审查依据：") || strings.HasPrefix(short, "{") {
		t.Fatalf("真实时间线预览仍被内部 JSON 占据: %q", short)
	}
	if preview.MustGet("truncated") != true {
		t.Fatal("长依据应保留展开完整内容入口")
	}
	if len([]rune(short)) > 180 {
		t.Fatal("不能通过放大时间线预算隐藏渲染问题")
	}
}

func TestThoughtNormalizationKeepsMarkdownAcrossSummaryFormats(t *testing.T) {
	for _, scenario := range []struct {
		kind    string
		summary any
		text    string
	}{
		{"reasoning", &jsoncompat.Array{Items: []any{"## 计划\n- **核对**"}}, ""},
		{"reasoning", &jsoncompat.Array{Items: []any{func() *jsoncompat.Object {
			block := jsoncompat.NewObject()
			block.Set("text", "## 计划\n- **核对**")
			return block
		}()}}, ""},
		{"agentThought", nil, "## 计划\n- **核对**"},
	} {
		item := jsoncompat.NewObject()
		item.Set("type", scenario.kind)
		if scenario.summary != nil {
			item.Set("summary", scenario.summary)
		}
		if scenario.text != "" {
			item.Set("text", scenario.text)
		}
		row := NormalizeItem(item)
		if row.MustGet("text") != "## 计划\n- **核对**" || row.MustGet("title") != "思考摘要" || row.MustGet("role") != "activity" {
			t.Fatalf("思考摘要 Markdown 丢失: %s", jsoncompat.EncodeCompact(row))
		}
	}
}
