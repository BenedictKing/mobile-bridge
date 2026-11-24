package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func getProjects(t *testing.T, server *Server) *jsoncompat.Object {
	t.Helper()
	token, session, err := server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/projects", nil)
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("projects status=%d body=%s", response.Code, response.Body.String())
	}
	decoded, err := jsoncompat.Unmarshal(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	object, ok := decoded.(*jsoncompat.Object)
	if !ok {
		t.Fatalf("projects body type=%T", decoded)
	}
	return object
}

func projectCwds(list *jsoncompat.Object) []string {
	projects, _ := list.MustGet("projects").(*jsoncompat.Array)
	cwds := []string{}
	for _, item := range projects.Items {
		project, _ := item.(*jsoncompat.Object)
		cwd, _ := project.MustGet("cwd").(string)
		cwds = append(cwds, cwd)
	}
	return cwds
}

func TestMigrationProjectsQueryOnlyTargetAgent(t *testing.T) {
	for _, target := range []string{"codex", "claude", "zcode"} {
		t.Run(target, func(t *testing.T) {
			server, _ := discoveryFixture(t)
			calls := map[string]int{}
			server.ACPAgents = map[string]*acpruntime.Manager{}
			for _, kind := range []string{"claude", "zcode"} {
				manager := acpruntime.New("")
				manager.NativeListExclusive = true
				manager.NativeList = func() []*jsoncompat.Object {
					calls[kind]++
					row := jsoncompat.NewObject()
					row.Set("sessionId", kind+"-session")
					row.Set("cwd", "/work")
					return []*jsoncompat.Object{row}
				}
				server.ACPAgents[kind] = manager
			}
			projects := (&handler{server: server}).targetProjects(target)
			if len(projects) != 1 || stringField(projects[0], "agent") != target {
				t.Fatalf("target %s projects: %v", target, projects)
			}
			prefix := "acp:"
			if target == "codex" {
				prefix = "codex:"
			}
			if key := stringField(projects[0], "key"); key != prefix+hashProject("/work") {
				t.Fatalf("target project key: %q", key)
			}
			for kind, count := range calls {
				if kind != target && count != 0 {
					t.Fatalf("queried unrelated agent %s %d times", kind, count)
				}
			}
		})
	}
}

