package httpd

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/planstore"
)

func TestPlanHistoryRouteIsReadOnlyAndSessionScoped(t *testing.T) {
	server, token, csrf := newFeatureServer(t, t.TempDir())
	manager := acpruntime.NewCommand(nil)
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	store, err := planstore.Open(filepath.Join(t.TempDir(), "acp-plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.Plans = store
	for version, content := range []string{"first", "second"} {
		if err := store.Record("kimi", "session-1", "plan-1", version+1, planstore.Hash(content), content); err != nil {
			t.Fatal(err)
		}
	}

	withoutAgent := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/session-1/plans?planId=plan-1", "")
	if withoutAgent.Code != http.StatusBadRequest || !strings.Contains(withoutAgent.Body.String(), "必须指定 Agent") {
		t.Fatalf("missing agent status=%d body=%s", withoutAgent.Code, withoutAgent.Body.String())
	}
	response := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/session-1/plans?agent=kimi&planId=plan-1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", response.Code, response.Body.String())
	}
	for _, expected := range []string{`"planId": "plan-1"`, `"version": 1`, `"content": "first"`, `"version": 2`, `"content": "second"`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("history missing %s: %s", expected, response.Body.String())
		}
	}
	other := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/session-2/plans?agent=kimi&planId=plan-1", "")
	if other.Code != http.StatusOK || !strings.Contains(other.Body.String(), `"revisions": []`) {
		t.Fatalf("cross-session history status=%d body=%s", other.Code, other.Body.String())
	}
	missingID := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/session-1/plans?agent=kimi", "")
	if missingID.Code != http.StatusBadRequest {
		t.Fatalf("missing plan id status=%d body=%s", missingID.Code, missingID.Body.String())
	}
}

func TestPlanHistoryRouteReportsUnavailableStore(t *testing.T) {
	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": acpruntime.NewCommand(nil)}
	response := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/session-1/plans?agent=kimi&planId=plan-1", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "当前网关未启用计划历史") {
		t.Fatalf("unavailable store status=%d body=%s", response.Code, response.Body.String())
	}
}
