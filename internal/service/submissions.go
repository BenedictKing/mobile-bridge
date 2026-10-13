// Package service: the submission ledger and queue state machine —
// send/queue/retry/cancel/edit/ignore with the dispatch outcome branches
// (accepted / failed / requeued / unknown). Parity: conformance/submissions
// goldens.
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Error kinds mirror the Python exception class names the oracle records.
const (
	KindValueError        = "ValueError"
	KindTurnNotDispatched = "TurnNotDispatched"
	KindAcpTimeout        = "AcpTimeout"
	KindIPCError          = "IPCError"
)

// KindedError carries the Python exception class name for parity output.
type KindedError struct {
	Kind    string
	Message string
}

func (e *KindedError) Error() string { return e.Message }

func valueError(message string) error { return &KindedError{KindValueError, message} }
func ipcError(message string) error   { return &KindedError{KindIPCError, message} }
func acpTimeout(message string) error { return &KindedError{KindAcpTimeout, message} }
func notDispatched(message string) error {
	return &KindedError{KindTurnNotDispatched, message}
}

// SubmissionAdapter is the adapter surface the ledger needs.
type SubmissionAdapter interface {
	Label() string
	HasCapability(name string) bool
	IsActive(session *LiveSession) bool
	// DispatchTurn returns the agent response or a KindedError
	// (TurnNotDispatched / ValueError / AcpTimeout kinds).
	DispatchTurn(session *LiveSession, submissionID string, entry *jsoncompat.Object) (any, error)
}

const (
	statusQueued    = "queued"
	statusUnknown   = "unknown"
	statusAccepted  = "accepted"
	statusFailed    = "failed"
	statusIgnored   = "ignored"
	statusCancelled = "cancelled"
)

func (b *Bridge) entry(key string) *jsoncompat.Object { return b.Submissions[key] }

func statusOf(entry *jsoncompat.Object) string {
	status, _ := entry.MustGet("status").(string)
	return status
}

func (b *Bridge) setStatus(key, status string) {
	b.Submissions[key].Set("status", status)
}

// Send mirrors Bridge.send (non-goal path): validation, dedup, ledger entry
// construction and immediate dispatch unless queued.
func (b *Bridge) Send(session *LiveSession, text, submissionID, mode string,
	skills []any, workMode any) (any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, valueError("请输入 1–100000 字的消息")
	}
	if len(text) > 100000 {
		return nil, valueError("请输入 1–100000 字的消息")
	}
	switch mode {
	case "send", "steer", "queue":
	default:
		return nil, valueError("未知发送方式")
	}
	if workMode != nil {
		if mode, ok := workMode.(string); ok {
			switch mode {
			case "default", "plan":
			default:
				return nil, valueError("未知工作模式")
			}
		} else {
			return nil, valueError("未知工作模式")
		}
	}
	if mode == "steer" && workMode != nil {
		return nil, valueError("补充当前任务时不能切换工作模式")
	}
	if mode == "steer" && !b.Adapter.HasCapability("steer") {
		return nil, valueError("当前 Agent 不支持补充任务，请使用排队发送")
	}

	key := session.ID + ":" + submissionID
	if prior := b.entry(key); prior != nil {
		same := textOf(prior) == text &&
			modeOf(prior) == mode &&
			strings.EqualFold(string(jsoncompat.EncodeCompact(skillsOf(prior))),
				string(jsoncompat.EncodeCompact(&jsoncompat.Array{Items: skills}))) &&
			valueEqual(fieldOr(prior, "workMode", nil), workMode)
		if same {
			result := jsoncompat.NewObject()
			result.Set("status", statusOf(prior))
			result.Set("duplicate", true)
			result.Set("id", submissionID)
			return result, nil
		}
		return nil, valueError("同一消息标识不能用于不同内容")
	}

	active := b.Adapter.IsActive(session)
	if active && mode == "send" {
		return nil, valueError(fmt.Sprintf("%s 正在执行。请选择「排队发送」或「补充当前任务」。",
			b.Adapter.Label()))
	}
	if !active && mode == "steer" {
		return nil, valueError("当前任务已经结束，请使用普通发送")
	}
	if workMode != nil && !b.stateHasModel(session) {
		return nil, valueError("尚未取得桌面模型设置，请重新连接后再切换模式")
	}

	entry := jsoncompat.NewObject()
	entry.Set("text", text)
	entry.Set("mode", mode)
	entry.Set("skills", &jsoncompat.Array{Items: skills})
	if mode == "queue" {
		entry.Set("status", statusQueued)
	} else {
		entry.Set("status", statusUnknown)
	}
	entry.Set("at", jsoncompat.Num(1000000000))
	if workMode != nil {
		entry.Set("workMode", workMode)
	}
	b.recordLedger(key, entry)
	session.Changed()
	if mode == "queue" {
		result := jsoncompat.NewObject()
		result.Set("status", statusQueued)
		result.Set("id", submissionID)
		return result, nil
	}
	return b.dispatch(session, key)
}

