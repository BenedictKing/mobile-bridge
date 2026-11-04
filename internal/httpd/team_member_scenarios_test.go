package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestTeamQuotaRecoveryTemporarilyRebindsReviewer(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	original := env.createMemberSession(t, "reviewer-original", "原评审")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"rev","sessionId":"`+original+`","role":"member"}]`)
	response := env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"rev","version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("设置默认评审失败: %d %s", response.Code, response.Body.String())
	}
	runID := env.createRun(t, teamID, `{"goal":"额度不足临时接替","mode":"pipeline","plan":[
		{"id":"review","memberId":"rev","prompt":"首次评审 QUOTA-REBIND QUOTAFAILFOR:`+original+`","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("额度不足未使 Run 进入 waiting_input")
	}

	replacementManager := acpruntime.NewCommand(teamAgent(t, dataDir))
	replacementManager.Label = "ZCode"
	env.server.ACPAgents["zcode"] = replacementManager
	replacementManager.AutoContinue = func(session *acpruntime.Session) {
		env.server.QueueSettleACP("zcode", session)
	}
	t.Cleanup(func() { replacementManager.AutoContinue = nil; replacementManager.Close() })
	replacementSession, err := replacementManager.Create("reviewer-replacement", dataDir, "临时接替评审", 30*time.Second)
	if err != nil {
		t.Fatalf("创建接替 Agent 会话失败: %v", err)
	}
	replacement := replacementSession.ID

	// 用户保留 rev 这个团队角色，只把执行会话从额度不足的原 Agent 换到接替 Agent。
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/rev",
		`{"sessionId":"`+replacement+`","version":2}`)
	if response.Code != http.StatusOK {
		t.Fatalf("额度不足后换绑成员失败: %d %s", response.Code, response.Body.String())
	}
	var projected struct {
		LeadID     string `json:"leadId"`
		ReviewerID string `json:"reviewerId"`
		Members    []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
			AgentKind string `json:"agentKind"`
			Role      string `json:"role"`
			Enabled   bool   `json:"enabled"`
		} `json:"members"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &projected); err != nil {
		t.Fatalf("换绑响应解析失败: %v", err)
	}
	member := projected.Members[1]
	if projected.LeadID != "lead" || projected.ReviewerID != "rev" || member.ID != "rev" ||
		member.SessionID != replacement || member.AgentKind != "zcode" || member.Role != team.RoleMember || !member.Enabled {
		t.Fatalf("换绑必须保持成员身份与团队角色: %+v", projected)
	}

	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("接替后恢复 Run 失败: %d %s", response.Code, response.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatalf("接替 Agent 未能完成额度停靠任务: run=%v team=%+v prompts=%v", env.runDetail(t, teamID, runID), env.store.Team(teamID), readTeamPrompts(t, dataDir))
	}
	detail := env.runDetail(t, teamID, runID)
	task := teamTaskOf(t, detail, "review")
	if task["attempt"] != float64(2) || taskResultOf(t, task)["sessionId"] != replacement {
		t.Fatalf("重试应由接替会话完成且 attempt=2: %v", task)
	}
	records := readTeamPrompts(t, dataDir)
	if len(records) != 2 || records[0].session != original || records[1].session != replacement {
		t.Fatalf("首次派发和接替重试应分别进入原会话与新会话: %v", records)
	}

	// 原 Agent 额度恢复后换回原会话；下一项任务仍使用同一个 rev 成员角色。
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/rev",
		`{"sessionId":"`+original+`","version":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("换回原 Agent 失败: %d %s", response.Code, response.Body.String())
	}
	secondRunID := env.createRun(t, teamID, `{"goal":"原 Agent 恢复","mode":"pipeline","plan":[
		{"id":"review-restored","memberId":"rev","prompt":"恢复后评审 REVIEWER-RESTORED","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, secondRunID)["status"] == "completed"
	}) {
		t.Fatal("换回原 Agent 后任务未完成")
	}
	records = readTeamPrompts(t, dataDir)
	if len(records) != 3 || records[2].session != original || !strings.Contains(records[2].text, "REVIEWER-RESTORED") {
		t.Fatalf("恢复后的任务应回到原 Agent: %v", records)
	}
	stored := env.store.Team(teamID)
	if stored.LeadID != "lead" || stored.ReviewerID != "rev" || stored.Members[1].Role != team.RoleMember ||
		stored.Members[1].SessionID != original || stored.Members[1].AgentKind != "kimi" {
		t.Fatalf("临时接替与恢复不得改写团队角色: %+v", stored)
	}
}

func TestTeamMemberOfflineAndRestore(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	dev := env.createMemberSession(t, "dev-sess", "执行")
	rev := env.createMemberSession(t, "rev-sess", "评审")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+dev+`","role":"member"},{"id":"rev","sessionId":"`+rev+`","role":"member"}]`)
	response := env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"rev","version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("设置默认评审失败: %d %s", response.Code, response.Body.String())
	}

	for _, memberID := range []string{"lead", "rev"} {
		response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/"+memberID, `{"enabled":false,"version":2}`)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("主控或当前默认评审 %s 未交接前不得下线: %d %s", memberID, response.Code, response.Body.String())
		}
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"dev","version":2}`)
	if response.Code != http.StatusOK {
		t.Fatalf("交接默认评审失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/rev", `{"enabled":false,"version":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("交接后下线普通成员失败: %d %s", response.Code, response.Body.String())
	}
	stored := env.store.Team(teamID)
	if len(stored.Members) != 3 || stored.Members[2].Enabled || stored.Members[2].Role != team.RoleMember || stored.ReviewerID != "dev" {
		t.Fatalf("下线应保留成员记录与角色，仅更新 enabled: %+v", stored)
	}

	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", `{"goal":"停用成员不可接新任务","mode":"pipeline","plan":[
		{"id":"offline-task","memberId":"rev","prompt":"不应派发 OFFLINE-TASK","writeLock":"none"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "已停用") {
		t.Fatalf("已下线成员不得接收新任务: %d %s", response.Code, response.Body.String())
	}
	otherRunID := env.createRun(t, teamID, `{"goal":"其他成员继续工作","mode":"pipeline","plan":[
		{"id":"online-task","memberId":"dev","prompt":"其他成员执行 ONLINE-OTHER","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, otherRunID)["status"] == "completed"
	}) {
		t.Fatal("成员下线不应阻塞其他成员执行任务")
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 1 || records[0].session != dev {
		t.Fatalf("下线期间只应派发其他成员的任务: %v", records)
	}

	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/rev", `{"enabled":true,"version":4}`)
	if response.Code != http.StatusOK {
		t.Fatalf("恢复成员上线失败: %d %s", response.Code, response.Body.String())
	}
	restoredRunID := env.createRun(t, teamID, `{"goal":"恢复成员接任务","mode":"pipeline","plan":[
		{"id":"restored-task","memberId":"rev","prompt":"恢复后执行 ONLINE-RESTORED","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, restoredRunID)["status"] == "completed"
	}) {
		t.Fatal("恢复上线后成员未能接收任务")
	}
	records := readTeamPrompts(t, dataDir)
	if len(records) != 2 || records[1].session != rev || !strings.Contains(records[1].text, "ONLINE-RESTORED") {
		t.Fatalf("恢复上线后的任务应派发给原成员会话: %v", records)
	}
	stored = env.store.Team(teamID)
	if !stored.Members[2].Enabled || stored.Members[2].Role != team.RoleMember || stored.LeadID != "lead" || stored.ReviewerID != "dev" {
		t.Fatalf("下线与恢复不得改变成员身份或团队角色: %+v", stored)
	}
}
