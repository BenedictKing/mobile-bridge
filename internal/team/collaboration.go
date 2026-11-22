package team

import (
	"fmt"
	"strings"
)

const (
	CollaborationPatternCustomDAG   = "custom-dag"
	CollaborationPatternSingleAgent = "single-agent"
)

// RunCollaborationParticipant describes one member's observed role in a Run.
type RunCollaborationParticipant struct {
	MemberID    string   `json:"memberId"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	AgentKind   string   `json:"agentKind,omitempty"`
	ReportTo    []string `json:"reportTo"`
	ReceiveFrom []string `json:"receiveFrom"`
}

// RunCollaboration is a read-only team.md projection of a Run snapshot.
type RunCollaboration struct {
	SchemaVersion int                           `json:"schemaVersion"`
	Pattern       string                        `json:"pattern"`
	TaskGoal      string                        `json:"taskGoal"`
	Orchestrator  *RunCollaborationParticipant  `json:"orchestrator,omitempty"`
	Members       []RunCollaborationParticipant `json:"members"`
	Revision      uint64                        `json:"revision"`
	RawMarkdown   string                        `json:"rawMarkdown"`
}

// BuildRunCollaboration derives collaboration data exclusively from persisted facts.
func BuildRunCollaboration(run *Run, teamRow *Team) *RunCollaboration {
	if run == nil {
		return nil
	}
	members := map[string]TeamMember{}
	leadID := ""
	if teamRow != nil {
		leadID = teamRow.LeadID
		for _, member := range teamRow.Members {
			members[member.ID] = member
		}
	}
	participants := map[string]*RunCollaborationParticipant{}
	order := []string{}
	add := func(memberID string) *RunCollaborationParticipant {
		if memberID == "" {
			return nil
		}
		if existing := participants[memberID]; existing != nil {
			return existing
		}
		member := members[memberID]
		role := member.Role
		if memberID == leadID {
			role = RoleLead
		} else if role == "" {
			role = RoleMember
		}
		participant := &RunCollaborationParticipant{
			MemberID: memberID, Name: collaborationName(member), Role: role,
			AgentKind: member.AgentKind, ReportTo: []string{}, ReceiveFrom: []string{},
		}
		participants[memberID] = participant
		order = append(order, memberID)
		return participant
	}
	add(leadID)
	tasksByID := make(map[string]*Task, len(run.Tasks))
	for i := range run.Tasks {
		tasksByID[run.Tasks[i].ID] = &run.Tasks[i]
		if run.Tasks[i].MemberID != leadID {
			add(run.Tasks[i].MemberID)
		}
	}
	for _, id := range order {
		participant := participants[id]
		if participant.Name == "" {
			participant.Name = id
		}
	}
	seenNames := map[string]bool{}
	for _, id := range order {
		participant := participants[id]
		base := participant.Name
		candidate := base
		if seenNames[candidate] {
			candidate = fmt.Sprintf("%s (%s)", base, id)
		}
		for suffix := 2; seenNames[candidate]; suffix++ {
			candidate = fmt.Sprintf("%s (%s) #%d", base, id, suffix)
		}
		participant.Name = candidate
		seenNames[candidate] = true
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		receiver := participants[task.MemberID]
		if receiver == nil {
			continue
		}
		for _, dependencyID := range task.DependsOn {
			dependency := tasksByID[dependencyID]
			if dependency == nil || dependency.MemberID == task.MemberID {
				continue
			}
			sender := participants[dependency.MemberID]
			if sender == nil {
				continue
			}
			receiver.ReceiveFrom = appendCollaborationReference(receiver.ReceiveFrom, sender.MemberID)
			sender.ReportTo = appendCollaborationReference(sender.ReportTo, receiver.MemberID)
		}
	}
	collaboration := &RunCollaboration{
		SchemaVersion: 1, Pattern: CollaborationPatternSingleAgent, TaskGoal: run.Goal,
		Members: []RunCollaborationParticipant{}, Revision: run.Version,
	}
	if orchestrator := participants[leadID]; orchestrator != nil {
		collaboration.Orchestrator = orchestrator
	}
	for _, id := range order {
		if id == leadID {
			continue
		}
		collaboration.Members = append(collaboration.Members, *participants[id])
	}
	if len(order) > 1 {
		collaboration.Pattern = CollaborationPatternCustomDAG
	}
	collaboration.RawMarkdown = collaborationMarkdown(collaboration, participants)
	return collaboration
}

func collaborationName(member TeamMember) string {
	return collaborationInline(strings.TrimSpace(member.Name))
}

func collaborationInline(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func appendCollaborationReference(references []string, memberID string) []string {
	for _, existing := range references {
		if existing == memberID {
			return references
		}
	}
	return append(references, memberID)
}

func collaborationMarkdown(collaboration *RunCollaboration, participants map[string]*RunCollaborationParticipant) string {
	var markdown strings.Builder
	markdown.WriteString("---\ncollaborate_pattern: ")
	markdown.WriteString(collaboration.Pattern)
	markdown.WriteString("\n---\n\n## Task Goal\n\n")
	markdown.WriteString(strings.TrimSpace(collaboration.TaskGoal))
	markdown.WriteString("\n\n## Orchestrator\n\n")
	if collaboration.Orchestrator != nil {
		writeCollaborationEntry(&markdown, collaboration.Orchestrator, participants)
	}
	markdown.WriteString("\n## Team Member\n\n")
	for i := range collaboration.Members {
		writeCollaborationEntry(&markdown, &collaboration.Members[i], participants)
	}
	return markdown.String()
}

func writeCollaborationEntry(markdown *strings.Builder, participant *RunCollaborationParticipant, participants map[string]*RunCollaborationParticipant) {
	fmt.Fprintf(markdown, "- %s\n  - ID: %s\n  - Role: %s\n", participant.Name, collaborationInline(participant.MemberID), collaborationInline(participant.Role))
	fmt.Fprintf(markdown, "  - Report to: %s\n", collaborationReferenceNames(participant.ReportTo, participants))
	fmt.Fprintf(markdown, "  - Receive from: %s\n\n", collaborationReferenceNames(participant.ReceiveFrom, participants))
}

func collaborationReferenceNames(references []string, participants map[string]*RunCollaborationParticipant) string {
	if len(references) == 0 {
		return "none"
	}
	names := make([]string, 0, len(references))
	for _, memberID := range references {
		if participant := participants[memberID]; participant != nil {
			names = append(names, participant.Name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