func textOf(entry *jsoncompat.Object) string {
	text, _ := entry.MustGet("text").(string)
	return text
}

func modeOf(entry *jsoncompat.Object) string {
	mode, _ := entry.MustGet("mode").(string)
	return mode
}

func skillsOf(entry *jsoncompat.Object) *jsoncompat.Array {
	if list, ok := entry.MustGet("skills").(*jsoncompat.Array); ok {
		return list
	}
	return &jsoncompat.Array{}
}

func fieldOr(entry *jsoncompat.Object, key string, fallback any) any {
	if value, ok := entry.Get(key); ok {
		return value
	}
	return fallback
}

func (b *Bridge) stateHasModel(session *LiveSession) bool {
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil {
		return false
	}
	if jsoncompat.Truthy(state.MustGet("latestModel")) {
		return true
	}
	if mode := objectOf(state.MustGet("latestCollaborationMode")); mode != nil {
		if settings := objectOf(mode.MustGet("settings")); settings != nil {
			return jsoncompat.Truthy(settings.MustGet("model"))
		}
	}
	return false
}

// dispatch mirrors Bridge._dispatch with its three outcome branches.
func (b *Bridge) dispatch(session *LiveSession, key string) (any, error) {
	submissionID := key[strings.Index(key, ":")+1:]
	entry := b.entry(key)
	response, err := b.Adapter.DispatchTurn(session, submissionID, entry)
	if err != nil {
		kinded := err.(*KindedError)
		switch kinded.Kind {
		case KindTurnNotDispatched:
			b.requeueSubmission(key, kinded.Message)
		case KindValueError:
			b.failSubmission(key, kinded.Message)
		}
		return nil, err
	}
	b.setStatus(key, statusAccepted)
	session.Changed()
	result := jsoncompat.NewObject()
	result.Set("status", statusAccepted)
	result.Set("id", submissionID)
	result.Set("result", response)
	return result, nil
}

// requeueSubmission mirrors _requeue_submission: the manual-retry path never
// climbs the counter because only unknown-status entries count.
func (b *Bridge) requeueSubmission(key, message string) {
	entry := b.entry(key)
	if entry == nil || statusOf(entry) != statusUnknown {
		return
	}
	attempts := 1
	if value, ok := entry.Get("queueAttempts"); ok {
		if number, isNumber := value.(json.Number); isNumber {
			fmt.Sscanf(number.String(), "%d", &attempts)
			attempts++
		}
	}
	entry.Set("queueAttempts", jsoncompat.Num(attempts))
	entry.Set("lastQueueError", truncateRunes(message, 500))
	if attempts >= 10 {
		entry.Set("status", statusFailed)
	} else {
		entry.Set("status", statusQueued)
	}
}

func (b *Bridge) failSubmission(key, message string) {
	entry := b.entry(key)
	if entry == nil || statusOf(entry) != statusUnknown {
		return
	}
	entry.Set("status", statusFailed)
	entry.Set("error", truncateRunes(message, 500))
}

