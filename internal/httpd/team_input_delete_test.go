package httpd

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestTeamInputDeleteAPI(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	input, _, err := env.server.TeamInputs.ImportHistoricalInput(teamID, "orphan", "继续", "source-run", "lead", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/teams/" + teamID + "/inputs/" + input.ID
	body := fmt.Sprintf(`{"version":%d}`, input.Version)
	if r := env.request(http.MethodDelete, path, `{}`); r.Code != http.StatusBadRequest {
		t.Fatalf("missing version: %d %s", r.Code, r.Body.String())
	}
	run := &team.Run{ID: "source-run", TeamID: teamID, Status: team.RunDispatching}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if r := env.request(http.MethodDelete, path, body); r.Code != http.StatusConflict {
		t.Fatalf("active run: %d %s", r.Code, r.Body.String())
	}
	run.Status = team.RunCompleted
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if r := env.request(http.MethodDelete, path, body); r.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	if env.server.TeamInputs.Get(teamID, input.ID) != nil {
		t.Fatal("message remains")
	}
	if env.store.Run(run.ID) == nil {
		t.Fatal("run history was removed")
	}
	if r := env.request(http.MethodDelete, path, body); r.Code != http.StatusNotFound {
		t.Fatalf("repeat: %d", r.Code)
	}
}
