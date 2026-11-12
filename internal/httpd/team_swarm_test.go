package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

func swarmHTTPFixture(t *testing.T, enabled bool) (*teamTestEnv, *team.Team, *team.Run, string) {
	t.Helper()
	env := newTeamTestEnv(t, "queued", false)
	policy := team.DefaultPolicy()
	policy.SwarmEnabled = enabled
	teamRow := &team.Team{ID: uuid.NewString(), Name: "Swarm HTTP", LeadID: "lead", Policy: policy, CreatedAt: time.Now(), Members: []team.TeamMember{{ID: "lead", SessionID: "swarm-session", AgentKind: "kimi", Role: team.RoleLead, Enabled: true}}}
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{ID: uuid.NewString(), TeamID: teamRow.ID, Goal: "冻结验收条件", Status: team.RunPaused, CreatedAt: time.Now()}
	if enabled {
		run.Swarm = &team.SwarmState{SchemaVersion: 1, ChainID: run.ID, Status: "awaiting_contract", Artifacts: []team.SwarmArtifact{}, Evals: []team.SwarmEval{}, Findings: []team.SwarmFinding{}, Operations: map[string]string{}}
	}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	return env, teamRow, run, "/api/teams/" + teamRow.ID + "/runs/" + run.ID
}

func swarmHTTPBody(t *testing.T, request any) string {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func swarmHTTPContract(goal string) team.SwarmContract {
	return team.SwarmContract{Criteria: []team.SwarmCriterion{{ID: "original", Text: goal, CheckKind: "human", Required: true}}, MaxEvals: 4}
}
func swarmHTTPPost(t *testing.T, env *teamTestEnv, path string, request any, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := env.request(http.MethodPost, path, swarmHTTPBody(t, request))
	if response.Code != want {
		t.Fatalf("POST %s want %d got %d: %s", path, want, response.Code, response.Body.String())
	}
	return response
}

func TestSwarmHTTPAuthCSRFAndTeamOwnership(t *testing.T) {
	env, teamRow, run, base := swarmHTTPFixture(t, true)
	for _, suffix := range []string{"/swarm", "/evals"} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8791"+base+suffix, nil)
		request.Host = "127.0.0.1:8791"
		request.Header.Set("Origin", "http://127.0.0.1:8791")
		response := httptest.NewRecorder()
		env.server.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("missing auth %s: %d %s", suffix, response.Code, response.Body.String())
		}
	}
	request := team.SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1, Contract: swarmHTTPContract(run.Goal)}
	response := featureRequest(env.server, env.token, "", http.MethodPost, base+"/contract-proposals", swarmHTTPBody(t, request))
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing csrf: %d %s", response.Code, response.Body.String())
	}
	other := teamRow.Clone()
	other.ID = uuid.NewString()
	other.Version = 0
	if err := env.store.SaveTeam(other); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/swarm", "/evals"} {
		response = env.request(http.MethodGet, "/api/teams/"+other.ID+"/runs/"+run.ID+suffix, "")
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), run.Goal) {
			t.Fatalf("cross-team %s: %d %s", suffix, response.Code, response.Body.String())
		}
	}
	swarmHTTPPost(t, env, "/api/teams/"+other.ID+"/runs/"+run.ID+"/contract-proposals", request, http.StatusNotFound)
	if len(env.store.Run(run.ID).Swarm.Operations) != 0 || env.queue.enqueueCount() != 0 {
		t.Fatal("rejected operations mutated state")
	}
}

