package team

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestBuildRunCollaborationProjectsObservedDAG(t *testing.T) {
	teamRow := &Team{LeadID: "lead", Members: []TeamMember{
		{ID: "lead", Name: "Coordinator", Role: RoleLead, AgentKind: "kimi"},
		{ID: "dev", Name: "Researcher", Role: RoleMember, AgentKind: "zcode"},
		{ID: "reviewer", Name: "Researcher", Role: RoleReviewer, AgentKind: "claude"},
		{ID: "unused", Name: "Unused", Role: RoleMember},
	}}
	run := &Run{Goal: "Compare vendors", Version: 7, Tasks: []Task{
		{ID: "plan", MemberID: "lead"},
		{ID: "research", MemberID: "dev", DependsOn: []string{"plan"}},
		{ID: "review", MemberID: "reviewer", DependsOn: []string{"research"}},
		{ID: "summary", MemberID: "lead", DependsOn: []string{"research", "review"}},
	}}
	collaboration := BuildRunCollaboration(run, teamRow)
	if collaboration == nil || collaboration.SchemaVersion != 1 || collaboration.Pattern != CollaborationPatternCustomDAG || collaboration.Revision != 7 {
		t.Fatalf("unexpected protocol metadata: %+v", collaboration)
	}
	if collaboration.TaskGoal != run.Goal || collaboration.Orchestrator == nil || collaboration.Orchestrator.MemberID != "lead" {
		t.Fatalf("goal and orchestrator must come from the snapshot: %+v", collaboration)
	}
	if len(collaboration.Members) != 2 || collaboration.Members[0].MemberID != "dev" || collaboration.Members[1].MemberID != "reviewer" {
		t.Fatalf("only actual non-lead participants belong in the protocol: %+v", collaboration.Members)
	}
	lead := collaboration.Orchestrator
	if strings.Join(lead.ReceiveFrom, ",") != "dev,reviewer" || strings.Join(lead.ReportTo, ",") != "dev" {
		t.Fatalf("lead relationships must follow direct dependencies: %+v", lead)
	}
	dev := collaboration.Members[0]
	if strings.Join(dev.ReceiveFrom, ",") != "lead" || strings.Join(dev.ReportTo, ",") != "reviewer,lead" {
		t.Fatalf("member relationships must follow both dependency directions: %+v", dev)
	}
	if collaboration.Members[0].Name != "Researcher" || collaboration.Members[1].Name != "Researcher (reviewer)" {
		t.Fatalf("duplicate display names must be disambiguated: %+v", collaboration.Members)
	}
	for _, fragment := range []string{
		"collaborate_pattern: custom-dag", "## Task Goal\n\nCompare vendors",
		"- Researcher (reviewer)\n  - ID: reviewer", "Report to: Coordinator", "Receive from: Researcher",
	} {
		if !strings.Contains(collaboration.RawMarkdown, fragment) {
			t.Fatalf("raw Markdown missing %q:\n%s", fragment, collaboration.RawMarkdown)
		}
	}
	if strings.Contains(collaboration.RawMarkdown, "Unused") {
		t.Fatalf("unused team members must not enter the protocol:\n%s", collaboration.RawMarkdown)
	}
}

func TestBuildRunCollaborationIgnoresInvalidReferencesAndDeduplicatesEdges(t *testing.T) {
	teamRow := &Team{LeadID: "lead", Members: []TeamMember{{ID: "lead", Role: RoleLead}, {ID: "dev", Role: RoleMember}}}
	run := &Run{Tasks: []Task{
		{ID: "plan", MemberID: "lead"},
		{ID: "research-a", MemberID: "dev", DependsOn: []string{"plan", "plan", "research-a", "missing"}},
		{ID: "research-b", MemberID: "dev", DependsOn: []string{"plan"}},
	}}
	collaboration := BuildRunCollaboration(run, teamRow)
	dev := collaboration.Members[0]
	if len(dev.ReceiveFrom) != 1 || dev.ReceiveFrom[0] != "lead" || len(collaboration.Orchestrator.ReportTo) != 1 {
		t.Fatalf("references must be known, non-self and unique: %+v", collaboration)
	}
	if collaboration.Orchestrator.Name != "lead" || dev.Name != "dev" {
		t.Fatalf("missing display names must fall back to member IDs: %+v", collaboration)
	}
}

