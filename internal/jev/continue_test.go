package jev

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestQuotaStopMatchesPaywallSignals(t *testing.T) {
	for _, text := range []string{
		"额度不足，请充值", "余额已耗尽", "用量已达今日上限", "You exceeded your quota",
		"usage limit reached", "Rate limit hit", "429 Too Many Requests", "payment required",
		"credit balance is zero", "subscription limit exceeded",
	} {
		if !QuotaStop(text) {
			t.Fatalf("expected quota stop for %q", text)
		}
	}
	for _, text := range []string{
		"", "usage_limit is a code identifier", "RateLimiter class", "一切正常",
	} {
		if QuotaStop(text) {
			t.Fatalf("unexpected quota stop for %q", text)
		}
	}
}

func continueContext(status, stopReason, error, tail string) *jsoncompat.Object {
	context := jsoncompat.NewObject()
	context.Set("turnStatus", status)
	context.Set("stopReason", stopReason)
	context.Set("error", error)
	context.Set("assistantTail", tail)
	return context
}

func TestDecideContinueAppliesDeterministicPolicy(t *testing.T) {
	cases := []struct {
		name       string
		context    *jsoncompat.Object
		assessment *ContinueAssessment
		send       bool
		reason     string
	}{
		{"quota in error", continueContext("failed", "", "余额不足", ""), nil, false, "quota"},
		{"user cancelled", continueContext("interrupted", "cancelled", "", ""), nil, false, "cancelled"},
		{"interrupted without key", continueContext("failed", "", "Agent 响应超时", ""), nil, true, "interrupted"},
		{"max_tokens without key", continueContext("completed", "max_tokens", "", ""), nil, true, "interrupted"},
		{"completed without key", continueContext("completed", "end_turn", "", ""), nil, false, "undecided"},
		{"jev says quota", continueContext("completed", "end_turn", "", ""), &ContinueAssessment{StopKind: "quota"}, false, "quota"},
		{"jev says awaiting user", continueContext("completed", "end_turn", "", ""), &ContinueAssessment{StopKind: "awaiting_user"}, false, "awaiting_user"},
	}
	for _, item := range cases {
		send, reason := DecideContinue(item.context, item.assessment)
		if send != item.send || reason != item.reason {
			t.Fatalf("%s: send=%v reason=%s", item.name, send, reason)
		}
	}
}

func boolPtr(value bool) *bool { return &value }

func TestDecideContinueWeighsJeVVerdict(t *testing.T) {
	base := func() *jsoncompat.Object { return continueContext("completed", "end_turn", "", "还在处理中") }
	cases := []struct {
		name       string
		assessment *ContinueAssessment
		send       bool
		reason     string
	}{
		{"complete", &ContinueAssessment{Incomplete: boolPtr(false), ShouldContinue: boolPtr(false)}, false, "complete"},
		{"unsure completeness", &ContinueAssessment{ShouldContinue: boolPtr(true), Confidence: 0.9}, false, "unsure"},
		{"jev stop", &ContinueAssessment{Incomplete: boolPtr(true), ShouldContinue: boolPtr(false)}, false, "jev_stop"},
		{"jev review", &ContinueAssessment{Incomplete: boolPtr(true)}, false, "jev_review"},
		{"low confidence", &ContinueAssessment{Incomplete: boolPtr(true), ShouldContinue: boolPtr(true), Confidence: 0.6}, false, "low_confidence"},
		{"resume", &ContinueAssessment{Incomplete: boolPtr(true), ShouldContinue: boolPtr(true), Confidence: 0.85}, true, "jev_continue"},
	}
	for _, item := range cases {
		send, reason := DecideContinue(base(), item.assessment)
		if send != item.send || reason != item.reason {
			t.Fatalf("%s: send=%v reason=%s", item.name, send, reason)
		}
	}
}

func testSettings(t *testing.T, serverURL string) *Settings {
	t.Helper()
	settings := New(filepath.Join(t.TempDir(), "jev.json"))
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", serverURL)
	value.Set("autoContinue", true)
	if _, err := settings.Update(value); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestContinueAssessMapsTypedAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"complete":{"noul":0.1},"stop_kind":{"choice":"interrupted"},"continue":{"choice":"continue","confidence":0.9}}}`))
	}))
	defer server.Close()
	assessment := testSettings(t, server.URL).ContinueAssess(jsoncompat.NewObject())
	if assessment == nil {
		t.Fatal("assessment missing")
	}
	if assessment.Incomplete == nil || !*assessment.Incomplete ||
		assessment.StopKind != "interrupted" ||
		assessment.ShouldContinue == nil || !*assessment.ShouldContinue ||
		assessment.Confidence != 0.9 {
		t.Fatalf("unexpected assessment: %+v", assessment)
	}
}

func TestContinueAssessRejectsMalformedAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"continue":{"choice":"maybe"}}}`))
	}))
	defer server.Close()
	if assessment := testSettings(t, server.URL).ContinueAssess(jsoncompat.NewObject()); assessment != nil {
		t.Fatalf("unexpected assessment: %+v", assessment)
	}
}

