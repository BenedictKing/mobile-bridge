package acpview

import (
	"encoding/json"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// kimiPlanPermission 按 Kimi acp-server 的真实报文构造 ExitPlanMode 审批：
// 无 rawInput，首个内容块为 "Plan saved to: <path>\n\n<plan>"，末块为固定请求语。
func kimiPlanPermission(toolCallID, path, plan string) *jsoncompat.Object {
	text := plan
	if path != "" {
		text = "Plan saved to: " + path + "\n\n" + plan
	}
	block := func(value string) *jsoncompat.Object {
		inner := jsoncompat.NewObject()
		inner.Set("type", "text")
		inner.Set("text", value)
		outer := jsoncompat.NewObject()
		outer.Set("type", "content")
		outer.Set("content", inner)
		return outer
	}
	toolCall := jsoncompat.NewObject()
	toolCall.Set("toolCallId", toolCallID)
	toolCall.Set("title", "ExitPlanMode")
	toolCall.Set("content", &jsoncompat.Array{Items: []any{
		block(text),
		block("Requesting approval to Presenting plan and exiting plan mode"),
	}})
	options := &jsoncompat.Array{}
	for _, pair := range [][2]string{{"plan_approve", "allow_once"}, {"plan_revise", "reject_once"}, {"plan_reject_and_exit", "reject_once"}} {
		option := jsoncompat.NewObject()
		option.Set("optionId", pair[0])
		option.Set("kind", pair[1])
		options.Items = append(options.Items, option)
	}
	params := jsoncompat.NewObject()
	params.Set("toolCall", toolCall)
	params.Set("options", options)
	return params
}

func planParams(t *testing.T, view *View, requestID string) *jsoncompat.Object {
	t.Helper()
	requests := view.Snapshot().MustGet("requests").(*jsoncompat.Array)
	if len(requests.Items) == 0 {
		t.Fatal("permission request was not surfaced")
	}
	return requests.Items[len(requests.Items)-1].(*jsoncompat.Object).MustGet("params").(*jsoncompat.Object)
}

func TestKimiPlanPermissionIsSurfacedAsStructuredPlan(t *testing.T) {
	view := NewView("sess-1", "Demo", "/work", func() float64 { return 1 })
	plan := "1. **确定位置与范围**：仅修改 `README.md`。\n2. **提炼问题**。"
	params := kimiPlanPermission("0:tool-1", "/Users/u/.kimi-code/sessions/wd_x/sess-1/agents/main/plans/white-tiger-jakeem.md", plan)
	if !KimiPlanPermission(params) {
		t.Fatal("Kimi plan permission was not recognized")
	}
	view.AddPermission("req-1", params)
	entry := planParams(t, view, "req-1")

	if entry.MustGet("planContent") != plan || entry.MustGet("text") != plan {
		t.Fatalf("plan body must drop the path prefix and request line: %s", jsoncompat.EncodeCompact(entry))
	}
	if entry.MustGet("command") != "" {
		t.Fatalf("plan was exposed as a command: %s", jsoncompat.EncodeCompact(entry))
	}
	// planId 取计划文件名而非 toolCallId：Kimi 每轮重生成工具调用 ID，文件名跨修订稳定。
	if entry.MustGet("planId") != "white-tiger-jakeem" || entry.MustGet("planVersion").(json.Number).String() != "1" {
		t.Fatalf("plan identity = %s", jsoncompat.EncodeCompact(entry))
	}

	view.AddPermission("req-2", kimiPlanPermission("0:tool-2", "/Users/u/.kimi-code/sessions/wd_x/sess-1/agents/main/plans/white-tiger-jakeem.md", plan))
	if same := planParams(t, view, "req-2"); same.MustGet("planVersion").(json.Number).String() != "1" ||
		same.MustGet("planHash") != entry.MustGet("planHash") {
		t.Fatalf("replayed plan advanced revision: %s", jsoncompat.EncodeCompact(same))
	}
	view.AddPermission("req-3", kimiPlanPermission("1:tool-3", "/Users/u/.kimi-code/sessions/wd_x/sess-1/agents/main/plans/white-tiger-jakeem.md", plan+"\n3. **检查交付**。"))
	revised := planParams(t, view, "req-3")
	if revised.MustGet("planVersion").(json.Number).String() != "2" || revised.MustGet("previousPlanContent") != plan {
		t.Fatalf("revised plan lost its previous version: %s", jsoncompat.EncodeCompact(revised))
	}
}

func TestKimiPlanPermissionIgnoresOtherPermissions(t *testing.T) {
	view := NewView("sess-1", "Demo", "/work", func() float64 { return 1 })
	plan := "# Plan\n\n- step one"

	// 无计划文件路径时正文即首块（Kimi 的 display.path 可缺省）。
	plain := kimiPlanPermission("tool-plain", "", plan)
	view.AddPermission("req-plain", plain)
	if entry := planParams(t, view, "req-plain"); entry.MustGet("planContent") != plan || entry.MustGet("planId") != "tool-plain" {
		t.Fatalf("plan without a saved path = %s", jsoncompat.EncodeCompact(entry))
	}

	// 同标题但没有 Revise 选项：不是计划审阅。
	withoutRevise := kimiPlanPermission("tool-1", "", plan)
	withoutRevise.MustGet("options").(*jsoncompat.Array).Items = withoutRevise.MustGet("options").(*jsoncompat.Array).Items[:1]
	view.AddPermission("req-no-revise", withoutRevise)
	if KimiPlanPermission(withoutRevise) {
		t.Fatal("permission without a Revise option was treated as a plan review")
	}

	// 其他工具即使内容里有 "Plan saved to" 也不是计划审阅。
	other := kimiPlanPermission("tool-2", "", plan)
	other.MustGet("toolCall").(*jsoncompat.Object).Set("title", "Write")
	view.AddPermission("req-other", other)
	if KimiPlanPermission(other) {
		t.Fatal("non-plan tool was treated as a plan review")
	}

	// 空正文不算计划，避免把残缺请求渲染成可批注的计划卡。
	empty := kimiPlanPermission("tool-3", "", " ")
	if KimiPlanPermission(empty) || KimiPlanPermission(nil) || KimiPlanPermission(jsoncompat.NewObject()) {
		t.Fatal("empty, nil or toolCall-less params must not be a plan review")
	}

	// 三张非计划卡片都不得带出计划元数据。
	for _, item := range view.Snapshot().MustGet("requests").(*jsoncompat.Array).Items {
		entry := item.(*jsoncompat.Object).MustGet("params").(*jsoncompat.Object)
		if id, _ := entry.MustGet("planId").(string); id == "tool-1" || id == "tool-2" {
			t.Fatalf("non-plan permission %s gained plan metadata: %s", id, jsoncompat.EncodeCompact(entry))
		}
	}
}