func TestBuildRunCollaborationSingleAgentAndMissingLead(t *testing.T) {
	single := BuildRunCollaboration(&Run{Tasks: []Task{{ID: "plan", MemberID: "lead"}}}, &Team{LeadID: "lead"})
	if single.Pattern != CollaborationPatternSingleAgent || len(single.Members) != 0 {
		t.Fatalf("a lead-only Run must remain single-agent: %+v", single)
	}
	if single.Orchestrator.ReportTo == nil || single.Orchestrator.ReceiveFrom == nil || single.Members == nil {
		t.Fatalf("protocol arrays must be empty arrays rather than null: %+v", single)
	}
	withoutLead := BuildRunCollaboration(&Run{Tasks: []Task{{ID: "work", MemberID: "dev"}}}, nil)
	if withoutLead.Orchestrator != nil || withoutLead.Pattern != CollaborationPatternSingleAgent || len(withoutLead.Members) != 1 {
		t.Fatalf("missing lead must preserve known members without inventing an orchestrator: %+v", withoutLead)
	}
	if got := BuildRunCollaboration(nil, nil); got != nil {
		t.Fatalf("nil Run must not produce a protocol: %+v", got)
	}
}

func TestBuildRunCollaborationSanitizesMarkdownFields(t *testing.T) {
	teamRow := &Team{LeadID: "lead", Members: []TeamMember{
		{ID: "lead", Name: "Lead\nInjected", Role: RoleLead},
		{ID: "dev", Name: "Dev\r\nMember", Role: RoleMember},
	}}
	collaboration := BuildRunCollaboration(&Run{Goal: "Goal", Tasks: []Task{
		{ID: "plan", MemberID: "lead"},
		{ID: "work", MemberID: "dev", DependsOn: []string{"plan"}},
	}}, teamRow)
	if strings.Contains(collaboration.RawMarkdown, "Lead\nInjected") || strings.Contains(collaboration.RawMarkdown, "Dev\r\nMember") {
		t.Fatalf("participant fields must stay on one Markdown line:\n%s", collaboration.RawMarkdown)
	}
	if !strings.Contains(collaboration.RawMarkdown, "Lead Injected") || !strings.Contains(collaboration.RawMarkdown, "Dev Member") {
		t.Fatalf("sanitized names must remain readable:\n%s", collaboration.RawMarkdown)
	}
}

func TestBuildRunCollaborationUsesDeclaredPattern(t *testing.T) {
	teamRow := &Team{LeadID: "lead", Members: []TeamMember{{ID: "lead", Role: RoleLead}, {ID: "dev", Role: RoleMember}}}
	run := &Run{CollaborationPattern: CollaborationPatternFanOutAndSynthesize, Tasks: []Task{
		{ID: "plan", MemberID: "lead"},
		{ID: "work", MemberID: "dev", DependsOn: []string{"plan"}},
	}}
	collaboration := BuildRunCollaboration(run, teamRow)
	if collaboration.Pattern != CollaborationPatternFanOutAndSynthesize || collaboration.PatternSource != CollaborationPatternSourceLead {
		t.Fatalf("valid Lead declaration must win: %+v", collaboration)
	}
	if !strings.Contains(collaboration.RawMarkdown, "collaborate_pattern: fan-out-and-synthesize") {
		t.Fatalf("declaration must reach raw Markdown:\n%s", collaboration.RawMarkdown)
	}
	run.CollaborationPattern = "invalid"
	collaboration = BuildRunCollaboration(run, teamRow)
	if collaboration.Pattern != CollaborationPatternCustomDAG || collaboration.PatternSource != CollaborationPatternSourceObserved {
		t.Fatalf("invalid persisted values must fall back to observation: %+v", collaboration)
	}
}

func TestRunCollaborationPatternPersistenceAndJournalRebuild(t *testing.T) {
	run := &Run{ID: "run-pattern", CollaborationPattern: CollaborationPatternFanOutAndSynthesize}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Run
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CollaborationPattern != run.CollaborationPattern || decoded.Clone().CollaborationPattern != run.CollaborationPattern {
		t.Fatalf("declaration must survive JSON and Clone: %+v", decoded)
	}
	for _, pattern := range []string{CollaborationPatternTournament, CollaborationPatternLoopUntilDone, "custom-dag"} {
		applyJournalEventToRun(&decoded, &Event{Type: EventRunCollaborationPattern, Payload: map[string]any{"collaborationPattern": pattern}})
	}
	if decoded.CollaborationPattern != CollaborationPatternLoopUntilDone {
		t.Fatalf("journal rebuild must keep the latest valid declaration: %q", decoded.CollaborationPattern)
	}
}

