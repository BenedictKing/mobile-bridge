package jev

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	maxRisk       = 1
	minReversible = 0.8
	minConfidence = 0.8
)

// Decision is the auditable outcome of one auto-approval evaluation: what
// Jev said (when it was asked), why the gateway did or did not auto-approve,
// and the native response to submit. Callers log it for troubleshooting;
// it never carries the API key.
type Decision struct {
	Approved   bool
	Reason     string
	Assessed   bool
	Risk       float64
	Reversible float64
	Confidence float64
	Recommend  string
	Response   *jsoncompat.Object
}

// Evaluate runs the auto-approval policy for one permission request and
// returns the full decision record. AutoResponse keeps the slim contract the
// agent bridges consume; Evaluate is what the logging wrapper calls.
func (s *Settings) Evaluate(method string, params *jsoncompat.Object) Decision {
	s.mu.Lock()
	config := s.effectiveLocked()
	s.mu.Unlock()
	if config.MustGet("autoApprove") != true {
		return Decision{Reason: "auto_approve_off"}
	}
	key, _ := config.MustGet("apiKey").(string)
	if key == "" {
		return Decision{Reason: "no_api_key"}
	}
	if !isApprovalMethod(method) {
		return Decision{Reason: "unsupported_method"}
	}
	assessment, ok := s.assess(config, key, method, params)
	if !ok {
		return Decision{Reason: "assess_failed"}
	}
	decision := Decision{
		Assessed:   true,
		Risk:       number(assessment.MustGet("risk"), 0),
		Reversible: number(assessment.MustGet("reversible"), 0),
		Confidence: number(assessment.MustGet("confidence"), 0),
	}
	decision.Recommend, _ = assessment.MustGet("recommend").(string)
	switch decision.Recommend {
	case "accept_once", "accept_session":
	case "review":
		decision.Reason = "recommend_review"
	default:
		decision.Reason = "recommend_" + decision.Recommend
	}
	if decision.Reason != "" {
		return decision
	}
	if decision.Risk > maxRisk {
		decision.Reason = "risk_too_high"
		return decision
	}
	if decision.Reversible < minReversible {
		decision.Reason = "not_reversible_enough"
		return decision
	}
	if decision.Confidence < minConfidence {
		decision.Reason = "low_confidence"
		return decision
	}
	if response := approvalResponse(method, params, decision.Recommend); response != nil {
		decision.Approved = true
		decision.Reason = "auto_approved"
		decision.Response = response
		return decision
	}
	decision.Reason = "no_matching_option"
	return decision
}

// AutoResponse evaluates an ACP permission request and returns a native
// response only when the persisted Jev policy and the remote assessment both
// satisfy the narrow automatic-approval policy.
func (s *Settings) AutoResponse(method string, params *jsoncompat.Object) *jsoncompat.Object {
	return s.Evaluate(method, params).Response
}

// approvalResponse maps a Jev recommendation onto the native reply the
// request's own options allow; nil means no eligible option exists.
func approvalResponse(method string, params *jsoncompat.Object, recommend string) *jsoncompat.Object {
	if method != "session/request_permission" && method != "session/requestPermission" {
		if recommend == "accept_once" || recommend == "accept_session" {
			result := jsoncompat.NewObject()
			result.Set("decision", "accept")
			return result
		}
		return nil
	}
	for _, value := range arrayValues(params.MustGet("options")) {
		option, _ := value.(*jsoncompat.Object)
		if option == nil {
			continue
		}
		kind := strings.ToLower(jsoncompat.PyStr(option.MustGet("kind")))
		if strings.Contains(kind, "allow") || strings.Contains(kind, "accept") {
			if recommend == "accept_session" && !strings.Contains(kind, "session") {
				continue
			}
			// ACP options carry optionId on the wire; "id" only survives in
			// already-normalized views, so prefer the wire key.
			optionID := option.MustGet("optionId")
			if optionID == nil {
				optionID = option.MustGet("id")
			}
			result := jsoncompat.NewObject()
			result.Set("optionId", optionID)
			return result
		}
	}
	return nil
}

func isApprovalMethod(method string) bool {
	switch method {
	case "session/request_permission", "session/requestPermission",
		"item/commandExecution/requestApproval", "item/fileChange/requestApproval",
		"item/permissions/requestApproval":
		return true
	}
	return false
}

