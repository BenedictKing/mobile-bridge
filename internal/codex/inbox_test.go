package codex

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestPendingRequestsReportsOnlyFollowedSessionsWithRequests(t *testing.T) {
	bridge := New(t.TempDir(), "")
	snapshot := func(id, title string, requests int) {
		state := jsoncompat.NewObject()
		state.Set("id", id)
		state.Set("title", title)
		list := &jsoncompat.Array{}
		for i := 0; i < requests; i++ {
			request := jsoncompat.NewObject()
			request.Set("id", id+"-req")
			request.Set("method", "item/commandExecution/requestApproval")
			list.Items = append(list.Items, request)
		}
		state.Set("requests", list)
		payload := jsoncompat.NewObject()
		payload.Set("type", "snapshot")
		payload.Set("revision", jsoncompat.Num(1))
		payload.Set("conversationState", state)
		bridge.handleEvent(event(id, payload, "owner"))
	}
	snapshot("thread-waiting", "需要确认", 1)
	snapshot("thread-idle", "空闲", 0)

	rows := bridge.PendingRequests()
	if len(rows) != 1 || rows[0].MustGet("id") != "thread-waiting" || rows[0].MustGet("title") != "需要确认" {
		t.Fatalf("pending rows = %v", rows)
	}
	// 返回克隆：调用方修改不得回写到跟随中的会话状态。
	rows[0].MustGet("requests").(*jsoncompat.Array).Items = nil
	view, _ := bridge.View("thread-waiting")
	if requests := view.MustGet("requests").(*jsoncompat.Array); len(requests.Items) != 1 {
		t.Fatalf("pending rows leaked shared state: %v", requests)
	}
}
