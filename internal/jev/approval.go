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

// AutoResponse evaluates an ACP permission request and returns a native
// response only when the persisted Jev policy and the remote assessment both
// satisfy the narrow automatic-approval policy.
func (s *Settings) AutoResponse(method string, params *jsoncompat.Object) *jsoncompat.Object {
	s.mu.Lock()
	config := s.effectiveLocked()
	s.mu.Unlock()
	if config.MustGet("autoApprove") != true {
		return nil
	}
	key, _ := config.MustGet("apiKey").(string)
	if key == "" || !isApprovalMethod(method) {
		return nil
	}
	assessment, ok := s.assess(config, key, method, params)
	if !ok || assessment.MustGet("recommend") != "accept_once" && assessment.MustGet("recommend") != "accept_session" {
		return nil
	}
	if number(assessment.MustGet("risk"), 99) > maxRisk ||
		number(assessment.MustGet("reversible"), 0) < minReversible ||
		number(assessment.MustGet("confidence"), 0) < minConfidence {
		return nil
	}
	recommend, _ := assessment.MustGet("recommend").(string)
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
			result := jsoncompat.NewObject()
			result.Set("optionId", option.MustGet("id"))
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

func (s *Settings) assess(config *jsoncompat.Object, key, method string, params *jsoncompat.Object) (*jsoncompat.Object, bool) {
	state := jsoncompat.NewObject()
	state.Set("request_kind", method)
	for _, field := range []string{"title", "command", "cwd", "reason", "permissions", "networkApprovalContext"} {
		if value, ok := params.Get(field); ok && jsoncompat.Truthy(value) {
			state.Set(field, jsoncompat.Clone(value))
		}
	}
	payload := jsoncompat.NewObject()
	payload.Set("model", config.MustGet("model"))
	payload.Set("state", state)
	payload.Set("questions", approvalQuestions())
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
	value, err := jsoncompat.Unmarshal(data)
	if err != nil {
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

func approvalQuestions() *jsoncompat.Object {
	questions := jsoncompat.NewObject()
	questions.Set("risk", "Rate risk from 0 (routine) to 3 (high).")
	questions.Set("reversible", "Can the operation be undone cheaply and completely?")
	questions.Set("recommend", "Choose accept_once, accept_session, or review.")
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