// assess posts one typed-questions payload and maps the answers to the
// compact shape the policy reads: risk score, reversibility, recommendation
// and its confidence.
func (s *Settings) assess(config *jsoncompat.Object, key, method string, params *jsoncompat.Object) (*jsoncompat.Object, bool) {
	payload := jsoncompat.NewObject()
	payload.Set("model", config.MustGet("model"))
	payload.Set("state", assessmentState(method, params))
	payload.Set("questions", approvalQuestions(method, params))
	value, ok := s.post(config, key, payload)
	if !ok {
		return nil, false
	}
	result, _ := value.(*jsoncompat.Object)
	answers, _ := result.MustGet("answers").(*jsoncompat.Object)
	if answers == nil {
		return nil, false
	}
	assessment := jsoncompat.NewObject()
	assessment.Set("risk", answerNumber(answers.MustGet("risk"), "score"))
	assessment.Set("reversible", answerNumber(answers.MustGet("reversible"), "noul"))
	assessment.Set("recommend", answerString(answers.MustGet("recommend"), "choice"))
	assessment.Set("confidence", answerNumber(answers.MustGet("recommend"), "confidence"))
	return assessment, true
}

// post sends one assessment request through the optional proxy and decodes
// the JSON reply. Authorization carries the key; it never reaches a payload.
func (s *Settings) post(config *jsoncompat.Object, key string, payload *jsoncompat.Object) (any, bool) {
	body := jsoncompat.EncodeCompact(payload)
	base, _ := config.MustGet("baseUrl").(string)
	endpoint, err := url.Parse(strings.TrimRight(base, "/") + "/v1/systemone")
	if err != nil {
		return nil, false
	}
	request, err := http.NewRequest(http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 4 * time.Second}
	if proxy, _ := config.MustGet("proxy").(string); proxy != "" {
		if parsed, parseErr := url.Parse(proxy); parseErr == nil {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
		}
	}
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			_ = response.Body.Close()
		}
		return nil, false
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	value, err := jsoncompat.Unmarshal(data)
	if err != nil {
		return nil, false
	}
	return value, true
}

// assessmentState is the compact, PII-light description of what the user is
// asked to allow: identity fields, change paths and option kinds — never
// secrets, and command text stays capped.
func assessmentState(method string, params *jsoncompat.Object) *jsoncompat.Object {
	state := jsoncompat.NewObject()
	state.Set("request_kind", method)
	for _, field := range []string{"title", "command", "cwd", "reason", "permissions", "networkApprovalContext"} {
		if value, ok := params.Get(field); ok && jsoncompat.Truthy(value) {
			if text, isString := value.(string); isString {
				state.Set(field, truncateRunes(text, 4000))
			} else {
				state.Set(field, jsoncompat.Clone(value))
			}
		}
	}
	if changes := arrayValues(params.MustGet("changes")); len(changes) > 0 {
		paths := &jsoncompat.Array{}
		for index, value := range changes {
			if index >= 50 {
				break
			}
			if change, ok := value.(*jsoncompat.Object); ok {
				paths.Items = append(paths.Items, jsoncompat.PyStr(change.MustGet("path")))
			} else {
				paths.Items = append(paths.Items, truncateRunes(jsoncompat.PyStr(value), 200))
			}
		}
		state.Set("changes", paths)
	}
	options := &jsoncompat.Array{}
	for _, value := range arrayValues(params.MustGet("options")) {
		option, _ := value.(*jsoncompat.Object)
		if option == nil {
			continue
		}
		entry := jsoncompat.NewObject()
		entry.Set("kind", jsoncompat.PyStr(option.MustGet("kind")))
		label := jsoncompat.PyStr(option.MustGet("label"))
		if label == "" {
			label = jsoncompat.PyStr(option.MustGet("id"))
		}
		entry.Set("label", label)
		options.Items = append(options.Items, entry)
	}
	if len(options.Items) > 0 {
		state.Set("options", options)
	}
	return state
}