func collaborationTurn(text string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-pattern")
	turn.Set("status", "completed")
	turn.Set("stopReason", "end_turn")
	message := jsoncompat.NewObject()
	message.Set("role", "assistant")
	message.Set("text", text)
	messages := &jsoncompat.Array{Items: []any{message}}
	turn.Set("messages", messages)
	return turn
}

func TestSettleLeadTaskPersistsCollaborationPattern(t *testing.T) {
	h := newActionsExecHarness(t)
	run := &Run{ID: "run-ex", TeamID: h.team.ID, Goal: "协作", AutoPlan: true, Status: RunDispatching, Tasks: []Task{
		{ID: "lead", MemberID: "lead", Status: TaskRunning},
	}}
	state := h.saveExecRun(t, run)
	dispatch := &taskDispatch{taskID: "lead", sessionID: "s-lead", submissionID: "sub-lead"}
	settle := func(pattern string) bool {
		text := "规划说明\n<team-actions>{\"collaborationPattern\":\"" + pattern + "\",\"actions\":[]}</team-actions>"
		return h.service.settleLeadTaskLocked(state, dispatch, h.team.Policy, collaborationTurn(text), &TaskResult{SessionID: "s-lead", TurnID: "turn-pattern", Result: "规划说明"})
	}
	if !settle(CollaborationPatternFanOutAndSynthesize) {
		t.Fatal("valid declaration must settle")
	}
	fresh := h.store.Run(run.ID)
	if fresh.CollaborationPattern != CollaborationPatternFanOutAndSynthesize || len(h.eventsOf(t, EventRunCollaborationPattern)) != 1 {
		t.Fatalf("first declaration must persist once: %+v", fresh)
	}
	fresh.Tasks = append(fresh.Tasks, Task{ID: "lead-next", MemberID: "lead", Status: TaskRunning})
	if err := h.store.SaveRun(fresh); err != nil {
		t.Fatal(err)
	}
	dispatch.taskID = "lead-next"
	if !settle(CollaborationPatternFanOutAndSynthesize) || len(h.eventsOf(t, EventRunCollaborationPattern)) != 1 {
		t.Fatal("identical declaration must not append another event")
	}
	fresh = h.store.Run(run.ID)
	fresh.Tasks = append(fresh.Tasks, Task{ID: "lead-final", MemberID: "lead", Status: TaskRunning})
	if err := h.store.SaveRun(fresh); err != nil {
		t.Fatal(err)
	}
	dispatch.taskID = "lead-final"
	if !settle(CollaborationPatternTournament) {
		t.Fatal("later declaration must settle")
	}
	fresh = h.store.Run(run.ID)
	if fresh.CollaborationPattern != CollaborationPatternTournament || len(h.eventsOf(t, EventRunCollaborationPattern)) != 2 {
		t.Fatalf("latest declaration must win with one event per change: %+v", fresh)
	}
	protocol := BuildRunCollaboration(fresh, h.team)
	if protocol.Pattern != CollaborationPatternTournament || protocol.PatternSource != CollaborationPatternSourceLead {
		t.Fatalf("settled declaration must reach RunDetail projection: %+v", protocol)
	}
}

func TestSettleLeadTaskRejectsInvalidCollaborationPattern(t *testing.T) {
	h := newActionsExecHarness(t)
	run := &Run{ID: "run-ex", TeamID: h.team.ID, AutoPlan: true, Status: RunDispatching, Tasks: []Task{
		{ID: "lead", MemberID: "lead", Status: TaskRunning},
	}}
	state := h.saveExecRun(t, run)
	text := "规划说明\n<team-actions>{\"collaborationPattern\":\"custom-dag\",\"actions\":[]}</team-actions>"
	if !h.service.settleLeadTaskLocked(state, &taskDispatch{taskID: "lead"}, h.team.Policy, collaborationTurn(text), &TaskResult{Result: "规划说明"}) {
		t.Fatal("invalid declaration must enter the existing repair path")
	}
	fresh := h.store.Run(run.ID)
	if fresh.CollaborationPattern != "" || len(h.eventsOf(t, EventRunCollaborationPattern)) != 0 {
		t.Fatalf("invalid declaration must not persist: %+v", fresh)
	}
	if findTaskIn(fresh, "actfix-lead-1") == nil {
		t.Fatalf("invalid declaration must create a bounded repair task: %+v", fresh.Tasks)
	}
}
