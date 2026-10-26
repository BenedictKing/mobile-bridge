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

func optionLabel(label string) *jsoncompat.Object {
	option := jsoncompat.NewObject()
	option.Set("label", label)
	return option
}