func TestSwarmHTTPContractFreezeVersionsAndIdempotency(t *testing.T) {
	env, _, run, base := swarmHTTPFixture(t, true)
	proposal := team.SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1, Contract: swarmHTTPContract(run.Goal)}
	swarmHTTPPost(t, env, base+"/contract-proposals", proposal, http.StatusOK)
	proposed := env.store.Run(run.ID)
	if proposed.Swarm.Proposal == nil || proposed.Swarm.Proposal.ID != proposal.ID || proposed.Swarm.Contract != nil {
		t.Fatalf("proposal not recorded: %+v", proposed.Swarm)
	}
	version := proposed.Version
	swarmHTTPPost(t, env, base+"/contract-proposals", proposal, http.StatusOK)
	if env.store.Run(run.ID).Version != version {
		t.Fatal("idempotent proposal changed version")
	}
	changed := proposal
	changed.Contract.Criteria = append([]team.SwarmCriterion(nil), proposal.Contract.Criteria...)
	changed.Contract.Criteria[0].Text = "不同内容"
	swarmHTTPPost(t, env, base+"/contract-proposals", changed, http.StatusBadRequest)
	stale := proposal
	stale.ID = uuid.NewString()
	swarmHTTPPost(t, env, base+"/contract-proposals", stale, http.StatusConflict)
	accept := team.SwarmRequest{ID: uuid.NewString(), Version: version, SchemaVersion: 1}
	swarmHTTPPost(t, env, base+"/contract-proposals/not-the-proposal/accept", accept, http.StatusBadRequest)
	swarmHTTPPost(t, env, base+"/contract-proposals/"+proposal.ID+"/accept", accept, http.StatusOK)
	frozen := env.store.Run(run.ID)
	if frozen.Swarm.Contract == nil || frozen.Swarm.ContractHash == "" || frozen.Swarm.Contract.OriginalGoal != run.Goal {
		t.Fatalf("contract not frozen: %+v", frozen.Swarm)
	}
	swarmHTTPPost(t, env, base+"/contract-proposals/"+proposal.ID+"/accept", accept, http.StatusOK)
	if env.store.Run(run.ID).Version != frozen.Version {
		t.Fatal("idempotent accept changed version")
	}
	newProposal := proposal
	newProposal.ID = uuid.NewString()
	newProposal.Version = frozen.Version
	swarmHTTPPost(t, env, base+"/contract-proposals", newProposal, http.StatusBadRequest)
	if env.queue.enqueueCount() != 0 {
		t.Fatal("paused contract operation dispatched model task")
	}
}

func TestSwarmHTTPProjectionDoesNotExposePrivateOperationLedger(t *testing.T) {
	env, _, run, base := swarmHTTPFixture(t, true)
	run.Swarm.Operations["private-operation"] = "private-operation-digest"
	run.Swarm.Evals = []team.SwarmEval{{ID: "visible-eval", ArtifactID: "visible-artifact", Status: "passed", Validity: "fresh", ExitCode: 0}}
	run.Tasks = []team.Task{{ID: "worker", MemberID: "dev", DispatchSessionID: "shared-actual-session", Status: team.TaskCompleted}, {ID: "review", MemberID: "reviewer", DispatchSessionID: "shared-actual-session", Status: team.TaskCompleted, ResultDetail: &team.TaskResult{ReviewVerdict: "passed"}}}
	run.Swarm.Artifacts = []team.SwarmArtifact{{ID: "visible-artifact", TaskID: "worker", Kind: "task", ReviewTaskID: "review", Validity: "fresh"}}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/swarm", "/evals"} {
		response := env.request(http.MethodGet, base+suffix, "")
		if response.Code != http.StatusOK {
			t.Fatalf("projection %s: %d %s", suffix, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "operations") || strings.Contains(response.Body.String(), "private-operation") {
			t.Errorf("private operation ledger exposed on %s: %s", suffix, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "visible-eval") {
			t.Errorf("eval missing on %s: %s", suffix, response.Body.String())
		}
		if suffix != "/evals" && !strings.Contains(response.Body.String(), `"independent": false`) {
			t.Errorf("shared actual session must not appear independent on %s: %s", suffix, response.Body.String())
		}
	}
}

func TestSwarmHTTPRecheckValidationAndDisabledRun(t *testing.T) {
	env, _, run, base := swarmHTTPFixture(t, true)
	proposal := team.SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1, Contract: swarmHTTPContract(run.Goal)}
	swarmHTTPPost(t, env, base+"/contract-proposals", proposal, http.StatusOK)
	accept := team.SwarmRequest{ID: uuid.NewString(), Version: env.store.Run(run.ID).Version, SchemaVersion: 1}
	swarmHTTPPost(t, env, base+"/contract-proposals/"+proposal.ID+"/accept", accept, http.StatusOK)
	version := env.store.Run(run.ID).Version
	for _, request := range []team.SwarmRequest{{ID: "not-a-uuid", Version: version, SchemaVersion: 1}, {ID: uuid.NewString(), Version: version, SchemaVersion: 99}, {ID: uuid.NewString(), Version: version, SchemaVersion: 1}} {
		swarmHTTPPost(t, env, base+"/evals/missing/recheck", request, http.StatusBadRequest)
	}
	if len(env.store.Run(run.ID).Swarm.Evals) != 0 || env.store.Run(run.ID).Version != version {
		t.Fatal("invalid recheck changed state")
	}
	disabled, _, offRun, offBase := swarmHTTPFixture(t, false)
	swarmHTTPPost(t, disabled, offBase+"/contract-proposals", team.SwarmRequest{ID: uuid.NewString(), Version: offRun.Version, SchemaVersion: 1, Contract: swarmHTTPContract(offRun.Goal)}, http.StatusBadRequest)
}

