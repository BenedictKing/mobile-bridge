package team

import (
	"strings"
	"testing"
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