func TestProjectsSortedByRecency(t *testing.T) {
	server, db := discoveryFixture(t)
	for _, row := range [][2]any{{"/b", 300}, {"/c", 200}} {
		if _, err := db.Exec(`INSERT INTO threads
			(id,name,title,cwd,updated_at,archived,originator,source,rollout_path)
			VALUES (?,?,?,?,?,0,'Codex Desktop','desktop','')`,
			fmt.Sprintf("s-%d", row[1]), row[0], row[0], row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	cwds := projectCwds(getProjects(t, server))
	want := []string{"/b", "/c", "/work"}
	if fmt.Sprint(cwds) != fmt.Sprint(want) {
		t.Fatalf("order=%v want=%v", cwds, want)
	}
}

func TestProjectsLimitKeepsResolvable(t *testing.T) {
	server, db := discoveryFixture(t)
	for i := 0; i < projectListLimit+5; i++ {
		if _, err := db.Exec(`INSERT INTO threads
			(id,name,title,cwd,updated_at,archived,originator,source,rollout_path)
			VALUES (?,?,?,?,?,0,'Codex Desktop','desktop','')`,
			fmt.Sprintf("extra-%d", i), fmt.Sprintf("/p%d", i), fmt.Sprintf("/p%d", i),
			fmt.Sprintf("/p%d", i), 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	cwds := projectCwds(getProjects(t, server))
	if len(cwds) != projectListLimit {
		t.Fatalf("returned=%d want=%d", len(cwds), projectListLimit)
	}
	// 最旧的 /work（updated_at=20）被截断，但按 key 仍可解析。
	h := &handler{server: server}
	if cwd := h.resolveProjectCwd("codex:" + hashProject("/work")); cwd != "/work" {
		t.Fatalf("resolveProjectCwd(/work)=%q", cwd)
	}
}

func TestProjectsIncludesSavedProjectWithoutSessions(t *testing.T) {
	server, db := discoveryFixture(t)
	server.CodexHome = writeState(t, `{
	  "local-projects": {
	    "saved": {"name": "New project", "rootPaths": ["/new-project/"]}
	  }
	}`)
	for i := 0; i < projectListLimit+5; i++ {
		if _, err := db.Exec(`INSERT INTO threads
			(id,name,title,cwd,updated_at,archived,originator,source,rollout_path)
			VALUES (?,?,?,?,?,0,'Codex Desktop','desktop','')`,
			fmt.Sprintf("saved-extra-%d", i), fmt.Sprintf("/saved-p%d", i), fmt.Sprintf("/saved-p%d", i),
			fmt.Sprintf("/saved-p%d", i), 1000+i); err != nil {
			t.Fatal(err)
		}
	}

	manager := acpruntime.NewCommand(fakeACPAgent(t, "/work"))
	defer manager.Close()
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}

	projects, _ := getProjects(t, server).MustGet("projects").(*jsoncompat.Array)
	if len(projects.Items) != projectListLimit+1 {
		t.Fatalf("returned=%d want=%d", len(projects.Items), projectListLimit+1)
	}
	saved, _ := projects.Items[len(projects.Items)-1].(*jsoncompat.Object)
	if cwd, _ := saved.MustGet("cwd").(string); cwd != "/new-project" {
		t.Fatalf("saved project cwd=%q", cwd)
	}
	if name, _ := saved.MustGet("name").(string); name != "New project" {
		t.Fatalf("saved project name=%q", name)
	}
	key := "codex:" + hashProject("/new-project")
	if actual, _ := saved.MustGet("key").(string); actual != key {
		t.Fatalf("saved project key=%q want=%q", actual, key)
	}
	h := &handler{server: server}
	if cwd := h.resolveProjectCwd(key); cwd != "/new-project" {
		t.Fatalf("resolveProjectCwd(saved)=%q", cwd)
	}

	token, session, err := server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/sessions",
		strings.NewReader(`{"agent":"kimi","project":"`+key+`","title":"新项目","id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create saved project status=%d body=%s", response.Code, response.Body.String())
	}
	created, err := jsoncompat.Unmarshal(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if cwd, _ := created.(*jsoncompat.Object).MustGet("cwd").(string); cwd != "/new-project" {
		t.Fatalf("created cwd=%q", cwd)
	}
}

func TestProjectsDeduplicatesAcrossAgents(t *testing.T) {
	server, _ := discoveryFixture(t)
	manager := acpruntime.NewCommand(fakeACPAgent(t, "/work"))
	defer manager.Close()
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}

	projects, _ := getProjects(t, server).MustGet("projects").(*jsoncompat.Array)
	if len(projects.Items) != 1 {
		t.Fatalf("同目录跨 Agent 应只返回一项，实际 %d 项", len(projects.Items))
	}
	project, _ := projects.Items[0].(*jsoncompat.Object)
	if agent, _ := project.MustGet("agent").(string); agent != "codex" {
		t.Fatalf("应保留最近活跃的 Codex 项目，实际 agent=%q", agent)
	}
	h := &handler{server: server}
	if cwd := h.resolveProjectCwd("acp:" + hashProject("/work")); cwd != "/work" {
		t.Fatalf("旧 ACP 项目键无法解析：%q", cwd)
	}
}

func TestProjectsDeleteHidesAllAliasesAndPersists(t *testing.T) {
	server, _ := discoveryFixture(t)
	manager := acpruntime.NewCommand(fakeACPAgent(t, "/work"))
	defer manager.Close()
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	ledgerPath := filepath.Join(t.TempDir(), "hidden-projects.json")
	server.HiddenProjects = LoadHiddenProjects(ledgerPath)
	key := "acp:" + hashProject("/work")
	if cwd := (&handler{server: server}).resolveProjectCwd(key); cwd != "/work" {
		t.Fatalf("test setup project unresolved: %q", cwd)
	}
	token, session, err := server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	remove := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8790"+target, strings.NewReader("{}"))
		request.Host = "127.0.0.1:8790"
		request.Header.Set("Origin", "http://127.0.0.1:8790")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	response := remove("/api/projects/" + key)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status": "hidden"`) {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	if projects, _ := getProjects(t, server).MustGet("projects").(*jsoncompat.Array); len(projects.Items) != 0 {
		t.Fatalf("hidden project still listed: %s", jsoncompat.EncodeCompact(projects))
	}
	if cwd := (&handler{server: server}).resolveProjectCwd("codex:" + hashProject("/work")); cwd != "" {
		t.Fatalf("hidden codex alias still resolves: %q", cwd)
	}
	if !LoadHiddenProjects(ledgerPath).hidden("/work") {
		t.Fatal("hidden project did not survive reload")
	}
	if response := remove("/api/projects/" + key); response.Code != http.StatusOK {
		t.Fatalf("idempotent delete status=%d body=%s", response.Code, response.Body.String())
	}
	if response := remove("/api/projects/acp:000000000000"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown project status=%d body=%s", response.Code, response.Body.String())
	}
	if response := remove("/api/projects/session-group?cwd=%2Fold-project"); response.Code != http.StatusOK {
		t.Fatalf("cwd delete status=%d body=%s", response.Code, response.Body.String())
	}
	if !server.HiddenProjects.hidden("/old-project") {
		t.Fatal("cwd delete did not hide the old local project")
	}
}

func TestRecentProjectEntriesDedupesByCwd(t *testing.T) {
	entries := []*projectEntry{
		newProjectEntry("codex:shared", "/shared", "codex", 400),
		newProjectEntry("acp:shared", "/shared", "kimi", 300),
		newProjectEntry("acp:other", "/other", "kimi", 200),
	}
	got := recentProjectEntries(entries, 2)
	if len(got) != 2 || got[0] != entries[0] || got[1] != entries[2] {
		t.Fatalf("去重结果=%v，应保留最近活跃的同目录项并在去重后应用上限", got)
	}
}

func TestRowMillis(t *testing.T) {
	row := jsoncompat.NewObject()
	row.Set("updated_at", json.Number("20"))
	if got := rowMillis(row, "updated_at_ms", "updated_at"); got != 20000 {
		t.Fatalf("seconds field=%d", got)
	}
	row.Set("updated_at_ms", json.Number("20500"))
	if got := rowMillis(row, "updated_at_ms", "updated_at"); got != 20500 {
		t.Fatalf("ms field=%d", got)
	}
	acp := jsoncompat.NewObject()
	acp.Set("updatedAt", json.Number("3700"))
	if got := rowMillis(acp, "updatedAt"); got != 3700 {
		t.Fatalf("updatedAt=%d", got)
	}
	if got := rowMillis(jsoncompat.NewObject(), "updatedAt"); got != 0 {
		t.Fatalf("missing=%d", got)
	}
}
