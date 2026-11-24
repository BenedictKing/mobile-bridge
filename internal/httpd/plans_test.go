package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/planstore"
	"github.com/google/uuid"
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

// planFeedbackAgent 在收到提示后发起一份计划审批，并把手机端回应写入 answers.txt。
func planFeedbackAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys,threading
DIR = "` + dir + `"
ANSWER = threading.Event()
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
def handle_prompt(ident):
 print(json.dumps({"jsonrpc":"2.0","id":"plan-1","method":"session/request_permission","params":{"sessionId":"km-plan","toolCall":{"toolCallId":"call-1","title":"ExitPlanMode","rawInput":{"plan":"# Plan\n\n- step one\n- step two"}},"options":[{"optionId":"approve","name":"Approve","kind":"allow_once"},{"optionId":"plan_revise","name":"Revise","kind":"reject_once"}]}}),flush=True)
 ANSWER.wait(20)
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":{"stopReason":"end_turn"}}),flush=True)
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new":
  result={"sessionId":"km-plan","modes":{"currentModeId":"plan","availableModes":[{"id":"default"},{"id":"plan"}]},"configOptions":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/prompt":
  threading.Thread(target=handle_prompt,args=(ident,),daemon=True).start()
  continue
 elif method is None and ident == "plan-1":
  record("answers.txt", json.dumps(m.get("result")))
  ANSWER.set()
  continue
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func TestPlanFeedbackIsValidatedThenRecordedAgainstServerVersion(t *testing.T) {
	dir := t.TempDir()
	server, token, csrf := newFeatureServer(t, t.TempDir())
	store, err := planstore.Open(filepath.Join(t.TempDir(), "acp-plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand(planFeedbackAgent(t, dir))
	manager.Kind = "kimi"
	manager.Plans = store
	defer manager.Close()
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	server.Plans = store

	create := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions", `{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"计划","id":"7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	session := manager.Get("km-plan")
	if session == nil {
		t.Fatal("session not registered")
	}
	// 采用原生 ID 后 View 同步切换：快照 id 与计划历史都归属 km-plan 而非占位 ID。
	if id := session.Snapshot().MustGet("id"); id != "km-plan" {
		t.Fatalf("view kept placeholder id %v", id)
	}
	go func() { _ = session.Prompt("plan please", 30*time.Second) }()
	deadline := time.Now().Add(10 * time.Second)
	var params *jsoncompat.Object
	for params == nil && time.Now().Before(deadline) {
		params = session.RequestParams("plan-1")
		if params == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if params == nil {
		t.Fatal("plan request never arrived")
	}
	planHash, _ := params.MustGet("planHash").(string)
	if planHash != planstore.Hash("# Plan\n\n- step one\n- step two") {
		t.Fatalf("unexpected plan params: %s", jsoncompat.EncodeCompact(params))
	}

	respond := func(feedback string) *httptest.ResponseRecorder {
		body := `{"commandId":"` + uuid.NewString() + `","requestId":"plan-1","response":{"optionId":"plan_revise","text":"revise"}` + feedback + `}`
		return featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/km-plan/respond?agent=kimi", body)
	}
	// 批注意见为空的 comment 属非法格式：整个回应被拒绝，Agent 不会收到。
	invalid := respond(`,"planFeedback":{"annotations":[{"id":"A1","type":"comment","quote":"step one","body":" "}]}`)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "计划批注格式不正确") {
		t.Fatalf("invalid feedback status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "answers.txt")); !os.IsNotExist(err) {
		t.Fatalf("invalid feedback must not reach the agent: %v", err)
	}
	if session.RequestParams("plan-1") == nil {
		t.Fatal("invalid feedback consumed the pending request")
	}

	// 前端回传的版本字段被忽略：记录以服务端 pending 请求的版本与哈希为准。
	valid := respond(`,"planVersion":99,"planFeedback":{"text":"整体再精简","annotations":[{"id":"A1","type":"comment","quote":"step one","body":"拆成两步"},{"id":"A2","type":"delete","quote":"step two","body":""}]}`)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid feedback status=%d body=%s", valid.Code, valid.Body.String())
	}
	for time.Now().Before(deadline) {
		if raw, _ := os.ReadFile(filepath.Join(dir, "answers.txt")); strings.Contains(string(raw), "plan_revise") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "answers.txt")); !strings.Contains(string(raw), `"text": "revise"`) {
		t.Fatalf("agent answer=%s", raw)
	}
	plan, ok := store.Get("kimi", "km-plan", "call-1")
	if !ok || len(plan.Revisions) != 1 || len(plan.Revisions[0].Feedback) != 1 {
		t.Fatalf("feedback not recorded: %+v", plan)
	}
	recorded := plan.Revisions[0].Feedback[0]
	if recorded.RequestID != "plan-1" || recorded.Text != "整体再精简" || len(recorded.Annotations) != 2 || recorded.Annotations[1].Type != "delete" {
		t.Fatalf("recorded feedback=%+v", recorded)
	}
	history := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/km-plan/plans?agent=kimi&planId=call-1", "")
	for _, expected := range []string{`"text": "整体再精简"`, `"id": "A1"`, `"quote": "step one"`, `"body": "拆成两步"`, `"type": "delete"`} {
		if !strings.Contains(history.Body.String(), expected) {
			t.Fatalf("history missing %s: %s", expected, history.Body.String())
		}
	}
}

// kimiPlanAgent 复刻 Kimi acp-server 的计划审阅报文：无 rawInput，计划正文在
// 首个内容块里。它把 plan_revise 只当"需修订"处理（忽略 outcome.text），回合
// 继续运行，直到网关取消；随后把收到的每一条 prompt 写入 prompts.txt。
func kimiPlanAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys
DIR = "` + dir + `"
pending = None
asked = False
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method is None:
  if ident == "plan-req": record("answers.txt", json.dumps(m.get("result")))
  continue
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new":
  result={"sessionId":"km-plan","modes":{"currentModeId":"plan","availableModes":[{"id":"default"},{"id":"plan"}]},"configOptions":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/prompt":
  text=m["params"]["prompt"][0]["text"]
  record("prompts.txt", text)
  if not asked:
   asked=True
   pending=ident
   print(json.dumps({"jsonrpc":"2.0","id":"plan-req","method":"session/request_permission","params":{"sessionId":"km-plan","toolCall":{"toolCallId":"0:tool-1","title":"ExitPlanMode","content":[{"type":"content","content":{"type":"text","text":"Plan saved to: /Users/u/.kimi-code/sessions/wd_x/km-plan/agents/main/plans/white-tiger.md\n\n# Plan\n\n- step one\n- step two"}},{"type":"content","content":{"type":"text","text":"Requesting approval to Presenting plan and exiting plan mode"}}]},"options":[{"optionId":"plan_approve","name":"Approve","kind":"allow_once"},{"optionId":"plan_revise","name":"Revise","kind":"reject_once"},{"optionId":"plan_reject_and_exit","name":"Reject and Exit","kind":"reject_once"}]}}),flush=True)
   continue
  result={"stopReason":"end_turn"}
 elif method == "session/cancel":
  record("cancels.txt", "cancel")
  if pending is not None:
   print(json.dumps({"jsonrpc":"2.0","id":pending,"result":{"stopReason":"cancelled"}}),flush=True)
   pending=None
  continue
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func TestKimiPlanRevisionRelaysFeedbackToAgent(t *testing.T) {
	dir := t.TempDir()
	server, token, csrf := newFeatureServer(t, t.TempDir())
	store, err := planstore.Open(filepath.Join(t.TempDir(), "acp-plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand(kimiPlanAgent(t, dir))
	manager.Kind = "kimi"
	manager.Plans = store
	defer manager.Close()
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	server.Plans = store

	create := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions", `{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"计划","id":"1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	session := manager.Get("km-plan")
	if session == nil {
		t.Fatal("session not registered")
	}
	go func() { _ = session.Prompt("plan please", 30*time.Second) }()
	deadline := time.Now().Add(10 * time.Second)
	var params *jsoncompat.Object
	for params == nil && time.Now().Before(deadline) {
		params = session.RequestParams("plan-req")
		if params == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if params == nil {
		t.Fatal("Kimi plan request never arrived")
	}
	if params.MustGet("planContent") != "# Plan\n\n- step one\n- step two" || params.MustGet("planId") != "white-tiger" ||
		params.MustGet("planVersion").(json.Number).String() != "1" {
		t.Fatalf("Kimi plan was not projected for review: %s", jsoncompat.EncodeCompact(params))
	}

	commandID := uuid.NewString()
	// response.text 与前端 BridgePlanReview.formatFeedback 的产物同形：带编号的批注+全局意见。
	feedback := "## Plan revision feedback\n\n#### A1 · Comment\n\n> step one\n\n拆成两步\n\n### General feedback\n整体再精简"
	encodedFeedback, _ := json.Marshal(feedback)
	body := `{"commandId":"` + commandID + `","requestId":"plan-req","response":{"optionId":"plan_revise","text":` + string(encodedFeedback) + `},"planFeedback":{"text":"整体再精简","annotations":[{"id":"A1","type":"comment","quote":"step one","body":"拆成两步"}]}}`
	valid := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/km-plan/respond?agent=kimi", body)
	if valid.Code != http.StatusOK {
		t.Fatalf("revise status=%d body=%s", valid.Code, valid.Body.String())
	}
	if warning := valid.Body.String(); strings.Contains(warning, "warning") {
		t.Fatalf("feedback relay should have succeeded: %s", warning)
	}

	// Kimi 看不到 outcome.text，网关必须取消盲改回合并把意见作为下一条消息送达。
	var prompts string
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(dir, "prompts.txt"))
		prompts = string(raw)
		if strings.Contains(prompts, "Plan revision feedback") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, expected := range []string{"plan please", "Plan revision feedback", "A1", "拆成两步", "整体再精简"} {
		if !strings.Contains(prompts, expected) {
			t.Fatalf("relayed prompt missing %s: %s", expected, prompts)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "cancels.txt")); len(raw) == 0 {
		t.Fatal("the blind revision turn was not cancelled before relaying feedback")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "answers.txt")); !strings.Contains(string(raw), `"optionId": "plan_revise"`) {
		t.Fatalf("agent answer=%s", raw)
	}

	plan, ok := store.Get("kimi", "km-plan", "white-tiger")
	if !ok || len(plan.Revisions) != 1 || len(plan.Revisions[0].Feedback) != 1 || plan.Revisions[0].Feedback[0].Annotations[0].ID != "A1" {
		t.Fatalf("feedback not recorded on the Kimi plan: %+v", plan)
	}

	// 同一 commandId 的重放不得重复发送修改意见：本用例未接持久命令账本，
	// 请求已过期故返回 409；生产账本命中回执时返回 200，同样不会再次转交。
	replay := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/km-plan/respond?agent=kimi", body)
	if replay.Code != http.StatusConflict {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	time.Sleep(200 * time.Millisecond)
	if raw, _ := os.ReadFile(filepath.Join(dir, "prompts.txt")); string(raw) != prompts {
		t.Fatalf("replayed command relayed feedback twice:\nbefore=%s\nafter=%s", prompts, raw)
	}
}
