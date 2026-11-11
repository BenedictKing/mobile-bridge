package httpd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestRunRecheckRouteRequiresUnknownRun(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	run := &team.Run{ID: "recheck-run", TeamID: teamID, Status: team.RunOutcomeUnknown,
		Tasks: []team.Task{{ID: "unknown", Status: team.TaskOutcomeUnknown}}}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	url := "/api/teams/" + teamID + "/runs/" + run.ID + "/recheck"
	response := env.request(http.MethodPost, url, `{}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"outcome_unknown"`) {
		t.Fatalf("无完成凭据应保持未知: %d %s", response.Code, response.Body.String())
	}
	fresh := env.store.Run(run.ID)
	fresh.Status = team.RunCompleted
	if err := env.store.SaveRun(fresh); err != nil {
		t.Fatal(err)
	}
	response = env.request(http.MethodPost, url, `{}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("已完成 Run 应拒绝核查: %d %s", response.Code, response.Body.String())
	}
}
