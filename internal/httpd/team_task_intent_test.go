package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestTeamInputsTaskIntentPreservedValidatedAndIdempotent(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	env.server.TeamInputs = team.LoadInputLedger(filepath.Join(env.dataDir, "inputs.json"), nil)
	const id = "9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := fmt.Sprintf(`{"id":%q,"text":"已完善并落盘实施计划","intent":"task"}`, id)
	response := env.postTeamInput(teamID, body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("任务派发应受理: %d %s", response.Code, response.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["intent"] != "task" || accepted["issueRouting"] != true {
		t.Fatalf("受理结果必须保留任务意图: %v", accepted)
	}
	input := env.server.TeamInputs.Get(teamID, id)
	if input == nil || input.Intent != team.InputIntentTask || input.Text != "已完善并落盘实施计划" {
		t.Fatalf("正文与任务意图必须分别持久化: %+v", input)
	}
	if replay := env.postTeamInput(teamID, body); replay.Code != http.StatusAccepted {
		t.Fatalf("相同任务重试应幂等: %d %s", replay.Code, replay.Body.String())
	}
	changed := env.postTeamInput(teamID, fmt.Sprintf(`{"id":%q,"text":"已完善并落盘实施计划","teamIssueRoutingVersion":"1"}`, id))
	if changed.Code != http.StatusConflict {
		t.Fatalf("同 ID 不能把执行请求变为普通消息: %d %s", changed.Code, changed.Body.String())
	}
	project := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+id, "")
	if project.Code != http.StatusOK || !json.Valid(project.Body.Bytes()) {
		t.Fatalf("任务意图应可回查: %d %s", project.Code, project.Body.String())
	}
	var projected map[string]any
	if err := json.Unmarshal(project.Body.Bytes(), &projected); err != nil || projected["intent"] != "task" {
		t.Fatalf("回查不得丢执行意图: %v %v", projected, err)
	}
	for _, intent := range []string{`"unknown"`, `true`, `42`, `null`} {
		bad := env.postTeamInput(teamID, fmt.Sprintf(`{"id":"8bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"任务","intent":%s}`, intent))
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("非法意图 %s 应明确拒绝: %d %s", intent, bad.Code, bad.Body.String())
		}
	}
}

func TestTeamTaskReportFlowsThroughHTTPACPRepairExecutionAndDelivery(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	lead := env.createMemberSession(t, "report-lead", "计划任务主控")
	dev := env.createMemberSession(t, "report-dev", "计划执行成员")
	teamID := env.createTeamWithPolicy(t, `{"autoDispatch":true}`, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"},{"id":"dev","sessionId":"`+dev+`","role":"member"}]`)
	const id = "7bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	writeAgentScriptFile(t, env.dataDir, "actions-REPORT.txt", fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"ignore","source":"full"}]}</team-issues>`, id))
	writeAgentScriptFile(t, env.dataDir, "actionsrepair-1.txt", fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"create","newIssueTitle":"落实计划","source":"full"}]}</team-issues><team-actions>{"actions":[{"type":"delegate","sourceTaskId":"actfix-lead-1","round":1,"member":"dev","task":"读取计划并实现 REPORT-IMPLEMENT"}]}</team-actions>`, id))
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/inputs", fmt.Sprintf(`{"id":%q,"text":"REPLYACTIONS:REPORT 已完善并落盘实施计划","intent":"task"}`, id))
	if response.Code != http.StatusAccepted {
		t.Fatalf("HTTP 任务派发未受理: %d %s", response.Code, response.Body.String())
	}
	if routed := env.server.Teams.RoutePendingInputs(teamID); routed != 1 {
		t.Fatalf("任务输入应路由并进入执行: %d", routed)
	}
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, id)["status"] == "completed"
	}) {
		t.Fatal("报告式正文经过纠错后未完成实际执行和交付")
	}
	detail := env.runDetail(t, teamID, id)
	if task := teamTaskOf(t, detail, "actfix-lead-1-d1-0"); task["status"] != "completed" || task["memberId"] != "dev" {
		t.Fatalf("必须派发并完成成员任务: %v", task)
	}
	if task := teamTaskOf(t, detail, "lead-auto-reduce"); task["status"] != "completed" || task["result"] == nil {
		t.Fatalf("必须返回控制会话交付结果: %v", task)
	}
	if input := env.server.TeamInputs.Get(teamID, id); input.Intent != team.InputIntentTask || input.AssociationStatus != team.IssueAssociationLinked {
		t.Fatalf("原任务意图和正式事项不能丢失: %+v", input)
	}
	if count := teamEventCount(env.journalEvents(t), team.EventInvalidActionBlock); count != 1 {
		t.Fatalf("只返回 ignore 块应触发一次纠错: %d", count)
	}
}
