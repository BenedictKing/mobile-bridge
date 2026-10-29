package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDefaultTeamEnsureIsIdempotent(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	project := "acp:" + hashProject(env.dataDir)
	target := "/api/projects/" + project + "/team"
	response := env.request(http.MethodPut, target, `{"leadAgent":"kimi"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建默认团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID                string `json:"id"`
		Project           string `json:"project"`
		ProjectCwd        string `json:"projectCwd"`
		DefaultForProject bool   `json:"defaultForProject"`
		LeadID            string `json:"leadId"`
		Created           bool   `json:"created"`
		Members           []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
		} `json:"members"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Project != project || created.ProjectCwd != env.dataDir || !created.DefaultForProject || !created.Created || created.LeadID == "" || len(created.Members) == 0 {
		t.Fatalf("默认团队投影不正确: %+v", created)
	}
	for _, member := range created.Members {
		if member.SessionID == "" {
			t.Fatalf("默认团队成员缺少专用会话: %+v", member)
		}
	}

	legacy := env.store.Team(created.ID)
	legacy.Project = "codex:changed-prefix"
	h := &handler{server: env.server}
	if got := h.teamProjectCwd(legacy); got != env.dataDir {
		t.Fatalf("项目键漂移后应按稳定工作目录匹配: %q", got)
	}
	response = env.request(http.MethodPut, target, "{}")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"created": false`) ||
		!strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("默认团队重放应复用原团队: %d %s", response.Code, response.Body.String())
	}
	if got := len(env.store.Teams()); got != 1 {
		t.Fatalf("默认团队重放不得重复创建: %d", got)
	}

	response = env.request(http.MethodGet, "/api/projects/"+project+"/agents", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"agent": "kimi"`) {
		t.Fatalf("项目 Agent 列表不正确: %d %s", response.Code, response.Body.String())
	}
}

func TestDefaultTeamRoles(t *testing.T) {
	cases := []struct {
		index, count int
		want         string
	}{
		{0, 1, "lead"},
		{0, 2, "lead"}, {1, 2, "member"},
		{0, 3, "lead"}, {1, 3, "member"}, {2, 3, "reviewer"},
		{1, 4, "member"}, {2, 4, "member"}, {3, 4, "reviewer"},
	}
	for _, testCase := range cases {
		if got := defaultTeamRole(testCase.index, testCase.count); got != testCase.want {
			t.Fatalf("defaultTeamRole(%d, %d)=%q，期望 %q", testCase.index, testCase.count, got, testCase.want)
		}
	}
}

func TestDefaultTeamRejectsUnknownLeadAgent(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	project := "acp:" + hashProject(env.dataDir)
	response := env.request(http.MethodPut, "/api/projects/"+project+"/team", `{"leadAgent":"not-real"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未知主控 Agent 应 400: %d %s", response.Code, response.Body.String())
	}
	if got := len(env.store.Teams()); got != 0 {
		t.Fatalf("拒绝主控选择后不得创建团队: %d", got)
	}
}

func TestDefaultTeamRetriesAfterPositionalLedgerConflict(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	env.server.Creations = LoadCreations("")
	project := "acp:" + hashProject(env.dataDir)
	teamID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:default-team:"+project)).String()
	legacyCreationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("team:"+teamID+":member:member-1")).String()
	env.server.Creations.begin(legacyCreationID, &creationEntry{
		Agent: "zcode", Project: project, Title: "旧主控创建", ID: "sess-old",
	})

	response := env.request(http.MethodPut, "/api/projects/"+project+"/team", `{"leadAgent":"kimi"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("更换主控后重试不应复用旧位置幂等键: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"leadId": "member-kimi"`) {
		t.Fatalf("默认团队成员 ID 应按 Agent 稳定派生: %s", response.Body.String())
	}
}
