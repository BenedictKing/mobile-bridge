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

func TestTeamMemberOfflineDuringRunParksPendingTask(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	dev := env.createMemberSession(t, "dev-sess", "执行")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+dev+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"运行中下线成员","mode":"pipeline","plan":[
		{"id":"first","memberId":"lead","prompt":"在途任务 OFFLINE-MID-RUN HOLDUNTIL:offline-mid","writeLock":"none"},
		{"id":"pending","memberId":"dev","prompt":"下线前不得派发 OFFLINE-MID-PENDING","dependsOn":["first"],"writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "first")["status"] == "running"
	}) {
		t.Fatal("首个任务未进入 running")
	}

	response := env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev", `{"enabled":false,"version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("运行中下线普通成员失败: %d %s", response.Code, response.Body.String())
	}
	releaseRunTaskTurn(t, env, teamID, runID, "first", "offline-mid")
	if !env.drive(t, teamIntegrationWait, func() bool {
		status := env.runDetail(t, teamID, runID)["status"]
		return status == "waiting_input" || status == "completed" || status == "failed" || status == "stopped"
	}) {
		t.Fatal("运行中下线后的 Run 未在时限内收敛")
	}
	detail := env.runDetail(t, teamID, runID)
	pending := teamTaskOf(t, detail, "pending")
	if detail["status"] != "waiting_input" || pending["status"] != "stopped" || pending["outcome"] != "member_disabled" {
		t.Fatalf("下线成员的未派任务应停靠等待，而不是继续执行: run=%v", detail)
	}
	if records := readTeamPrompts(t, dataDir); promptIndex(records, "OFFLINE-MID-PENDING") >= 0 {
		t.Fatalf("成员下线后不得再派发其任务: %v", records)
	}

	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev", `{"enabled":true,"version":2}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重新上线成员失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("恢复下线停靠的 Run 失败: %d %s", response.Code, response.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatalf("重新上线后 Run 未完成: %v", env.runDetail(t, teamID, runID))
	}
	records := readTeamPrompts(t, dataDir)
	if index := promptIndex(records, "OFFLINE-MID-PENDING"); index < 0 || records[index].session != dev {
		t.Fatalf("恢复后应把停靠任务派给重新上线的成员: %v", records)
	}
}

func TestTeamMemberCompositeUpdatesAreIndependent(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	lead := env.createMemberSession(t, "lead-sess", "主控")
	original := env.createMemberSession(t, "dev-original", "原执行")
	replacement := env.createMemberSession(t, "dev-replacement", "接替执行")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+original+`","role":"member"}]`)
	response := env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"lead","version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("设置主控兼任评审失败: %d %s", response.Code, response.Body.String())
	}

	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev",
		`{"name":"临时评审","role":"reviewer","sessionId":"`+replacement+`","enabled":true,"version":2}`)
	if response.Code != http.StatusOK {
		t.Fatalf("组合更新成员失败: %d %s", response.Code, response.Body.String())
	}
	stored := env.store.Team(teamID)
	member := stored.Members[1]
	if stored.LeadID != "lead" || stored.ReviewerID != "lead" || member.Name != "临时评审" ||
		member.Role != team.RoleReviewer || member.SessionID != replacement || !member.Enabled {
		t.Fatalf("成员资料、角色、会话和启停应可独立组合更新: %+v", stored)
	}

	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev", `{"name":"执行","role":"member","version":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("恢复成员资料与角色失败: %d %s", response.Code, response.Body.String())
	}
	stored = env.store.Team(teamID)
	member = stored.Members[1]
	if stored.LeadID != "lead" || stored.ReviewerID != "lead" || member.Name != "执行" ||
		member.Role != team.RoleMember || member.SessionID != replacement || !member.Enabled {
		t.Fatalf("部分更新不得重置未提交的成员字段或团队分工: %+v", stored)
	}
}

func TestTeamMemberRebindDuringRunUsesLatestSessionForPendingTask(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	original := env.createMemberSession(t, "dev-original", "原执行")
	replacement := env.createMemberSession(t, "dev-replacement", "主动接替")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+original+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"运行中主动换绑","mode":"pipeline","plan":[
		{"id":"first","memberId":"lead","prompt":"在途任务 REBIND-MID-RUN HOLDUNTIL:rebind-mid","writeLock":"none"},
		{"id":"pending","memberId":"dev","prompt":"主动换绑后执行 REBIND-MID-PENDING","dependsOn":["first"],"writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "first")["status"] == "running"
	}) {
		t.Fatal("首个任务未进入 running")
	}
	response := env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev", `{"sessionId":"`+replacement+`","version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("运行中主动换绑失败: %d %s", response.Code, response.Body.String())
	}
	releaseRunTaskTurn(t, env, teamID, runID, "first", "rebind-mid")
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatalf("主动换绑后 Run 未完成: %v", env.runDetail(t, teamID, runID))
	}
	records := readTeamPrompts(t, dataDir)
	index := promptIndex(records, "REBIND-MID-PENDING")
	if len(records) != 2 || index < 0 || records[index].session != replacement {
		t.Fatalf("未派任务应只使用最新绑定会话: %v", records)
	}
	detail := env.runDetail(t, teamID, runID)
	if result := taskResultOf(t, teamTaskOf(t, detail, "pending")); result["sessionId"] != replacement {
		t.Fatalf("任务结果应归属接替会话: %v", result)
	}
	stored := env.store.Team(teamID)
	if stored.Members[1].SessionID != replacement || stored.Members[1].Role != team.RoleMember || stored.LeadID != "lead" {
		t.Fatalf("主动换绑不得改变成员身份或团队角色: %+v", stored)
	}
}

