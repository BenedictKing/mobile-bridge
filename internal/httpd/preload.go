package httpd

import (
	"log"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	preloadRecentSessions  = 15
	preloadRecentProjects  = 5
	preloadProjectSessions = 3
)

type preloadSession struct {
	kind string
	id   string
	cwd  string
}

func selectPreloadSessions(rows *jsoncompat.Array) []preloadSession {
	if rows == nil {
		return nil
	}
	type candidate struct {
		preloadSession
		project string
	}
	candidates := make([]candidate, 0, len(rows.Items))
	for _, item := range rows.Items {
		row, _ := item.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		kind, _ := row.MustGet("agent").(string)
		id, _ := row.MustGet("id").(string)
		cwd, _ := row.MustGet("cwd").(string)
		if kind == "" || kind == "codex" || id == "" || cwd == "" {
			continue
		}
		project, _ := row.MustGet("projectKey").(string)
		candidates = append(candidates, candidate{preloadSession{kind, id, cwd}, project})
	}
	selected := []preloadSession{}
	seen := map[string]bool{}
	add := func(candidate candidate) {
		key := candidate.kind + "\x00" + candidate.id
		if seen[key] {
			return
		}
		seen[key] = true
		selected = append(selected, candidate.preloadSession)
	}
	for index, candidate := range candidates {
		if index >= preloadRecentSessions {
			break
		}
		add(candidate)
	}
	projects := []string{}
	projectRows := map[string][]candidate{}
	projectSeen := map[string]bool{}
	for _, candidate := range candidates {
		if !projectSeen[candidate.project] {
			projectSeen[candidate.project] = true
			projects = append(projects, candidate.project)
		}
		projectRows[candidate.project] = append(projectRows[candidate.project], candidate)
	}
	for index, project := range projects {
		if index >= preloadRecentProjects {
			break
		}
		rows := projectRows[project]
		if len(rows) > preloadProjectSessions {
			rows = rows[:preloadProjectSessions]
		}
		for _, candidate := range rows {
			add(candidate)
		}
	}
	return selected
}

// PreloadRecentSessions warms ACP history in the background after startup.
// Codex already has a store-backed history view and is deliberately not attached.
func (s *Server) PreloadRecentSessions() {
	h := &handler{server: s}
	result, err := h.sessionList(map[string][]string{})
	if err != nil {
		log.Printf("会话预热: 读取会话列表失败: %v", err)
		return
	}
	rows, _ := result.MustGet("sessions").(*jsoncompat.Array)
	targets := selectPreloadSessions(rows)
	loaded := 0
	for _, target := range targets {
		manager := s.acpAgentSet()[target.kind]
		if manager == nil {
			continue
		}
		if _, err := manager.Open(target.id, target.cwd, manager.InitializeTimeout); err != nil {
			log.Printf("会话预热: %s/%s 加载失败: %v", target.kind, target.id, err)
			continue
		}
		loaded++
	}
	log.Printf("会话预热: %d/%d 个 ACP 会话已加载", loaded, len(targets))
}