func TestContinueEnabledReadsPersistedSwitch(t *testing.T) {
	settings := testSettings(t, "http://127.0.0.1:1")
	if !settings.ContinueEnabled() {
		t.Fatal("auto-continue should be enabled")
	}
	if settings.ContinueKeyPresent() != true {
		t.Fatal("key should be present")
	}
}

func viewWithTurn(status string, stopReason string, messages []*jsoncompat.Object) *jsoncompat.Object {
	view := jsoncompat.NewObject()
	view.Set("cwd", "/work")
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-1")
	turn.Set("status", status)
	turn.Set("stopReason", stopReason)
	turn.Set("error", nil)
	items := &jsoncompat.Array{}
	for _, message := range messages {
		items.Items = append(items.Items, message)
	}
	turn.Set("messages", items)
	turns := &jsoncompat.Array{}
	turns.Items = append(turns.Items, turn)
	view.Set("turns", turns)
	return view
}

func message(kind, role, text string) *jsoncompat.Object {
	entry := jsoncompat.NewObject()
	entry.Set("kind", kind)
	entry.Set("role", role)
	entry.Set("text", text)
	return entry
}

func TestContinueContextCompressesSettledTurn(t *testing.T) {
	view := viewWithTurn("failed", "", []*jsoncompat.Object{
		message("userMessage", "user", "跑一下测试"),
		message("agentMessage", "assistant", "第一段"),
		message("agentMessage", "assistant", "第二段"),
		message("error", "error", "Agent 响应超时"),
	})
	context, ok := ContinueContext("kimi", view)
	if !ok {
		t.Fatal("context should build for settled turn")
	}
	if jsoncompat.PyStr(context.MustGet("agent")) != "kimi" ||
		jsoncompat.PyStr(context.MustGet("turnStatus")) != "failed" ||
		jsoncompat.PyStr(context.MustGet("userRequest")) != "跑一下测试" {
		t.Fatalf("unexpected context: %s", jsoncompat.EncodeCompact(context))
	}
	tail := jsoncompat.PyStr(context.MustGet("assistantTail"))
	if !strings.Contains(tail, "第一段\n第二段") {
		t.Fatalf("tail should stay chronological: %q", tail)
	}
	if !strings.Contains(jsoncompat.PyStr(context.MustGet("error")), "Agent 响应超时") {
		t.Fatalf("error missing: %s", jsoncompat.EncodeCompact(context))
	}
}

func TestContinueContextRejectsUnsettledTurns(t *testing.T) {
	if _, ok := ContinueContext("kimi", viewWithTurn("inProgress", "", []*jsoncompat.Object{message("userMessage", "user", "hi")})); ok {
		t.Fatal("in-progress turn must not build a context")
	}
	if _, ok := ContinueContext("kimi", viewWithTurn("completed", "end_turn", nil)); ok {
		t.Fatal("message-less turn must not build a context")
	}
	empty := jsoncompat.NewObject()
	empty.Set("turns", &jsoncompat.Array{})
	if _, ok := ContinueContext("kimi", empty); ok {
		t.Fatal("turn-less view must not build a context")
	}
}

func TestEvaluateRecordsRejectionReasons(t *testing.T) {
	blank := New(filepath.Join(t.TempDir(), "jev.json"))
	if reason := blank.Evaluate("session/request_permission", jsoncompat.NewObject()).Reason; reason != "auto_approve_off" {
		t.Fatalf("reason=%s", reason)
	}
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", "http://127.0.0.1:1")
	value.Set("autoApprove", true)
	if _, err := blank.Update(value); err != nil {
		t.Fatal(err)
	}
	// baseUrl points nowhere: the assessment must fail, not silently approve.
	decision := blank.Evaluate("session/request_permission", jsoncompat.NewObject())
	if decision.Assessed || decision.Reason != "assess_failed" || decision.Response != nil {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("secret_exposure",
			fullProbabilities(map[string]string{"secret_exposure": "0.5", "privilege_escalation": "0.1"}, ""),
			"accept_once", 0.99, 0.99)))
	}))
	defer server.Close()
	value.Set("baseUrl", server.URL)
	if _, err := blank.Update(value); err != nil {
		t.Fatal(err)
	}
	decision = blank.Evaluate("session/request_permission", jsoncompat.NewObject())
	if decision.Reason != "negative_sum_over_threshold" || decision.Approved {
		t.Fatalf("high negative mass must stay manual: %+v", decision)
	}
	if decision.Categories["secret_exposure"] != 0.5 || decision.Categories["privilege_escalation"] != 0.1 {
		t.Fatalf("category probabilities must survive for the log: %+v", decision.Categories)
	}
	if diff := decision.NegativeSum - 0.6; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("negative sum = %f, want 0.6", decision.NegativeSum)
	}
	if decision.Threshold != defaultThreshold || decision.Reversible != 0.99 || decision.Confidence != 0.99 || decision.Recommend != "accept_once" {
		t.Fatalf("assessment fields must survive for the log: %+v", decision)
	}
	if decision.TopCategory != "secret_exposure" {
		t.Fatalf("top category = %q", decision.TopCategory)
	}
}