func TestTeamMemberRebindAndOfflineSurviveGatewayRestart(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	original := env.createMemberSession(t, "rev-original", "原评审")
	spare := env.createMemberSession(t, "spare-sess", "备用成员")
	replacement := env.createMemberSession(t, "rev-replacement", "接替评审")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"rev","sessionId":"`+original+`","role":"member"},{"id":"spare","sessionId":"`+spare+`","role":"member"}]`)
	response := env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"rev","version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("设置默认评审失败: %d %s", response.Code, response.Body.String())
	}
	runID := env.createRun(t, teamID, `{"goal":"换绑后重启恢复","mode":"pipeline","plan":[
		{"id":"review","memberId":"rev","prompt":"重启前评审 RESTART-REBIND QUOTAFAILFOR:`+original+`","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("额度任务未进入 waiting_input")
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/rev", `{"sessionId":"`+replacement+`","version":2}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重启前换绑失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/spare", `{"enabled":false,"version":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重启前下线备用成员失败: %d %s", response.Code, response.Body.String())
	}

	env2 := reopenTeamIntegrationEnv(t, dataDir)
	stored := env2.store.Team(teamID)
	if stored.Members[1].SessionID != replacement || !stored.Members[1].Enabled || stored.Members[2].Enabled || stored.ReviewerID != "rev" {
		t.Fatalf("重启后成员绑定、启停或团队角色丢失: %+v", stored)
	}
	if status := env2.runDetail(t, teamID, runID)["status"]; status != "waiting_input" {
		t.Fatalf("重启后 Run 应保持 waiting_input: %s", status)
	}
	env2.drive(t, 400*time.Millisecond, func() bool { return false })
	if records := readTeamPrompts(t, dataDir); len(records) != 1 {
		t.Fatalf("重启后的等待态 Run 不得自动重发: %v", records)
	}

	response = env2.request(http.MethodPatch, "/api/teams/"+teamID+"/members/spare", `{"enabled":true,"version":4}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重启后恢复成员上线失败: %d %s", response.Code, response.Body.String())
	}
	response = env2.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重启后恢复 Run 失败: %d %s", response.Code, response.Body.String())
	}
	if !env2.drive(t, teamIntegrationWait, func() bool {
		return env2.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatalf("重启后接替任务未完成: %v", env2.runDetail(t, teamID, runID))
	}
	records := readTeamPrompts(t, dataDir)
	if len(records) != 2 || records[0].session != original || records[1].session != replacement {
		t.Fatalf("重启前后应分别使用原会话和持久化的接替会话: %v", records)
	}
}

func TestTeamMemberConcurrentEditRequiresRefreshAndPreservesOtherChanges(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	lead := env.createMemberSession(t, "lead-sess", "主控")
	dev := env.createMemberSession(t, "dev-sess", "执行")
	rev := env.createMemberSession(t, "rev-sess", "评审")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+dev+`","role":"member"},{"id":"rev","sessionId":"`+rev+`","role":"member"}]`)

	response := env.request(http.MethodPatch, "/api/teams/"+teamID, `{"reviewerId":"rev","version":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("第一个客户端设置评审失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev", `{"name":"过期名称","version":1}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("持有旧版本的并发修改应返回 409: %d %s", response.Code, response.Body.String())
	}
	stored := env.store.Team(teamID)
	if stored.Version != 2 || stored.ReviewerID != "rev" || stored.Members[1].Name != "" {
		t.Fatalf("版本冲突不得覆盖其他客户端的修改: %+v", stored)
	}
	response = env.request(http.MethodPatch, "/api/teams/"+teamID+"/members/dev", `{"name":"刷新后名称","version":2}`)
	if response.Code != http.StatusOK {
		t.Fatalf("刷新后重试应成功: %d %s", response.Code, response.Body.String())
	}
	stored = env.store.Team(teamID)
	if stored.Version != 3 || stored.ReviewerID != "rev" || stored.Members[1].Name != "刷新后名称" {
		t.Fatalf("刷新后重试应同时保留两个客户端的修改: %+v", stored)
	}
}

func TestTeamMemberRebindRejectsCrossProjectAndDuplicateSession(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	original := env.createMemberSession(t, "dev-original", "原执行")
	other := env.createMemberSessionIn(t, "other-project", t.TempDir(), "其他项目会话")
	project := "acp:" + hashProject(dataDir)
	response := env.request(http.MethodPost, "/api/teams", `{"name":"项目团队","project":"`+project+`","members":[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+original+`","role":"member"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建项目团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	response = env.request(http.MethodPatch, "/api/teams/"+created.ID+"/members/dev", `{"sessionId":"`+other+`","version":1}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "项目空间不一致") {
		t.Fatalf("跨项目换绑应被拒绝: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPatch, "/api/teams/"+created.ID+"/members/dev", `{"sessionId":"`+lead+`","version":1}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "已被成员") {
		t.Fatalf("重复绑定其他成员会话应被拒绝: %d %s", response.Code, response.Body.String())
	}
	stored := env.store.Team(created.ID)
	if stored.Version != 1 || stored.Members[1].SessionID != original || stored.Members[1].Role != team.RoleMember || stored.LeadID != "lead" {
		t.Fatalf("非法换绑不得留下部分修改: %+v", stored)
	}
}
