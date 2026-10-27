package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	minReversible = 0.8
	minConfidence = 0.8
)

// Decision is the auditable outcome of one auto-approval evaluation: what
// Jev said (when it was asked), why the gateway did or did not auto-approve,
// and the native response to submit. Categories carries the per-policy-class
// probabilities and NegativeSum their negative total compared against
// Threshold. Callers log it for troubleshooting; it never carries the API key.
type Decision struct {
	Approved    bool
	Reason      string
	Assessed    bool
	Categories  map[string]float64
	NegativeSum float64
	Threshold   float64
	Reversible  float64
	Confidence  float64
	Recommend   string
	TopCategory string
	Response    *jsoncompat.Object
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
	return s.evaluate(config, key, method, params)
}

// evaluate is the assessment itself, past the enable/key/method gates: one
// remote call, then the gate order recommend → negative sum → reversibility
// → confidence → native response mapping.
func (s *Settings) evaluate(config *jsoncompat.Object, key, method string, params *jsoncompat.Object) Decision {
	policy, _, _ := parsePolicyValue(config.MustGet("policy"))
	threshold := number(config.MustGet("threshold"), defaultThreshold)
	assessment, ok := s.assess(config, key, method, params, policy)
	if !ok {
		return Decision{Reason: "assess_failed", Threshold: threshold}
	}
	decision := Decision{
		Assessed:    true,
		Categories:  assessment.categories,
		NegativeSum: assessment.negativeSum,
		Threshold:   threshold,
		Reversible:  assessment.reversible,
		Confidence:  assessment.confidence,
		Recommend:   assessment.recommend,
		TopCategory: assessment.topCategory,
	}
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
	if decision.NegativeSum >= decision.Threshold {
		decision.Reason = "negative_sum_over_threshold"
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

// DecisionPayload renders a Decision for the trial API: category
// probabilities, the verdict numbers and the reason. Every float passes
// through numFloat — writeValue has no float64 branch — and neither the
// native response nor any key material ever appears here.
func DecisionPayload(d Decision) *jsoncompat.Object {
	payload := jsoncompat.NewObject()
	payload.Set("assessed", d.Assessed)
	payload.Set("approved", d.Approved)
	payload.Set("reason", d.Reason)
	categories := jsoncompat.NewObject()
	names := make([]string, 0, len(d.Categories))
	for name := range d.Categories {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		categories.Set(name, numFloat(d.Categories[name]))
	}
	payload.Set("categories", categories)
	payload.Set("negative_sum", numFloat(d.NegativeSum))
	payload.Set("threshold", numFloat(d.Threshold))
	payload.Set("reversible", numFloat(d.Reversible))
	payload.Set("confidence", numFloat(d.Confidence))
	payload.Set("recommend", d.Recommend)
	payload.Set("top_category", d.TopCategory)
	return payload
}

// TrialEvaluate runs the same assessment chain as a live approval, strictly
// as a dry run: it never touches the session cache or the decision journal
// and ignores the auto-approve switch — a configured API key is the only
// prerequisite.
func (s *Settings) TrialEvaluate(method string, params *jsoncompat.Object) (Decision, error) {
	if params == nil {
		params = jsoncompat.NewObject()
	}
	s.mu.Lock()
	config := s.effectiveLocked()
	s.mu.Unlock()
	key, _ := config.MustGet("apiKey").(string)
	if key == "" {
		return Decision{}, errors.New("未配置 Jev API Key，无法试评")
	}
	if !isApprovalMethod(method) {
		return Decision{Reason: "unsupported_method"}, nil
	}
	return s.evaluate(config, key, method, params), nil
}

// EvaluateSession runs the approval policy for one live permission request,
// with the session-scoped approval cache in front of the remote assessment.
// The cache is keyed by (agent, session, method, normalized subject) and
// versioned by the effective-config fingerprint, so any settings change
// invalidates it. Cached entries never store the native response: the hit is
// remapped onto the current card's options, and a card offering no matching
// option falls through to a fresh evaluation. Question cards skip the cache
// entirely — one title can carry many different questions. Every decision
// that entered the evaluation chain (cache hits included) lands in the
// decision journal; the pre-evaluation early exits never do.
func (s *Settings) EvaluateSession(agent, sessionID, requestID, method string, params *jsoncompat.Object) Decision {
	if params == nil {
		params = jsoncompat.NewObject()
	}
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
	subject := normalizeSubject(params)
	if subject != "" {
		fingerprint := configFingerprint(config)
		if entry, ok := s.cache.lookup(fingerprint, agent, sessionID, method, subject); ok {
			if response := approvalResponse(method, params, entry.recommend); response != nil {
				decision := Decision{
					Approved:    true,
					Reason:      "cache_hit",
					Assessed:    true,
					Categories:  entry.categories,
					NegativeSum: entry.negativeSum,
					Threshold:   entry.threshold,
					Reversible:  entry.reversible,
					Confidence:  entry.confidence,
					Recommend:   entry.recommend,
					TopCategory: entry.topCategory,
					Response:    response,
				}
				s.recordDecision(agent, sessionID, requestID, method, decision, params)
				return decision
			}
			// The current card offers no eligible option for the cached
			// recommendation (fresh option ids on every card): treat the hit
			// as a miss rather than replying with a stale optionId.
		}
		decision := s.evaluate(config, key, method, params)
		if decision.Approved {
			s.cache.store(fingerprint, agent, sessionID, method, subject, decision)
		}
		s.recordDecision(agent, sessionID, requestID, method, decision, params)
		return decision
	}
	decision := s.evaluate(config, key, method, params)
	s.recordDecision(agent, sessionID, requestID, method, decision, params)
	return decision
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
			if recommend == "accept_once" && !strings.Contains(kind, "once") {
				continue
			}
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

// assessment is one parsed remote verdict: the policy-category probability
// distribution plus the auxiliary gates' inputs.
type assessment struct {
	categories  map[string]float64
	negativeSum float64
	topCategory string
	reversible  float64
	confidence  float64
	recommend   string
}

// assess posts one typed-questions payload and folds the answers into the
// policy verdict. The outcome answer must carry a full probability
// distribution over every policy category (mirroring the reference client's
// normalizeAnswer); anything short of that fails closed instead of guessing.
func (s *Settings) assess(config *jsoncompat.Object, key, method string, params *jsoncompat.Object, policy []Category) (*assessment, bool) {
	payload := jsoncompat.NewObject()
	payload.Set("model", config.MustGet("model"))
	payload.Set("state", assessmentState(method, params))
	payload.Set("questions", approvalQuestions(policy, method, params))
	value, ok := s.post(config, key, payload)
	if !ok {
		return nil, false
	}
	result, _ := value.(*jsoncompat.Object)
	answers, _ := result.MustGet("answers").(*jsoncompat.Object)
	if answers == nil {
		return nil, false
	}
	outcome, _ := answers.MustGet("outcome").(*jsoncompat.Object)
	if outcome == nil {
		return nil, false
	}
	probabilities, _ := outcome.MustGet("probabilities").(*jsoncompat.Object)
	if probabilities == nil {
		return nil, false
	}
	choice, _ := outcome.MustGet("choice").(string)
	known := map[string]bool{}
	for _, category := range policy {
		known[category.Name] = true
	}
	if !known[choice] {
		return nil, false
	}
	negative := negativeNames(policy)
	categories := map[string]float64{}
	negativeSum := 0.0
	for _, category := range policy {
		raw, exists := probabilities.Get(category.Name)
		if !exists {
			return nil, false
		}
		probability := number(raw, math.NaN())
		if math.IsNaN(probability) || math.IsInf(probability, 0) || probability < 0 || probability > 1 {
			return nil, false
		}
		probability = math.Min(math.Max(probability, 0), 1)
		categories[category.Name] = probability
		if negative[category.Name] {
			negativeSum += probability
		}
	}
	topCategory := ""
	topProbability := -1.0
	for _, category := range policy {
		if categories[category.Name] > topProbability {
			topProbability = categories[category.Name]
			topCategory = category.Name
		}
	}
	return &assessment{
		categories:  categories,
		negativeSum: negativeSum,
		topCategory: topCategory,
		reversible:  answerNumber(answers.MustGet("reversible"), "noul"),
		confidence:  answerNumber(answers.MustGet("recommend"), "confidence"),
		recommend:   stringOf(answerString(answers.MustGet("recommend"), "choice")),
	}, true
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

// permissionRequestTitle is the placeholder title PermissionCard substitutes
// for tool calls without one; it identifies no subject, so subject derivation
// falls through to the command.
const permissionRequestTitle = "权限请求"

// subjectOf derives the card's human subject for state and logs: normalized
// cards carry title and command at the top level (the codex bridge), while
// raw ACP session/request_permission params nest them under toolCall.
func subjectOf(params *jsoncompat.Object) (title, command string, question bool) {
	if params == nil {
		return "", "", false
	}
	if title, _ := params.MustGet("title").(string); title != "" {
		command, _ := params.MustGet("command").(string)
		return title, command, false
	}
	if command, _ := params.MustGet("command").(string); command != "" {
		return "", command, false
	}
	if params.MustGet("toolCall") != nil {
		return acpview.PermissionCard(params)
	}
	return "", "", false
}

// assessmentState is the compact, PII-light description of what the user is
// asked to allow: identity fields, change paths and option kinds — never
// secrets, and command text stays capped. Raw ACP requests carry the command
// inside toolCall, so subjectOf fills title/command when the top level has
// none (normalized cards already provide them there).
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
	if params.MustGet("toolCall") != nil {
		if title, command, _ := subjectOf(params); title != "" || command != "" {
			if _, exists := state.Get("title"); !exists && title != "" {
				state.Set("title", title)
			}
			if _, exists := state.Get("command"); !exists && command != "" {
				state.Set("command", truncateRunes(command, 4000))
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
// a policy-category outcome choice (the answer's probabilities carry the full
// distribution), reversibility, and a recommendation restricted to the
// decisions the native card actually offers (review always stays selectable
// so Jev can punt instead of guessing).
func approvalQuestions(policy []Category, method string, params *jsoncompat.Object) *jsoncompat.Object {
	criteria := jsoncompat.NewObject()
	for _, category := range policy {
		criteria.Set(category.Name, category.Description)
	}
	outcome := jsoncompat.NewObject()
	outcome.Set("type", "choice")
	outcome.Set("instructions", "Which category best describes this permission request? Read it the way the agent will carry it out: the command line with pipes and substitutions, the files and paths changed, the services touched. Prefer a positive category describing ordinary or protective work when it fits.")
	outcome.Set("criteria", criteria)
	reversible := jsoncompat.NewObject()
	reversible.Set("type", "noul")
	reversible.Set("instructions", "If allowed, can the effects of this operation be undone cheaply and completely?")
	reversibleCriteria := jsoncompat.NewObject()
	reversibleCriteria.Set("true", "The effects stay local and reversible: edited files can be restored, temporary state can be cleaned up, nothing external is modified.")
	reversibleCriteria.Set("false", "The effects are destructive, external, or persistent: deleted data, published or pushed state, changed permissions, remote side effects.")
	reversible.Set("criteria", reversibleCriteria)
	recommendCriteria := jsoncompat.NewObject()
	for key, text := range availableDecisions(method, params) {
		recommendCriteria.Set(key, text)
	}
	recommendCriteria.Set("review", recommendText["review"])
	recommend := jsoncompat.NewObject()
	recommend.Set("type", "choice")
	recommend.Set("instructions", "Which decision should the user take on this permission request?")
	recommend.Set("criteria", recommendCriteria)
	questions := jsoncompat.NewObject()
	questions.Set("outcome", outcome)
	questions.Set("reversible", reversible)
	questions.Set("recommend", recommend)
	return questions
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

func stringOf(value any) string {
	text, _ := value.(string)
	return text
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