func TestEvaluateSendsStructuredQuestions(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work", fullProbabilities(nil, ""), "review", 1, 0.5)))
	}))
	defer server.Close()
	settings := approvalSettings(t, server.URL)
	// Raw ACP shape: the subject lives in toolCall, not at the top level.
	params := jsoncompat.NewObject()
	toolCall := jsoncompat.NewObject()
	toolCall.Set("title", "Bash")
	rawInput := jsoncompat.NewObject()
	rawInput.Set("command", "rm -rf /tmp/scratch")
	toolCall.Set("rawInput", rawInput)
	params.Set("toolCall", toolCall)
	options := &jsoncompat.Array{}
	allow := jsoncompat.NewObject()
	allow.Set("optionId", "allow-once")
	allow.Set("kind", "allow_once")
	options.Items = append(options.Items, allow)
	params.Set("options", options)
	decision := settings.Evaluate("session/request_permission", params)
	if decision.Reason != "recommend_review" {
		t.Fatalf("reason=%s", decision.Reason)
	}
	decoded, err := jsoncompat.Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	root := decoded.(*jsoncompat.Object)
	questions := root.MustGet("questions").(*jsoncompat.Object)
	outcome := questions.MustGet("outcome").(*jsoncompat.Object)
	if outcome.MustGet("type") != "choice" {
		t.Fatalf("outcome question must be a choice: %s", jsoncompat.EncodeCompact(questions))
	}
	criteria := outcome.MustGet("criteria").(*jsoncompat.Object)
	want := map[string]bool{}
	for _, category := range defaultPolicy {
		want[category.Name] = true
	}
	if criteria.Len() != len(want) {
		t.Fatalf("criteria keys = %v, want the policy names", criteria.Keys())
	}
	for _, name := range criteria.Keys() {
		if !want[name] {
			t.Fatalf("unexpected criteria key %q", name)
		}
	}
	if questions.MustGet("reversible").(*jsoncompat.Object).MustGet("type") != "noul" {
		t.Fatal("reversible question must stay typed")
	}
	if questions.MustGet("recommend").(*jsoncompat.Object).MustGet("type") != "choice" {
		t.Fatal("recommend question must stay typed")
	}
	// The state must carry the subject derived from toolCall.
	state := root.MustGet("state").(*jsoncompat.Object)
	if state.MustGet("title") != "Bash" || state.MustGet("command") != "rm -rf /tmp/scratch" {
		t.Fatalf("state subject missing: %s", jsoncompat.EncodeCompact(state))
	}
}

func TestAssessFailsClosedOnIncompleteDistribution(t *testing.T) {
	cases := []struct {
		name          string
		choice        string
		probabilities string
	}{
		{"no probabilities", "routine_work", ""},
		{"probabilities not an object", "routine_work", `"0.9"`},
		{"missing one category", "routine_work", fullProbabilities(nil, "privilege_escalation")},
		{"negative value", "routine_work", fullProbabilities(map[string]string{"secret_exposure": "-0.2"}, "")},
		{"value above one", "routine_work", fullProbabilities(map[string]string{"secret_exposure": "1.5"}, "")},
		{"non numeric value", "routine_work", fullProbabilities(map[string]string{"secret_exposure": `"high"`}, "")},
		{"choice outside policy", "definitely_not_a_category", fullProbabilities(nil, "")},
		{"empty choice", "", fullProbabilities(nil, "")},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(outcomeAnswers(item.choice, item.probabilities, "accept_once", 1, 1)))
			}))
			defer server.Close()
			decision := approvalSettings(t, server.URL).Evaluate("session/request_permission", jsoncompat.NewObject())
			if decision.Reason != "assess_failed" || decision.Approved || decision.Response != nil || decision.Assessed {
				t.Fatalf("incomplete distribution must fail closed: %+v", decision)
			}
		})
	}
}

func TestPositiveSumNotCounted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work",
			fullProbabilities(map[string]string{"routine_work": "0.9"}, ""),
			"accept_once", 0.99, 0.99)))
	}))
	defer server.Close()
	settings := approvalSettings(t, server.URL)
	params := jsoncompat.NewObject()
	options := &jsoncompat.Array{}
	allow := jsoncompat.NewObject()
	allow.Set("optionId", "allow-once")
	allow.Set("kind", "allow_once")
	options.Items = append(options.Items, allow)
	params.Set("options", options)
	decision := settings.Evaluate("session/request_permission", params)
	if decision.NegativeSum != 0 {
		t.Fatalf("positive mass leaked into the negative sum: %f", decision.NegativeSum)
	}
	if !decision.Approved || decision.Reason != "auto_approved" || decision.TopCategory != "routine_work" {
		t.Fatalf("all-positive distribution should pass the negative gate: %+v", decision)
	}
}
