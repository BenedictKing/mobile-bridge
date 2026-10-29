package acpview

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func notification(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}

func snapshotMessage(t *testing.T, view *View, kind string) *jsoncompat.Object {
	t.Helper()
	snapshot := view.Snapshot()
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) == 0 {
		t.Fatal("no turn in snapshot")
	}
	turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	for i := len(messages.Items) - 1; i >= 0; i-- {
		message, _ := messages.Items[i].(*jsoncompat.Object)
		if messageKind, _ := message.MustGet("kind").(string); messageKind == kind {
			return message
		}
	}
	t.Fatalf("no %s message in snapshot", kind)
	return nil
}

func attachmentsOf(message *jsoncompat.Object) []*jsoncompat.Object {
	list, _ := message.MustGet("attachments").(*jsoncompat.Array)
	items := []*jsoncompat.Object{}
	if list == nil {
		return items
	}
	for _, item := range list.Items {
		attachment, _ := item.(*jsoncompat.Object)
		if attachment != nil {
			items = append(items, attachment)
		}
	}
	return items
}

func TestTextAcceptsMigratedContentWrappers(t *testing.T) {
	view := NewView("sess-1", "标题", "/work", func() float64 { return 0 })
	view.BeginTurn("问题")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"content":[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]}}`))
	message := snapshotMessage(t, view, "agentMessage")
	if text, _ := message.MustGet("text").(string); text != "第一段\n第二段" {
		t.Fatalf("text=%q", text)
	}
}

// Agent message chunks carrying non-text blocks used to vanish outright
// (Text() only reads the text field); they must become row attachments now.
func TestNonTextChunksBecomeAttachments(t *testing.T) {
	view := NewView("sess-1", "标题", "/work", func() float64 { return 0 })
	view.BeginTurn("画一张图")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"text","text":"结果如下"}}`))
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"image","path":"/data/agent-media/sess-1/abc.png","mimeType":"image/png"}}`))
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"resource_link","uri":"file:///work/report.html","name":"报告"}}`))
	// The same image streamed again must not duplicate the attachment.
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"image","path":"/data/agent-media/sess-1/abc.png","mimeType":"image/png"}}`))

	message := snapshotMessage(t, view, "agentMessage")
	if text, _ := message.MustGet("text").(string); text != "结果如下" {
		t.Fatalf("text=%q", text)
	}
	attachments := attachmentsOf(message)
	if len(attachments) != 2 {
		t.Fatalf("attachments=%d want 2: %v", len(attachments), attachments)
	}
	image, _ := attachments[0].MustGet("path").(string)
	if attachments[0].MustGet("type") != "localImage" || image != "/data/agent-media/sess-1/abc.png" {
		t.Fatalf("image attachment=%v", attachments[0])
	}
	if name, _ := attachments[0].MustGet("name").(string); name != "abc.png" {
		t.Fatalf("image name=%q", name)
	}
	if attachments[1].MustGet("type") != "file" ||
		attachments[1].MustGet("path") != "/work/report.html" ||
		attachments[1].MustGet("name") != "报告" {
		t.Fatalf("resource attachment=%v", attachments[1])
	}
}

// Remote resource links have no local preview and stay out of the view; a
// chunk carrying nothing else creates no message at all.
func TestRemoteResourceLinkIgnored(t *testing.T) {
	view := NewView("sess-1", nil, "/work", func() float64 { return 0 })
	view.BeginTurn("看链接")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"https://example.com/a.png","name":"远程"}}`))
	snapshot := view.Snapshot()
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	for _, item := range messages.Items {
		if message, _ := item.(*jsoncompat.Object); message != nil {
			if attachments := attachmentsOf(message); len(attachments) != 0 {
				t.Fatalf("remote link became attachment: %v", attachments)
			}
		}
	}
}

// Tool call content arrays contribute image attachments alongside the
// flattened text output.
func TestToolCallContentAttachments(t *testing.T) {
	view := NewView("sess-1", nil, "/work", func() float64 { return 0 })
	view.BeginTurn("跑工具")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"截图","kind":"screenshot","rawInput":{"command":"snap"},"content":[{"type":"text","text":"done"},{"type":"image","path":"/tmp/shot.png","mimeType":"image/png"}]}`))
	message := snapshotMessage(t, view, "toolCall")
	attachments := attachmentsOf(message)
	if len(attachments) != 1 || attachments[0].MustGet("path") != "/tmp/shot.png" {
		t.Fatalf("tool attachments=%v", attachments)
	}
	if output, _ := message.MustGet("output").(string); output != "done" {
		t.Fatalf("output=%q", output)
	}

	// A later tool_call_update adding another image appends only the new one.
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"image","path":"/tmp/second.png"}]}`))
	message = snapshotMessage(t, view, "toolCall")
	if attachments = attachmentsOf(message); len(attachments) != 2 {
		t.Fatalf("after update attachments=%v", attachments)
	}
}

func TestBeginTurnKeepsUserAttachments(t *testing.T) {
	view := NewView("sess-1", nil, "/work", func() float64 { return 0 })
	image := notification(t, `{"type":"localImage","path":"/tmp/upload.png","name":"截图.png"}`)
	document := notification(t, `{"type":"file","path":"/tmp/notes.txt","name":"笔记.txt"}`)
	view.BeginTurn("看附件", image, document)

	message := snapshotMessage(t, view, "userMessage")
	attachments := attachmentsOf(message)
	if len(attachments) != 2 {
		t.Fatalf("user attachments=%v", attachments)
	}
	if attachments[0].MustGet("type") != "localImage" || attachments[0].MustGet("path") != "/tmp/upload.png" ||
		attachments[1].MustGet("type") != "file" || attachments[1].MustGet("name") != "笔记.txt" {
		t.Fatalf("user attachments=%v", attachments)
	}
}