// approvalQuestions builds the typed question set the systemone API scores:
// risk rubric, reversibility, and a recommendation restricted to the
// decisions the native card actually offers (review always stays selectable
// so Jev can punt instead of guessing).
func approvalQuestions(method string, params *jsoncompat.Object) *jsoncompat.Object {
	criteria := jsoncompat.NewObject()
	for key, text := range availableDecisions(method, params) {
		criteria.Set(key, text)
	}
	criteria.Set("review", recommendText["review"])
	risk := jsoncompat.NewObject()
	risk.Set("type", "score")
	risk.Set("instructions", "Rate the risk of allowing the operation described by this permission request.")
	rubric := &jsoncompat.Array{}
	for _, item := range riskRubric {
		rubric.Items = append(rubric.Items, item)
	}
	risk.Set("criteria", rubric)
	reversible := jsoncompat.NewObject()
	reversible.Set("type", "noul")
	reversible.Set("instructions", "If allowed, can the effects of this operation be undone cheaply and completely?")
	reversibleCriteria := jsoncompat.NewObject()
	reversibleCriteria.Set("true", "The effects stay local and reversible: edited files can be restored, temporary state can be cleaned up, nothing external is modified.")
	reversibleCriteria.Set("false", "The effects are destructive, external, or persistent: deleted data, published or pushed state, changed permissions, remote side effects.")
	reversible.Set("criteria", reversibleCriteria)
	recommend := jsoncompat.NewObject()
	recommend.Set("type", "choice")
	recommend.Set("instructions", "Which decision should the user take on this permission request?")
	recommend.Set("criteria", criteria)
	questions := jsoncompat.NewObject()
	questions.Set("risk", risk)
	questions.Set("reversible", reversible)
	questions.Set("recommend", recommend)
	return questions
}

var riskRubric = []string{
	"0 - Routine. Read-only or observation-only: listing files, searching, reading configuration, showing a diff that is not applied.",
	"1 - Low. Routine reversible work inside the current project: editing workspace files, running tests or builds, installing project-local dependencies.",
	"2 - Elevated. Effects reach beyond the workspace or are hard to undo: network calls to unfamiliar hosts, changing permissions or ownership, pushing or rewriting shared git history, editing user dotfiles or app settings, using credentials.",
	"3 - High. Destructive or system-level: deleting data broadly, sudo or administrator actions, force operations, credential or security changes, anything that could make the machine or remote service unusable.",
}

var recommendText = map[string]string{
	"accept_once":    "Allow this one operation only. Prefer when the action is reasonable but the pattern is not established.",
	"accept_session": "Allow for this session when the same action will clearly repeat and stays reversible.",
	"accept_always":  "Allow permanently. Only when the action is routine and low risk for any future context.",
	"decline":        "Refuse the operation. The command or change is unsafe, unexplained, or unrelated to the requested work.",
	"review":         "No safe automatic recommendation; the user must read the details before choosing.",
}

// availableDecisions normalizes what the native card offers into
// recommendation keys with their rubric text.
func availableDecisions(method string, params *jsoncompat.Object) map[string]string {
	available := map[string]string{}
	switch method {
	case "session/request_permission", "session/requestPermission":
		for _, value := range arrayValues(params.MustGet("options")) {
			option, _ := value.(*jsoncompat.Object)
			if option == nil {
				continue
			}
			kind := strings.ToLower(jsoncompat.PyStr(option.MustGet("kind")))
			switch {
			case strings.Contains(kind, "always") && strings.Contains(kind, "allow"):
				available["accept_always"] = recommendText["accept_always"]
			case strings.Contains(kind, "allow") || strings.Contains(kind, "accept"):
				available["accept_once"] = recommendText["accept_once"]
			case strings.Contains(kind, "reject") || strings.Contains(kind, "deny") || strings.Contains(kind, "decline"):
				available["decline"] = recommendText["decline"]
			}
		}
	default:
		sawAccept, sawDecline := false, false
		for _, value := range arrayValues(params.MustGet("availableDecisions")) {
			decision := jsoncompat.PyStr(value)
			switch decision {
			case "accept":
				sawAccept = true
			case "decline", "cancel":
				sawDecline = true
			}
		}
		if !sawAccept && !sawDecline {
			return map[string]string{
				"accept_once": recommendText["accept_once"],
				"decline":     recommendText["decline"],
			}
		}
		if sawAccept {
			available["accept_once"] = recommendText["accept_once"]
		}
		if sawDecline {
			available["decline"] = recommendText["decline"]
		}
	}
	if len(available) == 0 {
		available["review"] = recommendText["review"]
	}
	return available
}

func arrayValues(value any) []any {
	array, _ := value.(*jsoncompat.Array)
	if array == nil {
		return nil
	}
	return array.Items
}

func answerString(value any, key string) any {
	object, _ := value.(*jsoncompat.Object)
	if object == nil {
		return nil
	}
	return object.MustGet(key)
}

func answerNumber(value any, key string) float64 {
	return number(answerString(value, key), 0)
}

func number(value any, fallback float64) float64 {
	switch item := value.(type) {
	case json.Number:
		parsed, err := item.Float64()
		if err == nil {
			return parsed
		}
	case float64:
		return item
	case int:
		return float64(item)
	}
	return fallback
}

// truncateRunes caps logged or sent text at rune boundaries.
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
