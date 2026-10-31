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
	// 两个 Noul 信号的路由带（对齐 jev-guard 的 approval≥0.75 ask 与官方
	// 0.7/0.3 双侧带）：needs_human 是「该停下来找人看」的概率，0.5 以上
	// 复核；unsafe 是「不安全/离题、该拒绝」的信号，0.6 以上推荐拒绝。
	// 不再设独立的置信度闸：Choice 的 confidence 是集中度指标
	// (p_max−1/n)/(1−1/n)，三选项下过 0.8 需要 p_max≈0.87，数学上几乎
	// 不可达，正是自动审核从不放行的旧根因；Noul 的 p 本身就是不确定度。
	holdHumanAt     = 0.5
	declineUnsafeAt = 0.6
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
	// 建议由两个 Noul 信号在代码里推导（控制流留在代码侧，问题只做窄判断）：
	// unsafe 高 → 拒绝；needs_human 高 → 人工复核；否则本次允许。
	recommend := "accept_once"
	switch {
	case assessment.unsafe >= declineUnsafeAt:
		recommend = "decline"
	case assessment.needsHuman >= holdHumanAt:
		recommend = "review"
	}
	decision := Decision{
		Assessed:    true,
		Categories:  assessment.categories,
		NegativeSum: assessment.negativeSum,
		Threshold:   threshold,
		Reversible:  assessment.reversible,
		// 徽章上的「Jev N%」语义是放行把握：不需要人看的概率。
		Confidence:  1 - assessment.needsHuman,
		Recommend:   recommend,
		TopCategory: assessment.topCategory,
	}
	switch recommend {
	case "accept_once":
	case "review":
		decision.Reason = "recommend_review"
	default:
		decision.Reason = "recommend_" + recommend
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
	needsHuman  float64
	unsafe      float64
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
	needsHuman, ok := noulAnswer(answers, "needs_human")
	if !ok {
		return nil, false
	}
	unsafe, ok := noulAnswer(answers, "unsafe")
	if !ok {
		return nil, false
	}
	return &assessment{
		categories:  categories,
		negativeSum: negativeSum,
		topCategory: topCategory,
		reversible:  answerNumber(answers.MustGet("reversible"), "noul"),
		needsHuman:  needsHuman,
		unsafe:      unsafe,
	}, true
}

// noulAnswer reads one yes-probability. A missing or out-of-range answer fails
// closed the way pi-jev-auto-mode treats malformed responses: the gate never
// silently turns a broken reply into an approval.
func noulAnswer(answers *jsoncompat.Object, key string) (float64, bool) {
	object, _ := answers.MustGet(key).(*jsoncompat.Object)
	if object == nil {
		return 0, false
	}
	value := number(object.MustGet("noul"), math.NaN())
	if math.IsNaN(value) || value < 0 || value > 1 {
		return 0, false
	}
	return value, true
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

// approvalQuestions builds the typed question set the systemone API scores
// atomically (one narrow judgment each, the way the TypeSafe guide and the
// jev-guard/pi-jev-auto-mode gates phrase theirs): a policy-category outcome
// choice (the answer's probabilities carry the full distribution),
// reversibility, and two yes/no signals — needs_human for routing to a human
// and unsafe for recommending refusal. The recommendation itself is derived
// in code from those answers, never asked.
func approvalQuestions(policy []Category, method string, params *jsoncompat.Object) *jsoncompat.Object {
	criteria := jsoncompat.NewObject()
	for _, category := range policy {
		criteria.Set(category.Name, category.Description)
	}
	outcome := jsoncompat.NewObject()
	outcome.Set("type", "choice")
	outcome.Set("instructions", "Which category best describes this permission request? Read it the way the agent will carry it out — the command line with pipes and substitutions, the files and paths changed, the services touched — and judge by effects, not by verbs: fetched content counts as code only when a later stage executes or evaluates it. Probabilities are what a careful reviewer would bet, so when one category plainly fits, give it most of the mass and split only a small remainder; do not spread probability across negative categories defensively. Prefer a positive category describing ordinary or protective work when it fits.")
	outcome.Set("criteria", criteria)
	reversible := jsoncompat.NewObject()
	reversible.Set("type", "noul")
	reversible.Set("instructions", "If allowed, can the effects of this operation be undone cheaply and completely?")
	reversibleCriteria := jsoncompat.NewObject()
	reversibleCriteria.Set("true", "The effects stay local and reversible: edited files can be restored, temporary state can be cleaned up, nothing external is modified. Read-only lookups and network fetches that change no remote state are fully reversible.")
	reversibleCriteria.Set("false", "The effects are destructive, external, or persistent: deleted data, published or pushed state, changed permissions, remote side effects.")
	reversible.Set("criteria", reversibleCriteria)
	needsHuman := jsoncompat.NewObject()
	needsHuman.Set("type", "noul")
	needsHuman.Set("instructions", "Should a careful engineer pause and ask a human before this exact call runs? Judge by effects: destroying data, exposing secrets, weakening security, running fetched content as code, escalating privileges, disrupting services, or writing outside the expected scope all warrant a human — so does a command you genuinely cannot decode. Ordinary read-only lookups, plainly reversible work, and requests that match the agent's stated task do not.")
	needsHumanCriteria := jsoncompat.NewObject()
	needsHumanCriteria.Set("true", "A human should look at this before it runs: destructive, secret-revealing, security-weakening, executing fetched code, privilege-escalating, service-disrupting, out of scope — or genuinely unreadable.")
	needsHumanCriteria.Set("false", "The call is ordinary or plainly reversible and matches the work the agent described; letting it run unattended is fine.")
	needsHuman.Set("criteria", needsHumanCriteria)
	unsafe := jsoncompat.NewObject()
	unsafe.Set("type", "noul")
	unsafe.Set("instructions", "Is this call unsafe, unexplained, or unrelated to the requested work — something to refuse outright rather than merely confirm?")
	unsafeCriteria := jsoncompat.NewObject()
	unsafeCriteria.Set("true", "The command or change should be refused: unsafe, unexplained, or unrelated to the requested work.")
	unsafeCriteria.Set("false", "The call is a reasonable part of the requested work.")
	unsafe.Set("criteria", unsafeCriteria)
	questions := jsoncompat.NewObject()
	questions.Set("outcome", outcome)
	questions.Set("reversible", reversible)
	questions.Set("needs_human", needsHuman)
	questions.Set("unsafe", unsafe)
	return questions
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
