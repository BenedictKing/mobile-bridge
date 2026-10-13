package acpruntime

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestSessionFoldsUpdatesAndRequests(t *testing.T) {
	session := &Session{ID: "sess-1", View: acpview.NewView("sess-1", "Demo", "/work", func() float64 { return 1 }), pending: map[string]*jsoncompat.Object{}, changed: make(chan struct{})}
	update := jsoncompat.NewObject()
	update.Set("method", "session/update")
	params := jsoncompat.NewObject()
	params.Set("sessionUpdate", "agent_message_chunk")
	content := jsoncompat.NewObject()
	content.Set("text", "hello")
	params.Set("content", content)
	update.Set("params", params)
	session.onNotification(update)
	snapshot := session.Snapshot()
	turns := snapshot.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 1 || !jsoncompat.Truthy(turns.Items[0]) {
		t.Fatalf("update did not create turn: %s", jsoncompat.EncodeCompact(snapshot))
	}
	request := jsoncompat.NewObject()
	request.Set("id", "permission-1")
	request.Set("method", "session/request_permission")
	permissionParams := jsoncompat.NewObject()
	toolCall := jsoncompat.NewObject()
	toolCall.Set("title", "Run command")
	toolCall.Set("rawInput", "ls")
	permissionParams.Set("toolCall", toolCall)
	permissionParams.Set("options", &jsoncompat.Array{})
	request.Set("params", permissionParams)
	session.onRequest(request, func(any) {})
	if len(session.Snapshot().MustGet("requests").(*jsoncompat.Array).Items) != 1 {
		t.Fatal("permission request was not exposed")
	}
}
