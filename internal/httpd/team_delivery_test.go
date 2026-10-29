package httpd

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestTeamConversationDirectModeDoesNotCreateRun(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	target := "/api/sessions/" + env.sessionID + "/send?host=local"
	directID := "1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := fmt.Sprintf(`{"id":%q,"teamMode":"direct","text":"只回答一个问题","mode":"send","workMode":"default"}`, directID)
	response := env.request(http.MethodPost, target, body)
	if response.Code != http.StatusOK {
		t.Fatalf("主控直答发送失败: %d %s", response.Code, response.Body.String())
	}
	if got := len(env.store.RunsByTeam(teamID)); got != 0 {
		t.Fatalf("主控直答不得创建 Run: %d", got)
	}

	teamIDForMessage := "2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body = fmt.Sprintf(`{"id":%q,"teamId":%q,"teamMode":"team","text":"让团队完成","mode":"send","workMode":"default"}`, teamIDForMessage, teamID)
	response = env.request(http.MethodPost, target, body)
	if response.Code != http.StatusOK || env.store.Run(teamIDForMessage) == nil {
		t.Fatalf("团队执行仍应创建 Run: %d %s", response.Code, response.Body.String())
	}
	_, _ = env.server.Teams.Cancel(teamIDForMessage)
}

func TestRunDeliveryHTTPAcceptAndDuplicate(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	run := &team.Run{ID: "delivery-http-run", TeamID: teamID, Goal: "交付", Status: team.RunCompleted, DeliveryStatus: team.DeliveryPendingReview}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	target := "/api/teams/" + teamID + "/runs/" + run.ID + "/delivery"
	body := `{"status":"accepted","version":1}`
	response := env.request(http.MethodPost, target, body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deliveryStatus": "accepted"`) {
		t.Fatalf("接受成果 API 失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, target, body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("交付重放应返回 duplicate: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, target, `{"status":"discarded","version":2}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("已接受成果不得丢弃: %d %s", response.Code, response.Body.String())
	}
}
