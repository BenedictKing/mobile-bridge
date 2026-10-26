package acpview

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func nativeTestTurn(id, status, text string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", id)
	turn.Set("status", status)
	message := jsoncompat.NewObject()
	message.Set("id", "u-"+id)
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", text)
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	turn.Set("actionable", false)
	return turn
}

func TestApplyNativeReplacesTurnsAndTitle(t *testing.T) {
	view := NewView("sess-1", nil, "/w", func() float64 { return 0 })
	view.BeginTurn("旧问题")
	if view.Status != "active" {
		t.Fatalf("begin status = %q", view.Status)
	}
	snapshot := &NativeSnapshot{Title: "真实标题", Turns: []*jsoncompat.Object{
		nativeTestTurn("t1", "completed", "第一问"),
		nativeTestTurn("t2", "inProgress", "第二问"),
	}}
	if !view.ApplyNative(snapshot) {
		t.Fatal("apply returned false")
	}
	if view.Title != "真实标题" {
		t.Fatalf("title = %v", view.Title)
	}
	if len(view.Turns) != 2 {
		t.Fatalf("turns = %d", len(view.Turns))
	}
	if view.Status != "active" {
		t.Fatalf("in-progress tip status = %q, want active", view.Status)
	}
	snapshot.Turns[1] = nativeTestTurn("t2", "completed", "第二问")
	if !view.ApplyNative(snapshot) || view.Status != "idle" {
		t.Fatalf("settled tip status = %q, want idle", view.Status)
	}
	if tip := view.TipTurn(); tip == nil || tip.MustGet("id") != "t2" {
		t.Fatalf("tip = %v", tip)
	}
}

func TestApplyNativeEmptyIsNoop(t *testing.T) {
	view := NewView("sess-1", nil, "/w", func() float64 { return 0 })
	view.BeginTurn("保留")
	if view.ApplyNative(&NativeSnapshot{Title: "别的"}) {
		t.Fatal("empty snapshot applied")
	}
	if view.Title != nil || view.Status != "active" {
		t.Fatalf("view mutated: title=%v status=%q", view.Title, view.Status)
	}
}

func TestSessionInfoUpdateChangesTitle(t *testing.T) {
	view := NewView("sess-1", "旧标题", "/w", func() float64 { return 0 })
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "session_info_update")
	update.Set("title", "自动标题")
	view.OnUpdate(update)
	if view.Snapshot().MustGet("title") != "自动标题" {
		t.Fatalf("title = %v", view.Snapshot().MustGet("title"))
	}
	metadataOnly := jsoncompat.NewObject()
	metadataOnly.Set("sessionUpdate", "session_info_update")
	metadataOnly.Set("updatedAt", "2026-10-26T00:00:00Z")
	view.OnUpdate(metadataOnly)
	if view.Snapshot().MustGet("title") != "自动标题" {
		t.Fatal("metadata-only update changed the title")
	}
	update.Set("title", nil)
	view.OnUpdate(update)
	if view.Title != nil || view.Snapshot().MustGet("title") != "未命名聊天" {
		t.Fatalf("cleared title = %v", view.Snapshot().MustGet("title"))
	}
}

func TestPlanUpdateAndRemoval(t *testing.T) {
	view := NewView("sess-1", nil, "/w", func() float64 { return 0 })
	view.BeginTurn("审阅计划")
	update := func(id, content string) *jsoncompat.Object {
		update := jsoncompat.NewObject()
		update.Set("sessionUpdate", "plan_update")
		plan := jsoncompat.NewObject()
		plan.Set("type", "markdown")
		plan.Set("planId", id)
		plan.Set("content", content)
		update.Set("plan", plan)
		return update
	}
	view.OnUpdate(update("plan-1", "旧内容"))
	view.OnUpdate(update("plan-2", "保留"))
	view.OnUpdate(update("plan-1", "新内容"))
	plans := func() []*jsoncompat.Object {
		turn := view.TipTurn()
		messages := turn.MustGet("messages").(*jsoncompat.Array)
		rows := []*jsoncompat.Object{}
		for _, item := range messages.Items {
			if message := item.(*jsoncompat.Object); message.MustGet("kind") == "plan" {
				rows = append(rows, message)
			}
		}
		return rows
	}
	rows := plans()
	if len(rows) != 2 || rows[0].MustGet("text") != "新内容" {
		t.Fatalf("plan update duplicated or did not replace: %v", rows)
	}
	removed := jsoncompat.NewObject()
	removed.Set("sessionUpdate", "plan_removed")
	view.OnUpdate(removed)
	if len(plans()) != 2 {
		t.Fatal("missing planId removed a plan")
	}
	removed.Set("planId", "plan-1")
	view.OnUpdate(removed)
	rows = plans()
	if len(rows) != 1 || rows[0].MustGet("planId") != "plan-2" {
		t.Fatalf("wrong plan removed: %v", rows)
	}
}

