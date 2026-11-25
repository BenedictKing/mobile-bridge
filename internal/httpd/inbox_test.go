package httpd

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/planstore"
)

type inboxResponse struct {
	Items []struct {
		Agent       string          `json:"agent"`
		SessionID   string          `json:"sessionId"`
		Host        string          `json:"host"`
		Title       string          `json:"title"`
		RequestID   string          `json:"requestId"`
		Kind        string          `json:"kind"`
		Summary     string          `json:"summary"`
		Supported   bool            `json:"supported"`
		PlanID      string          `json:"planId"`
		PlanVersion json.RawMessage `json:"planVersion"`
	} `json:"items"`
}

func TestInboxAggregatesLoadedSessionsReadOnly(t *testing.T) {
	dir := t.TempDir()
	server, token, csrf := newFeatureServer(t, t.TempDir())
	store, err := planstore.Open(filepath.Join(t.TempDir(), "acp-plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand(kimiPlanAgent(t, dir))
	manager.Kind = "kimi"
	manager.Plans = store
	defer manager.Close()
	// 另一个从未加载过会话的 Agent：Inbox 不得为它启动进程。
	idle := acpruntime.NewCommand([]string{"/nonexistent/agent"})
	idle.Kind = "zcode"
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager, "zcode": idle}
	server.Plans = store

	empty := featureRequest(server, token, csrf, http.MethodGet, "/api/inbox", "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"items": []`) {
		t.Fatalf("empty inbox status=%d body=%s", empty.Code, empty.Body.String())
	}

	create := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions", `{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"计划","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	session := manager.Get("km-plan")
	go func() { _ = session.Prompt("plan please", 30*time.Second) }()
	deadline := time.Now().Add(10 * time.Second)
	for session.RequestParams("plan-req") == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if session.RequestParams("plan-req") == nil {
		t.Fatal("plan request never arrived")
	}

	response := featureRequest(server, token, csrf, http.MethodGet, "/api/inbox", "")
	if response.Code != http.StatusOK {
		t.Fatalf("inbox status=%d body=%s", response.Code, response.Body.String())
	}
	var inbox inboxResponse
	if err := json.Unmarshal(response.Body.Bytes(), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox.Items) != 1 {
		t.Fatalf("inbox items = %s", response.Body.String())
	}
	item := inbox.Items[0]
	if item.Agent != "kimi" || item.SessionID != "km-plan" || item.Host != "local" || item.RequestID != "plan-req" ||
		item.Kind != "plan" || !item.Supported || item.PlanID != "white-tiger" || string(item.PlanVersion) != "1" {
		t.Fatalf("inbox item = %+v", item)
	}
	if item.Summary != "# Plan - step one - step two" {
		t.Fatalf("summary must be a single compact line, got %q", item.Summary)
	}
	// 列表行不携带计划正文等大字段；回应仍需回到原聊天。
	if strings.Contains(response.Body.String(), "planContent") || strings.Contains(response.Body.String(), "options") {
		t.Fatalf("inbox leaked full request payload: %s", response.Body.String())
	}
	if idle.Peek("anything") != nil || len(idle.Active()) != 0 {
		t.Fatal("inbox started a session for an agent with nothing loaded")
	}

	// 侧栏徽标随活动轮询返回同源计数，即使该聊天不在手机当前列表里。
	activity := featureRequest(server, token, csrf, http.MethodPost, "/api/activity", `{"hosts":{}}`)
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"inboxCount": 1`) {
		t.Fatalf("activity inbox count status=%d body=%s", activity.Code, activity.Body.String())
	}

	write := featureRequest(server, token, csrf, http.MethodPost, "/api/inbox", `{}`)
	if write.Code != http.StatusMethodNotAllowed {
		t.Fatalf("inbox write status=%d body=%s", write.Code, write.Body.String())
	}
	if session.RequestParams("plan-req") == nil {
		t.Fatal("reading the inbox consumed the pending request")
	}
}

func TestInboxSummaryClassifiesRequestDialects(t *testing.T) {
	long := strings.Repeat("长", 200)
	cases := []struct{ method, params, want string }{
		{"acp/requestInput", `{"message":"请选择部署环境"}`, "input"},
		{"acp/requestPermission", `{"question":true,"text":"选 A 还是 B？"}`, "question"},
		{"item/plan/requestImplementation", `{"text":"实施计划"}`, "plan"},
		{"item/commandExecution/requestApproval", `{"command":"make test"}`, "permission"},
	}
	for _, tc := range cases {
		params := jsonObject(t, tc.params)
		kind, summary := inboxSummary(tc.method, params)
		if kind != tc.want || summary == "" {
			t.Fatalf("%s => kind=%s summary=%q", tc.method, kind, summary)
		}
	}
	_, clipped := inboxSummary("acp/requestPermission", jsonObject(t, `{"planContent":"`+long+`"}`))
	if runes := []rune(clipped); len(runes) != 161 || !strings.HasSuffix(clipped, "…") {
		t.Fatalf("long summary not clipped: %d", len(runes))
	}
	if kind, summary := inboxSummary("custom/method", nil); kind != "permission" || summary != "custom/method" {
		t.Fatalf("nil params => %s %q", kind, summary)
	}
}
