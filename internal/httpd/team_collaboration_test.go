package httpd

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestRunDetailIncludesReadOnlyCollaborationProtocol(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	teamRow := env.store.Team(teamID)
	teamRow.Members = append(teamRow.Members, team.TeamMember{
		ID: "dev", SessionID: "sess-dev", AgentKind: "kimi", Name: "执行者", Role: team.RoleMember, Enabled: true,
	})
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{ID: "collaboration-run", TeamID: teamID, Goal: "生成协作协议", Status: team.RunDispatching, Tasks: []team.Task{
		{ID: "plan", MemberID: "lead", Status: team.TaskCompleted},
		{ID: "work", MemberID: "dev", Status: team.TaskRunning, DependsOn: []string{"plan"}},
		{ID: "summary", MemberID: "lead", Status: team.TaskPlanned, DependsOn: []string{"work"}},
	}}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	before, err := env.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+run.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 RunDetail 失败: %d %s", response.Code, response.Body.String())
	}
	var detail struct {
		Version       uint64                `json:"version"`
		Collaboration team.RunCollaboration `json:"collaboration"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatalf("RunDetail 解析失败: %v\n%s", err, response.Body.String())
	}
	protocol := detail.Collaboration
	if protocol.SchemaVersion != 1 || protocol.Pattern != team.CollaborationPatternCustomDAG || protocol.Revision != detail.Version {
		t.Fatalf("协议元数据与 RunDetail 不一致: %+v", protocol)
	}
	if protocol.Orchestrator == nil || protocol.Orchestrator.MemberID != "lead" || len(protocol.Members) != 1 || protocol.Members[0].MemberID != "dev" {
		t.Fatalf("协议参与者不正确: %+v", protocol)
	}
	if len(protocol.Members[0].ReceiveFrom) != 1 || protocol.Members[0].ReceiveFrom[0] != "lead" || len(protocol.Members[0].ReportTo) != 1 || protocol.Members[0].ReportTo[0] != "lead" {
		t.Fatalf("协议关系没有经过 HTTP JSON 完整保留: %+v", protocol.Members[0])
	}
	if protocol.RawMarkdown == "" {
		t.Fatal("RunDetail 必须同时提供 team.md 原文")
	}
	after, err := env.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("只读协议不得写入 journal: before=%d after=%d", len(before), len(after))
	}
}