func TestSwarmHTTPPolicyDefaultAndRunSnapshot(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	response := env.request(http.MethodPost, "/api/teams", `{"name":"default"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create default: %d %s", response.Code, response.Body.String())
	}
	var defaultTeam team.Team
	if err := json.Unmarshal(response.Body.Bytes(), &defaultTeam); err != nil {
		t.Fatal(err)
	}
	if defaultTeam.Policy.SwarmEnabled {
		t.Fatal("swarm must default off")
	}
	response = env.request(http.MethodPost, "/api/teams", `{"name":"invalid","policy":{"swarmEnabled":"yes"}}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid flag: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, "/api/teams", `{"name":"enabled","policy":{"swarmEnabled":true}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("enabled flag: %d %s", response.Code, response.Body.String())
	}
	var enabled team.Team
	if err := json.Unmarshal(response.Body.Bytes(), &enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled.Policy.SwarmEnabled {
		t.Fatal("enabled flag missing")
	}
	enabled.LeadID = "lead"
	enabled.Members = []team.TeamMember{{ID: "lead", SessionID: "unresolved", AgentKind: "kimi", Role: team.RoleLead, Enabled: true}}
	if err := env.store.SaveTeam(&enabled); err != nil {
		t.Fatal(err)
	}
	requestID := uuid.NewString()
	body := `{"id":"` + requestID + `","goal":"保留策略快照","plan":[{"id":"work","memberId":"lead","prompt":"分析当前代码","writeLock":"exclusive"}]}`
	response = env.request(http.MethodPost, "/api/teams/"+enabled.ID+"/runs", body)
	if response.Code != http.StatusOK {
		t.Fatalf("new run: %d %s", response.Code, response.Body.String())
	}
	run := env.store.Run(requestID)
	if run.Swarm == nil || run.Swarm.SchemaVersion != 1 {
		t.Fatal("new enabled run missing swarm")
	}
	enabled = *env.store.Team(enabled.ID)
	enabled.Policy.SwarmEnabled = false
	if err := env.store.SaveTeam(&enabled); err != nil {
		t.Fatal(err)
	}
	if env.store.Run(requestID).Swarm == nil {
		t.Fatal("policy toggle removed existing run snapshot")
	}
	response = env.request(http.MethodPost, "/api/teams/"+enabled.ID+"/runs", body)
	if response.Code != http.StatusOK || env.store.Run(requestID).Swarm == nil {
		t.Fatalf("idempotent create changed snapshot: %d %s", response.Code, response.Body.String())
	}
	legacyID := uuid.NewString()
	legacyBody := strings.Replace(body, requestID, legacyID, 1)
	response = env.request(http.MethodPost, "/api/teams/"+enabled.ID+"/runs", legacyBody)
	if response.Code != http.StatusOK || env.store.Run(legacyID).Swarm != nil {
		t.Fatalf("new disabled run incompatible: %d %s", response.Code, response.Body.String())
	}
	for _, id := range []string{requestID, legacyID} {
		_, _ = env.server.Teams.Cancel(id)
	}
}
