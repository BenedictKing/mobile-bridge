package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDefaultTeamEnsureIsIdempotent(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	project := "acp:" + hashProject(env.dataDir)
	target := "/api/projects/" + project + "/team"
	response := env.request(http.MethodPut, target, "{}")
	if response.Code != http.StatusOK {
		t.Fatalf("创建默认团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID                string `json:"id"`
		Project           string `json:"project"`
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
	if created.ID == "" || created.Project != project || !created.DefaultForProject || !created.Created || created.LeadID == "" || len(created.Members) == 0 {
		t.Fatalf("默认团队投影不正确: %+v", created)
	}
	for _, member := range created.Members {
		if member.SessionID == "" {
			t.Fatalf("默认团队成员缺少专用会话: %+v", member)
		}
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
