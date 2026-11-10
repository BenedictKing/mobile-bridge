package model

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestNormalizeACPPlanPermissionRetainsPresentationFields(t *testing.T) {
	params := jsoncompat.NewObject()
	params.Set("text", "计划摘要")
	params.Set("planContent", "# 计划详情")
	params.Set("allowedPrompts", &jsoncompat.Array{Items: []any{"run tests"}})
	params.Set("command", "")
	params.Set("options", &jsoncompat.Array{})
	request := jsoncompat.NewObject()
	request.Set("id", "req-plan")
	request.Set("method", "acp/requestPermission")
	request.Set("params", params)

	result := obj(NormalizeRequest(request).MustGet("params"))
	for _, key := range []string{"text", "planContent", "allowedPrompts", "command", "options"} {
		if _, ok := result.Get(key); !ok {
			t.Errorf("normalized params dropped %q", key)
		}
	}
	if result.MustGet("planContent") != "# 计划详情" {
		t.Fatalf("plan content changed: %v", result.MustGet("planContent"))
	}
}

func normalizeFixture(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	state, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return obj(state)
}

func normalizedUsers(view *jsoncompat.Object) []*jsoncompat.Object {
	var users []*jsoncompat.Object
	for _, turn := range arr(view.MustGet("turns")).Items {
		for _, message := range arr(obj(turn).MustGet("messages")).Items {
			if row := obj(message); row.MustGet("role") == "user" {
				users = append(users, row)
			}
		}
	}
	return users
}

func TestNormalizeStateReconcilesOpeningBySubmissionID(t *testing.T) {
	for _, kind := range []string{"userMessage", "steeringUserMessage"} {
		t.Run(kind, func(t *testing.T) {
			state := normalizeFixture(t, `{"turns":[
				{"turnId":"active","items":[{"id":"real","type":"`+kind+`",
				"clientId":"submission-1","clientUserMessageId":"submission-1","status":"accepted",
				"content":[{"type":"text","text":"同一条消息"},{"type":"localImage","path":"/photo.png"}],
				"input":[{"type":"text","text":"同一条消息"},{"type":"localImage","path":"/photo.png"}]}]},
				{"turnId":null,"status":"inProgress","items":[],"params":{
				"clientUserMessageId":"submission-1","input":[{"type":"text","text":"同一条消息"}]}}
			]}`)
			view := NormalizeState(state, true)
			users := normalizedUsers(view)
			if len(users) != 1 || users[0].MustGet("id") != "real" {
				t.Fatalf("replayed input duplicated: %s", jsoncompat.EncodeCompact(view))
			}
			if len(arr(users[0].MustGet("attachments")).Items) != 1 {
				t.Fatal("replayed image attachment was lost")
			}
			if len(arr(view.MustGet("turns")).Items) != 1 {
				t.Fatal("acknowledged optimistic turn should not remain as an empty running turn")
			}
		})
	}
}

func TestNormalizeStateKeepsDistinctSendsUntilTheirReplay(t *testing.T) {
	state := normalizeFixture(t, `{"turns":[
		{"turnId":"active","items":[{"id":"first","type":"userMessage","clientId":"send-1",
		"content":[{"type":"text","text":"相同文字"}]}]},
		{"turnId":null,"items":[],"params":{"clientUserMessageId":"send-2",
		"input":[{"type":"text","text":"相同文字"},{"type":"localImage","path":"/second.png"}]}},
		{"turnId":null,"items":[],"params":{"clientUserMessageId":"send-3",
		"input":[{"type":"text","text":"相同文字"}]}}
	]}`)
	users := normalizedUsers(NormalizeState(state, true))
	if len(users) != 3 || users[1].MustGet("requestId") != "send-2" || users[1].MustGet("id") == users[2].MustGet("id") {
		t.Fatalf("unreplayed identical sends must keep distinct identities: %v", users)
	}
	if len(arr(users[1].MustGet("attachments")).Items) != 1 {
		t.Fatal("pending input lost its image")
	}
	// The new messages may replay into the existing active turn, rather than
	// replacing the desktop's id-less optimistic turns.
	items := arr(obj(arr(state.MustGet("turns")).Items[0]).MustGet("items"))
	items.Items = append(items.Items,
		normalizeFixture(t, `{"id":"second","type":"userMessage","clientId":"send-2","content":[{"type":"text","text":"相同文字"},{"type":"localImage","path":"/second.png"}]}`),
		normalizeFixture(t, `{"id":"third","type":"userMessage","clientId":"send-3","content":[{"type":"text","text":"相同文字"}]}`))
	view := NormalizeState(state, true)
	users = normalizedUsers(view)
	if len(users) != 3 || users[1].MustGet("id") != "second" || users[2].MustGet("id") != "third" {
		t.Fatalf("replay must replace only each matching opening: %s", jsoncompat.EncodeCompact(view))
	}
}

func TestNormalizeStateReconcilesCanonicalHistory(t *testing.T) {
	state := normalizeFixture(t, `{"turnHistory":{"kind":"canonical","history":{
		"entitiesByKey":{
		"pending":{"turnId":null,"params":{"clientUserMessageId":"send","input":[{"type":"text","text":"输入"}]},"items":[]},
		"real":{"turnId":"active","items":{"entitiesByKey":{"u":{"id":"user","type":"userMessage","clientId":"send","content":[{"type":"text","text":"输入"}]}},"islands":[{"entries":[{"value":"u"}]}]}}},
		"islands":[{"entries":[{"value":"pending"},{"value":"real"}]}]}}}`)
	users := normalizedUsers(NormalizeState(state, true))
	if len(users) != 1 || users[0].MustGet("id") != "user" {
		t.Fatal("canonical history did not reconcile replay following the optimistic turn")
	}
}