func TestAddPermissionSurfacesAskUserQuestion(t *testing.T) {
	view := NewView("sess-1", "Demo", "/work", func() float64 { return 1 })
	params := jsoncompat.NewObject()
	toolCall := jsoncompat.NewObject()
	toolCall.Set("title", "Asking user questions")
	text := jsoncompat.NewObject()
	text.Set("type", "text")
	text.Set("text", "选 A 还是 B？")
	block := jsoncompat.NewObject()
	block.Set("type", "content")
	block.Set("content", text)
	toolCall.Set("content", &jsoncompat.Array{Items: []any{block}})
	params.Set("toolCall", toolCall)
	options := &jsoncompat.Array{}
	choice := jsoncompat.NewObject()
	choice.Set("optionId", "q0_opt_0")
	choice.Set("name", "选项 A")
	choice.Set("kind", "allow_once")
	skip := jsoncompat.NewObject()
	skip.Set("optionId", "q0_skip")
	skip.Set("name", "Skip")
	skip.Set("kind", "reject_once")
	options.Items = append(options.Items, choice, skip)
	params.Set("options", options)
	view.AddPermission("req-1", params)
	requests := view.Snapshot().MustGet("requests").(*jsoncompat.Array)
	if len(requests.Items) != 1 {
		t.Fatalf("question permission was not exposed: %s", jsoncompat.EncodeCompact(view.Snapshot()))
	}
	entryParams := requests.Items[0].(*jsoncompat.Object).MustGet("params").(*jsoncompat.Object)
	if entryParams.MustGet("question") != true {
		t.Fatalf("AskUserQuestion permission was not flagged: %s", jsoncompat.EncodeCompact(entryParams))
	}
	if text, _ := entryParams.MustGet("text").(string); text != "选 A 还是 B？" {
		t.Fatalf("question text was not surfaced, got %q", text)
	}
}

func TestToolCallRendersQuestionSummary(t *testing.T) {
	view := NewView("sess-1", "Demo", "/work", func() float64 { return 1 })
	view.BeginTurn("问一下")
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call")
	update.Set("toolCallId", "tool-1")
	update.Set("title", "Asking user questions")
	question := jsoncompat.NewObject()
	question.Set("question", "选 A 还是 B？")
	question.Set("header", "偏好")
	question.Set("options", &jsoncompat.Array{Items: []any{
		optionLabel("A"), optionLabel("B")}})
	rawInput := jsoncompat.NewObject()
	rawInput.Set("questions", &jsoncompat.Array{Items: []any{question}})
	update.Set("rawInput", rawInput)
	text := jsoncompat.NewObject()
	text.Set("type", "text")
	text.Set("text", string(jsoncompat.EncodeCompact(rawInput)))
	block := jsoncompat.NewObject()
	block.Set("type", "content")
	block.Set("content", text)
	update.Set("content", &jsoncompat.Array{Items: []any{block}})
	view.OnUpdate(update)
	snapshot := view.Snapshot()
	turn := snapshot.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	messages := turn.MustGet("messages").(*jsoncompat.Array)
	var toolMessage *jsoncompat.Object
	for _, value := range messages.Items {
		if message := value.(*jsoncompat.Object); message.MustGet("kind") == "toolCall" {
			toolMessage = message
		}
	}
	if toolMessage == nil {
		t.Fatal("question tool call missing")
	}
	toolText, _ := toolMessage.MustGet("text").(string)
	for _, want := range []string{"选 A 还是 B？", "· A", "· B"} {
		if !strings.Contains(toolText, want) {
			t.Fatalf("tool text missing %q: %q", want, toolText)
		}
	}
	if output, _ := toolMessage.MustGet("output").(string); output != "" {
		t.Fatalf("question arguments leaked as raw JSON: %q", output)
	}

	result := jsoncompat.NewObject()
	result.Set("sessionUpdate", "tool_call_update")
	result.Set("toolCallId", "tool-1")
	result.Set("content", &jsoncompat.Array{Items: []any{block}})
	view.OnUpdate(result)
	if output, _ := toolMessage.MustGet("output").(string); output != "" {
		t.Fatalf("question result leaked as raw JSON: %q", output)
	}
}