// MarkSubmissionFailed mirrors mark_submission_failed: definitive agent-side
// rejection of an accepted dispatch; other statuses are a silent no-op.
func (b *Bridge) MarkSubmissionFailed(threadID, submissionID, message string) {
	entry := b.entry(threadID + ":" + submissionID)
	if entry == nil || statusOf(entry) != statusAccepted {
		return
	}
	entry.Set("status", statusFailed)
	entry.Set("error", truncateRunes(message, 500))
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// QueueAction mirrors queue_action: delete / edit / send.
func (b *Bridge) QueueAction(session *LiveSession, submissionID, action string, text any) (any, error) {
	switch action {
	case "delete":
		return b.cancelQueued(session, submissionID)
	case "edit":
		return b.editQueued(session, submissionID, text)
	case "send":
		return b.sendQueuedNow(session, submissionID)
	}
	return nil, valueError("未知队列操作")
}

func (b *Bridge) queuedEntry(session *LiveSession, submissionID string,
	statuses ...string) (*jsoncompat.Object, error) {
	entry := b.entry(session.ID + ":" + submissionID)
	if entry == nil || !containsStatus(statuses, statusOf(entry)) {
		return nil, valueError("此消息已离开队列，请查看会话")
	}
	return entry, nil
}

func containsStatus(statuses []string, status string) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func (b *Bridge) cancelQueued(session *LiveSession, submissionID string) (any, error) {
	entry, err := b.queuedEntry(session, submissionID, statusQueued, statusFailed)
	if err != nil {
		return nil, err
	}
	entry.Set("status", statusCancelled)
	session.Changed()
	result := jsoncompat.NewObject()
	result.Set("status", statusCancelled)
	return result, nil
}

func (b *Bridge) editQueued(session *LiveSession, submissionID string, text any) (any, error) {
	value, isString := text.(string)
	if !isString || strings.TrimSpace(value) == "" || len(value) > 100000 {
		return nil, valueError("请输入 1–100000 字的消息")
	}
	entry, err := b.queuedEntry(session, submissionID, statusQueued, statusFailed)
	if err != nil {
		return nil, err
	}
	if _, planned := entry.Get("planResponse"); planned {
		return nil, valueError("此类排队消息不能编辑")
	}
	if _, activation := entry.Get("goalActivation"); activation {
		return nil, valueError("此类排队消息不能编辑")
	}
	entry.Set("text", value)
	entry.Set("at", jsoncompat.Num(1000000000))
	entry.Delete("queueAttempts")
	entry.Delete("error")
	entry.Delete("lastQueueError")
	entry.Set("status", statusQueued)
	session.Changed()
	result := jsoncompat.NewObject()
	result.Set("status", statusQueued)
	result.Set("id", submissionID)
	result.Set("text", value)
	return result, nil
}

func (b *Bridge) sendQueuedNow(session *LiveSession, submissionID string) (any, error) {
	entry, err := b.queuedEntry(session, submissionID, statusQueued, statusFailed)
	if err != nil {
		return nil, err
	}
	if !session.Connected {
		message := "会话尚未连接，请重新打开"
		if session.Error != nil {
			if text, ok := session.Error.(string); ok && text != "" {
				message = text
			}
		}
		return nil, ipcError(message)
	}
	if b.Adapter.IsActive(session) {
		return nil, valueError("当前任务仍在运行，请等待完成后再立即发送")
	}
	// A manual send resets the attempt ladder before dispatching.
	entry.Delete("queueAttempts")
	entry.Delete("error")
	entry.Delete("lastQueueError")
	entry.Set("status", statusUnknown)
	result, dispatchErr := b.dispatch(session, session.ID+":"+submissionID)
	session.Changed()
	return result, dispatchErr
}

// IgnoreSubmission mirrors ignore_submission: only unknown (or already
// ignored) outcomes can be hidden from the queue list.
func (b *Bridge) IgnoreSubmission(session *LiveSession, submissionID string) (any, error) {
	entry := b.entry(session.ID + ":" + submissionID)
	if entry == nil || !containsStatus([]string{statusUnknown, statusIgnored}, statusOf(entry)) {
		return nil, valueError("只能忽略发送结果未确认的记录")
	}
	entry.Set("status", statusIgnored)
	session.Changed()
	result := jsoncompat.NewObject()
	result.Set("status", statusIgnored)
	result.Set("id", submissionID)
	return result, nil
}

// SubmissionMeta mirrors _submission_meta for the regular ledger (goal
// commands and inputs stay empty in this slice).
func (b *Bridge) SubmissionMeta(session *LiveSession) *jsoncompat.Object {
	view := jsoncompat.NewObject()
	submissions := &jsoncompat.Array{}
	for _, pair := range b.orderedLedger(session.ID) {
		key, entry := pair[0].(string), pair[1].(*jsoncompat.Object)
		if !containsStatus([]string{statusQueued, statusUnknown, statusFailed}, statusOf(entry)) {
			continue
		}
		submissionID := key[strings.Index(key, ":")+1:]
		row := jsoncompat.NewObject()
		row.Set("id", submissionID)
		row.Set("text", textOf(entry))
		row.Set("status", statusOf(entry))
		row.Set("editable", true)
		if names, ok := entry.Get("attachmentNames"); ok && jsoncompat.Truthy(names) {
			row.Set("attachments", names)
		}
		if message, ok := entry.Get("error"); ok && jsoncompat.Truthy(message) {
			row.Set("error", message)
		} else if message, ok := entry.Get("lastQueueError"); ok && jsoncompat.Truthy(message) {
			row.Set("error", message)
		}
		submissions.Items = append(submissions.Items, row)
	}
	view.Set("submissions", submissions)
	view.Set("goalSubmission", nil)
	return view
}

// orderedLedger lists this session's entries in insertion order.
func (b *Bridge) orderedLedger(threadID string) [][2]any {
	pairs := [][2]any{}
	for _, key := range b.ledgerOrder {
		if entry, ok := b.Submissions[key]; ok && strings.HasPrefix(key, threadID+":") {
			pairs = append(pairs, [2]any{key, entry})
		}
	}
	return pairs
}

var _ = errors.New
