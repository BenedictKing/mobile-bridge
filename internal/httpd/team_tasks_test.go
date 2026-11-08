package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestTeamTaskObservabilityAPI(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	otherTeam, _ := env.createTeam(t)
	for _, fixture := range []struct {
		runID, taskID, status string
	}{
		{"run-tasks-live", "task-live", team.TaskRunning},
		{"run-tasks-done", "task-done", team.TaskCompleted},
	} {
		if err := env.store.SaveRun(&team.Run{ID: fixture.runID, TeamID: teamID, Goal: "任务 API", Mode: team.ModeSingle,
			Status: team.RunDispatching, Tasks: []team.Task{{ID: fixture.taskID, MemberID: "lead", Status: fixture.status}}}); err != nil {
			t.Fatal(err)
		}
	}
	event := &team.Event{ID: "event-task-api", RunID: "run-tasks-live", Type: team.EventTaskCompleted,
		Source: "test", Payload: map[string]any{"taskId": "task-live"}}
	if err := env.journal.Append(event); err != nil {
		t.Fatal(err)
	}

	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/tasks", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "task-done") ||
		!strings.Contains(response.Body.String(), "task-live") {
		t.Fatalf("默认在途列表异常: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/tasks?all=1", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "task-done") {
		t.Fatalf("全量列表异常: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/run-tasks-live/tasks/task-live", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"taskId": "task-live"`) {
		t.Fatalf("check 异常: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+otherTeam+"/runs/run-tasks-live/tasks/task-live", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("跨 Team check 应 404: %d %s", response.Code, response.Body.String())
	}

	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/task-events?after=0&limit=10", "")
	var page struct {
		Events []struct {
			EventID  string `json:"eventId"`
			Sequence int    `json:"sequence"`
			Status   string `json:"status"`
		} `json:"events"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(page.Events) != 1 || page.Events[0].EventID != "event-task-api" ||
		page.Events[0].Status != team.TaskCompleted || page.Cursor == "0" {
		t.Fatalf("通知页异常: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/task-events?after="+page.Cursor, "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "event-task-api") {
		t.Fatalf("游标后不得重复通知: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/task-events?after=bad", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("非法游标应 400: %d %s", response.Code, response.Body.String())
	}

	updateBody := `{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","version":1,"strategy":"enqueue","text":"补充要求"}`
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/run-tasks-live/tasks/task-live/update", updateBody)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"updateStatus": "accepted"`) ||
		!strings.Contains(response.Body.String(), `"updateOf": "task-live"`) {
		t.Fatalf("enqueue HTTP 契约异常: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/run-tasks-live/tasks/task-live/update", updateBody)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("更新重放应幂等: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, "/api/teams/"+otherTeam+"/runs/run-tasks-live/tasks/task-live/update", updateBody)
	if response.Code != http.StatusNotFound {
		t.Fatalf("跨 Team 更新应 404: %d %s", response.Code, response.Body.String())
	}
}