func TestAddInputSupportsKimiQuestionForms(t *testing.T) {
	variant := func(value string) *jsoncompat.Object {
		entry := jsoncompat.NewObject()
		entry.Set("const", value)
		entry.Set("title", value)
		entry.Set("description", value+" 说明")
		return entry
	}
	single := jsoncompat.NewObject()
	single.Set("type", "string")
	single.Set("title", "Pick a color")
	single.Set("oneOf", &jsoncompat.Array{Items: []any{variant("Red"), variant("Green")}})
	multi := jsoncompat.NewObject()
	multi.Set("type", "array")
	multi.Set("title", "Pick features")
	multi.Set("minItems", 1)
	items := jsoncompat.NewObject()
	items.Set("anyOf", &jsoncompat.Array{Items: []any{variant("Auth"), variant("Uploads")}})
	multi.Set("items", items)
	properties := jsoncompat.NewObject()
	properties.Set("q0", single)
	properties.Set("q1", multi)
	schema := jsoncompat.NewObject()
	schema.Set("type", "object")
	schema.Set("properties", properties)
	schema.Set("required", &jsoncompat.Array{Items: []any{"q0", "q1"}})
	params := jsoncompat.NewObject()
	params.Set("message", "Pick a color\nPick features")
	params.Set("requestedSchema", schema)

	view := NewView("sess-1", "Demo", "/work", func() float64 { return 1 })
	view.AddInput("req-form", params)
	request := view.Snapshot().MustGet("requests").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	if supported, _ := request.MustGet("supported").(bool); !supported {
		t.Fatalf("Kimi question form was rejected: %s", jsoncompat.EncodeCompact(request))
	}
	content := jsoncompat.NewObject()
	content.Set("q0", "Green")
	content.Set("q1", &jsoncompat.Array{Items: []any{"Uploads", "Auth"}})
	validated, err := ValidateForm(schema, content)
	if err != nil {
		t.Fatalf("Kimi question answers were rejected: %v", err)
	}
	if validated.MustGet("q0") != "Green" {
		t.Fatalf("single-select answer = %v", validated.MustGet("q0"))
	}
}

func TestOpenCodeFormConstraints(t *testing.T) {
	text := jsoncompat.NewObject()
	text.Set("type", "string")
	text.Set("format", "uri")
	text.Set("pattern", "^https://")
	multi := jsoncompat.NewObject()
	multi.Set("type", "array")
	multi.Set("title", "功能")
	multi.Set("minItems", jsoncompat.Num(2))
	multi.Set("maxItems", jsoncompat.Num(2))
	variants := &jsoncompat.Array{}
	for _, label := range []string{"A", "B", "C"} {
		variant := jsoncompat.NewObject()
		variant.Set("const", label)
		variants.Items = append(variants.Items, variant)
	}
	items := jsoncompat.NewObject()
	items.Set("anyOf", variants)
	multi.Set("items", items)
	properties := jsoncompat.NewObject()
	properties.Set("homepage", text)
	properties.Set("features", multi)
	schema := jsoncompat.NewObject()
	schema.Set("type", "object")
	schema.Set("properties", properties)
	if !FormSupported(schema) {
		t.Fatal("OpenCode constraints must remain renderable")
	}
	content := jsoncompat.NewObject()
	content.Set("homepage", "https://example.com")
	content.Set("features", &jsoncompat.Array{Items: []any{"A", "B"}})
	if _, err := ValidateForm(schema, content); err != nil {
		t.Fatalf("valid OpenCode answers rejected: %v", err)
	}
	content.Set("features", &jsoncompat.Array{Items: []any{"A"}})
	if _, err := ValidateForm(schema, content); err == nil {
		t.Fatal("minItems was not enforced")
	}
	content.Set("features", &jsoncompat.Array{Items: []any{"A", "B", "C"}})
	if _, err := ValidateForm(schema, content); err == nil {
		t.Fatal("maxItems was not enforced")
	}
}

func optionLabel(label string) *jsoncompat.Object {
	option := jsoncompat.NewObject()
	option.Set("label", label)
	return option
}
