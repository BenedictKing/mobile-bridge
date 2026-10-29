package httpd

import (
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

type projectAgentCandidate struct {
	Agent     string
	SessionID string
	Title     string
	Cwd       string
	UpdatedAt int64
}

func (h *handler) handleProjectTeamRoute(path string) {
	switch {
	case strings.HasSuffix(path, "/team") && h.request.Method == http.MethodPut:
		h.handleDefaultTeamPut(strings.TrimSuffix(strings.TrimPrefix(path, "/api/projects/"), "/team"))
	case strings.HasSuffix(path, "/agents") && h.request.Method == http.MethodGet:
		h.handleProjectAgents(strings.TrimSuffix(strings.TrimPrefix(path, "/api/projects/"), "/agents"))
	default:
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
	}
}

func (h *handler) handleProjectAgents(projectKey string) {
	cwd := h.resolveProjectCwd(projectKey)
	if cwd == "" {
		h.fail(http.StatusNotFound, errorBody("项目空间不存在"))
		return
	}
	candidates := h.projectAgentCandidates(cwd)
	rows := &jsoncompat.Array{}
	for _, candidate := range candidates {
		row := jsoncompat.NewObject()
		row.Set("agent", candidate.Agent)
		row.Set("sessionId", candidate.SessionID)
		row.Set("title", candidate.Title)
		row.Set("cwd", candidate.Cwd)
		_, available := h.server.teamMemberKind(candidate.SessionID)
		row.Set("available", available)
		rows.Items = append(rows.Items, row)
	}
	result := jsoncompat.NewObject()
	result.Set("agents", rows)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) projectAgentCandidates(cwd string) []projectAgentCandidate {
	candidates := []projectAgentCandidate{}
	agents := h.server.acpAgentSet()
	kinds := make([]string, 0, len(agents))
	for kind := range agents {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil {
			continue
		}
		rows, err := manager.List(15 * time.Second)
		if err != nil {
			continue
		}
		for _, row := range rows {
			rowCwd, _ := row.MustGet("cwd").(string)
			if rowCwd == "" || filepath.Clean(rowCwd) != filepath.Clean(cwd) {
				continue
			}
			sessionID, _ := row.MustGet("sessionId").(string)
			title, _ := row.MustGet("title").(string)
			candidates = append(candidates, projectAgentCandidate{
				Agent: kind, SessionID: sessionID, Title: title, Cwd: rowCwd,
				UpdatedAt: rowMillis(row, "updatedAt"),
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].UpdatedAt != candidates[j].UpdatedAt {
			return candidates[i].UpdatedAt > candidates[j].UpdatedAt
		}
		return candidates[i].Agent < candidates[j].Agent
	})
	unique := make([]projectAgentCandidate, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate.Agent] {
			continue
		}
		seen[candidate.Agent] = true
		unique = append(unique, candidate)
	}
	return unique
}

func (h *handler) handleDefaultTeamPut(projectKey string) {
	cwd := h.resolveProjectCwd(projectKey)
	if cwd == "" {
		h.fail(http.StatusBadRequest, errorBody("团队的项目空间无效，请刷新项目列表"))
		return
	}
	for _, existing := range h.server.TeamStore.Teams() {
		if existing.Project == projectKey && existing.DefaultForProject {
			h.outputDefaultTeam(existing, false)
			return
		}
	}
	for _, existing := range h.server.TeamStore.Teams() {
		if existing.Project == projectKey {
			h.outputDefaultTeam(existing, false)
			return
		}
	}
	candidates := h.projectAgentCandidates(cwd)
	if len(candidates) == 0 {
		h.fail(http.StatusConflict, errorBody("项目中还没有可用的 ACP Agent，请先在该项目使用一个 Agent"))
		return
	}
	teamID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:default-team:"+projectKey)).String()
	name := filepath.Base(filepath.Clean(cwd)) + "团队"
	policy := team.DefaultPolicy()
	policy.AutoDispatch = true
	members := make([]team.TeamMember, 0, len(candidates))
	for index, candidate := range candidates {
		entry := jsoncompat.NewObject()
		memberID := "member-" + strconv.Itoa(index+1)
		entry.Set("id", memberID)
		entry.Set("project", projectKey)
		entry.Set("agent", candidate.Agent)
		entry.Set("role", team.RoleMember)
		if index == 0 {
			entry.Set("role", team.RoleLead)
		}
		entry.Set("name", candidate.Agent)
		sessionID, status, err := h.createTeamProjectSession(entry, name, teamID)
		if err != nil {
			h.fail(status, errorBody(err.Error()))
			return
		}
		entry.Set("sessionId", sessionID)
		member, err := h.parseTeamMember(entry, index)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		members = append(members, member)
	}
	now := time.Now()
	created := &team.Team{
		ID: teamID, Name: name, Project: projectKey, DefaultForProject: true,
		Members: members, LeadID: members[0].ID, Policy: policy, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.server.TeamStore.SaveTeam(created); err != nil {
		if existing := h.server.TeamStore.Team(teamID); existing != nil {
			h.outputDefaultTeam(existing, false)
			return
		}
		h.failTeamStoreError(err)
		return
	}
	h.outputDefaultTeam(created, true)
}

func (h *handler) outputDefaultTeam(teamRow *team.Team, created bool) {
	result := h.teamProject(teamRow)
	result.Set("created", created)
	if !created {
		result.Set("duplicate", true)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}
