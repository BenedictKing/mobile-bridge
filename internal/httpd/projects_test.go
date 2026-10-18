package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
