package httpd

import (
	"net/http"
	"testing"
)

func TestTeamDefaultReviewerLifecycle(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	lead := env.createMemberSession(t, "lead-sess", "主控")
	dev := env.createMemberSession(t, "dev-sess", "执行")
	rev := env.createMemberSession(t, "rev-sess", "评审")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+dev+`","role":"member"},{"id":"rev","sessionId":"`+rev+`","role":"member"}]`)
	response := env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"rev","version":1}`)
	if response.Code != http.StatusOK || env.store.Team(teamID).ReviewerID != "rev" {
		t.Fatalf("设置默认评审失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"lead","version":2}`)
	if response.Code != http.StatusOK || env.store.Team(teamID).LeadID != "lead" || env.store.Team(teamID).ReviewerID != "lead" {
		t.Fatalf("主控应可同时作为默认评审: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"leadId":"rev","version":3}`)
	if response.Code != http.StatusOK || env.store.Team(teamID).LeadID != "rev" {
		t.Fatalf("更换主控失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"rev","version":4}`)
	if response.Code != http.StatusOK || env.store.Team(teamID).LeadID != "rev" || env.store.Team(teamID).ReviewerID != "rev" {
		t.Fatalf("默认评审应可同时作为主控: %d %s", response.Code, response.Body.String())
	}
	runID := env.createRun(t, teamID, `{"goal":"持续任务","mode":"single"}`)
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"dev","version":5}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("活跃 Run 更换评审应 409: %d %s", response.Code, response.Body.String())
	}
	if response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`); response.Code != http.StatusOK {
		t.Fatalf("取消 Run 失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"dev","version":5}`)
	if response.Code != http.StatusOK || env.store.Team(teamID).ReviewerID != "dev" {
		t.Fatalf("终态后应允许更换评审: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodDelete, "/api/teams/"+teamID+"/members/dev?version=6", "{}")
	if response.Code != http.StatusOK || env.store.Team(teamID).ReviewerID != "" {
		t.Fatalf("移除成员后应清空失效评审: %d %s", response.Code, response.Body.String())
	}
}
