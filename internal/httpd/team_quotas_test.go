package httpd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestTeamQuotasRouteAndLeadBriefing(t *testing.T) {
	env := newTeamTestEnv(t, "accepted", true)
	teamID, _ := env.createTeam(t)
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	env.server.Pool = accpool.New(env.dataDir, "")
	duration := int64(300)
	account := &accpool.Account{
		ID: "u_u1", UserID: "u1", Email: "secret@example.com", AccessToken: "at1", RefreshToken: "rt1",
		Status: accpool.StatusActive, QuotaUpdatedAt: 1700000000000, QuotaError: "refresh failed with at1",
		Quota: &accpool.Quota{
			Weekly: &accpool.WindowQuota{Used: 25, Limit: 100, ResetAt: "2026-11-07T00:00:00Z"},
			Limits: []*accpool.WindowQuota{
				{Name: "5小时", Used: 40, Limit: 80, WindowDuration: &duration},
				{Name: "MCP", Used: 99, Limit: 100, Auxiliary: true},
			},
			Booster: &accpool.BoosterQuota{BalanceCents: int64Ptr(23), Currency: "CNY"},
		},
	}
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.dataDir, "pool", "kimi"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.dataDir, "pool", "kimi", "u_u1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := []byte(`{"access_token":"at1","refresh_token":"rt1","token_type":"Bearer"}`)
	if err := os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), credentials, 0o600); err != nil {
		t.Fatal(err)
	}
	teamRow := env.store.Team(teamID)
	teamRow.Members = append(teamRow.Members, team.TeamMember{ID: "claude", SessionID: "sess-claude", AgentKind: "claude", Enabled: true})
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}

	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/quotas", "")
	if response.Code != http.StatusOK {
		t.Fatalf("quotas status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Agents []struct {
			Agent          string `json:"agent"`
			Status         string `json:"status"`
			AccountStatus  string `json:"accountStatus"`
			QuotaUpdatedAt int64  `json:"quotaUpdatedAt"`
			Windows        []struct {
				Name             string  `json:"name"`
				Remaining        float64 `json:"remaining"`
				RemainingPercent float64 `json:"remainingPercent"`
				Limit            float64 `json:"limit"`
			} `json:"windows"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Agents) != 2 {
		t.Fatalf("agents=%+v", payload.Agents)
	}
	first := payload.Agents[0]
	if first.Agent != "kimi" || first.Status != "ready" || first.AccountStatus != accpool.StatusActive || first.QuotaUpdatedAt != 1700000000000 {
		t.Fatalf("quota row=%+v", first)
	}
	if len(first.Windows) != 2 || first.Windows[0].Remaining != 75 || first.Windows[0].RemainingPercent != 75 ||
		first.Windows[1].Remaining != 40 || first.Windows[1].RemainingPercent != 50 {
		t.Fatalf("normalized windows=%+v", first.Windows)
	}
	if payload.Agents[1].Agent != "claude" || payload.Agents[1].Status != "not_supported" {
		t.Fatalf("unsupported agent row=%+v", payload.Agents[1])
	}
	for _, secret := range []string{"u_u1", "secret@example.com", "at1", "rt1"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("quota response leaked %q: %s", secret, response.Body.String())
		}
	}

	briefing := NewTeamLeadBriefingSource(env.server)(teamRow)
	for _, want := range []string{
		"[Team account quotas]", "agent=kimi", "remaining=75/100 (75%)", "remaining=40/80 (50%)",
		"boosterBalanceCents=23 CNY", "quotaUpdatedAt=2023-11-14T22:13:20Z", "agent=claude · status=not_supported",
		"quotaRefreshError=true", "未做实时刷新", "结合成员 tags 与剩余额度",
	} {
		if !strings.Contains(briefing, want) {
			t.Fatalf("Lead quota briefing missing %q:\n%s", want, briefing)
		}
	}
	for _, unwanted := range []string{"MCP", "u_u1", "secret@example.com", "at1", "rt1"} {
		if strings.Contains(briefing, unwanted) {
			t.Fatalf("Lead quota briefing contains %q:\n%s", unwanted, briefing)
		}
	}
}

func int64Ptr(value int64) *int64 { return &value }
