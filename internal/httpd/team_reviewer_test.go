package httpd

import (
	"encoding/json"
	"fmt"
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

func TestTeamRoleAssignmentsPreserveMemberRoles(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	lead := env.createMemberSession(t, "lead-sess", "主控")
	dev := env.createMemberSession(t, "dev-sess", "执行")
	rev := env.createMemberSession(t, "rev-sess", "评审")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+dev+`","role":"member"},{"id":"rev","sessionId":"`+rev+`","role":"member"}]`)
	steps := []struct {
		name, body, leadID, reviewerID string
	}{
		{"设置评审", `{"reviewerId":"rev","version":%d}`, "lead", "rev"},
		{"切换评审", `{"reviewerId":"dev","version":%d}`, "lead", "dev"},
		{"清空评审", `{"reviewerId":"","version":%d}`, "lead", ""},
		{"重新设置评审", `{"reviewerId":"rev","version":%d}`, "lead", "rev"},
		{"评审兼任主控", `{"leadId":"rev","version":%d}`, "rev", "rev"},
		{"切回主控", `{"leadId":"lead","version":%d}`, "lead", "rev"},
		{"主控兼任评审", `{"reviewerId":"lead","version":%d}`, "lead", "lead"},
		{"再次清空评审", `{"reviewerId":"","version":%d}`, "lead", ""},
	}
	wantRoles := map[string]string{"lead": "lead", "dev": "member", "rev": "member"}
	for index, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			response := env.request(http.MethodPatch, "/api/teams/"+teamID, fmt.Sprintf(step.body, index+1))
			if response.Code != http.StatusOK {
				t.Fatalf("调整角色失败: %d %s", response.Code, response.Body.String())
			}
			var projected struct {
				LeadID     string `json:"leadId"`
				ReviewerID string `json:"reviewerId"`
				Members    []struct {
					ID   string `json:"id"`
					Role string `json:"role"`
				} `json:"members"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &projected); err != nil {
				t.Fatalf("响应解析失败: %v", err)
			}
			stored := env.store.Team(teamID)
			if projected.LeadID != step.leadID || projected.ReviewerID != step.reviewerID || stored.LeadID != step.leadID || stored.ReviewerID != step.reviewerID {
				t.Fatalf("主控/评审标识不符: 响应=%s/%s 持久化=%s/%s", projected.LeadID, projected.ReviewerID, stored.LeadID, stored.ReviewerID)
			}
			if len(projected.Members) != len(wantRoles) || len(stored.Members) != len(wantRoles) {
				t.Fatalf("成员数量被角色调整改变: 响应=%d 持久化=%d", len(projected.Members), len(stored.Members))
			}
			for _, member := range projected.Members {
				if member.Role != wantRoles[member.ID] {
					t.Errorf("响应成员 %s 角色被改写为 %q，期望 %q", member.ID, member.Role, wantRoles[member.ID])
				}
			}
			for _, member := range stored.Members {
				if member.Role != wantRoles[member.ID] {
					t.Errorf("持久化成员 %s 角色被改写为 %q，期望 %q", member.ID, member.Role, wantRoles[member.ID])
				}
			}
		})
	}
}
