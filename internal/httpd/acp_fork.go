package httpd

// ACP message actions (fork / edit-fork): a gateway-synthesized capability
// that goes beyond the Python bridge (whose fork only existed for codex via
// create.fork_copy). ACP has no native thread copy, so the branch is a new
// session on the SAME agent and working directory whose first message
// carries the transcript up to the chosen turn — exactly what the phone's
// fork dialog promises ("新会话与原会话共享工作目录").

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/google/uuid"
)

// acpForkTextsByLocale carries the branch message wording per UI locale:
// the header line and the closing instruction under the transcript.
var acpForkTextsByLocale = map[string][2]string{
	"zh-CN": {"【从 %s 分支的会话】", "以上是从 %s 分支出来的会话转录，请在此基础上继续。"},
	"en-US": {"[Session branched from %s]", "The transcript above was branched from %s. Please continue from this context."},
}

// annotateACPForkability re-arms the actionable flag on an ACP view before
// it reaches the shared projection. acpview mints every turn non-actionable
// (Python parity — neither implementation ever offered branching there), so
// the projection's editable/forkable rules would keep every ACP row false
// forever and the phone would never render the controls this file serves. A
// settled turn (anything no longer inProgress) becomes actionable; the
// timeline's own editable/forkable rules still decide which rows qualify.
func annotateACPForkability(view *jsoncompat.Object) {
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return
	}
	for _, item := range turns.Items {
		turn, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if status, _ := turn.MustGet("status").(string); status != "inProgress" {
			turn.Set("actionable", true)
		}
	}
}

// validateACPMessageFork runs the fork-side row checks under the thread's
// slot lock: the cursor row must exist, match the client's version, carry
// the action's flag (fork → the assistant forkable flag, edit-fork → the
// first-user editable flag, mirroring Python's message_action) and have
// finished.
func validateACPMessageFork(projection *timeline.Timeline, version, key, action string) (*jsoncompat.Object, string) {
	if projection == nil {
		return nil, "消息位置已变化，请刷新后重试"
	}
	row := projection.Row(key)
	if row == nil {
		return nil, "消息位置已变化，请刷新后重试"
	}
	if rowVersion, _ := row.MustGet("version").(string); rowVersion != version {
		return nil, "消息内容已变化，请重新打开编辑"
	}
	flag := "forkable"
	if action != "fork" {
		flag = "editable"
	}
	if allowed, ok := row.MustGet(flag).(bool); !ok || !allowed {
		return nil, "此消息不支持该操作"
	}
	if turnStatus, _ := row.MustGet("turnStatus").(string); turnStatus == "inProgress" {
		return nil, "请先停止当前任务，再编辑或分支"
	}
	return row, ""
}

// trimViewToTurn truncates a live snapshot's turns through the chosen turn
// (include: fork keeps the turn's own exchange) or before it (edit-fork: the
// edited text becomes the branch's new user message). The snapshot is owned
// by this request, so the turns array is trimmed in place. Reports whether
// the turn was found.
func trimViewToTurn(view *jsoncompat.Object, turnID string, include bool) bool {
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return false
	}
	index := -1
	for i, item := range turns.Items {
		if turn, ok := item.(*jsoncompat.Object); ok && stringField(turn, "id") == turnID {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	keep := index
	if include {
		keep = index + 1
	}
	kept := &jsoncompat.Array{}
	kept.Items = append(kept.Items, turns.Items[:keep]...)
	view.Set("turns", kept)
	return true
}

// acpForkMessage renders the first message injected into a branched session:
// the transcript up to the chosen turn (migrationTranscript's full budget)
// framed as a branch of the source chat. For edit-fork the edited text rides
// as the trailing user question. Unlike user-initiated sends, this
// system-generated message never carries the result-visibility convention.
func acpForkMessage(sourceLabel string, view *jsoncompat.Object, uiLocale, tail string) string {
	locale := normalizeUILocale(uiLocale)
	texts := acpForkTextsByLocale[locale]
	base := migrationTextsByLocale[locale]
	entries, truncated := migrationTranscript(view, "full")
	title, _ := view.MustGet("title").(string)
	cwd, _ := view.MustGet("cwd").(string)
	lines := []string{
		sprintf(texts[0], sourceLabel),
		base.title + title,
		base.directory + cwd,
	}
	if truncated {
		lines = append(lines, base.truncated)
	}
	lines = append(lines, "")
	for _, entry := range entries {
		role := base.roles[entry.role]
		if role == "" {
			role = entry.role
		}
		lines = append(lines, role+base.sep+entry.text)
	}
	lines = append(lines, "", sprintf(texts[1], sourceLabel))
	if tail != "" {
		lines = append(lines, "", tail)
	}
	return strings.Join(lines, "\n")
}

// handleACPMessageAction serves POST message-action for ACP-owned threads.
// fork / edit-fork synthesize a branch: a new session on the same agent and
// working directory whose first prompt carries the transcript (up to and
// including the chosen turn for fork, up to it for edit-fork, where the
// edited text rides as the branch's new user question). The in-place edit
// has no ACP counterpart and is refused with a pointer to edit-fork.
func (h *handler) handleACPMessageAction(kind string, manager *acpruntime.Manager, threadID string, body *jsoncompat.Object) {
	action := stringField(body, "action")
	identifier := stringField(body, "id")
	if _, parseErr := uuid.Parse(identifier); parseErr != nil {
		h.fail(http.StatusBadRequest, errorBody("消息操作无效"))
		return
	}
	switch action {
	case "edit":
		h.fail(http.StatusBadRequest, errorBody("当前 Agent 不支持编辑并重新发送，请使用编辑并新建分支"))
		return
	case "fork", "edit-fork":
	default:
		h.fail(http.StatusBadRequest, errorBody("消息操作无效"))
		return
	}
	text := stringField(body, "text")
	if action != "fork" && (strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 100000) {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–100000 字的消息"))
		return
	}
	key := stringField(body, "key")
	// Idempotency shares the codex message-action ledger: a retried POST with
	// the same identity returns the recorded result instead of minting a
	// duplicate branch; the same id with different content is rejected.
	messageActionMu.Lock()
	if prior, known := messageActionKnown[identifier]; known {
		same := prior.action == action && prior.key == key && prior.version == body.MustGet("version") &&
			prior.source == threadID && prior.text == text
		result := prior.result
		messageActionMu.Unlock()
		if !same {
			h.fail(http.StatusBadRequest, errorBody("同一操作标识不能用于不同内容"))
			return
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}
	messageActionMu.Unlock()

	view, _, ok := h.liveView(threadID)
	if !ok {
		h.fail(http.StatusConflict, errorBody("找不到这个会话，请稍后刷新列表"))
		return
	}
	if status, _ := view.MustGet("status").(string); status == "active" {
		h.fail(http.StatusConflict, errorBody("请先停止当前任务，再编辑或分支"))
		return
	}
	_, _, _ = h.server.refreshLiveProjection(threadID)
	// Validate the cursor row under the thread's slot lock (the SSE loop may
	// fold a new generation concurrently) and release before the create/prompt
	// calls — the lock must never span agent RPCs.
	var row *jsoncompat.Object
	var rowErr string
	if slot := h.server.lookupProjection(threadID); slot != nil {
		slot.mu.Lock()
		row, rowErr = validateACPMessageFork(slot.timeline, stringField(body, "version"), key, action)
		slot.mu.Unlock()
	} else {
		row, rowErr = validateACPMessageFork(nil, stringField(body, "version"), key, action)
	}
	if rowErr != "" {
		h.fail(http.StatusBadRequest, errorBody(rowErr))
		return
	}
	turnID, _ := row.MustGet("turnId").(string)
	if !trimViewToTurn(view, turnID, action == "fork") {
		h.fail(http.StatusConflict, errorBody("消息位置已变化，请刷新后重试"))
		return
	}

	sourceTitle := stringField(view, "title")
	if strings.TrimSpace(sourceTitle) == "" {
		sourceTitle = "未命名聊天"
	}
	cwd := manager.CwdFor(threadID)
	if cwd == "" {
		cwd = stringField(view, "cwd")
	}
	if cwd == "" {
		cwd = "."
	}
	sourceLabel := manager.Label
	if sourceLabel == "" {
		sourceLabel = kind
	}
	tail := ""
	if action == "edit-fork" {
		tail = text
	}
	message := acpForkMessage(sourceLabel, view, stringField(body, "uiLocale"), tail)

	// Same-manager creation keeps the branch on the agent that owns the
	// context (the transcript assumes its tools and model).
	child, createErr := manager.Create("sess_"+uuid.NewString(), cwd,
		truncateSubText(sourceTitle, 90)+" · 分支", 60*time.Second)
	if createErr != nil {
		h.fail(http.StatusConflict, errorBody(createErr.Error()))
		return
	}
	childID := child.ID
	if childID == threadID {
		// An agent echoing the source id back from session/new would fork a
		// session onto itself (the browser E2E caught exactly this shape).
		child.Close()
		h.fail(http.StatusConflict, errorBody("Agent 返回了与源会话相同的标识，无法创建分支"))
		return
	}
	if h.server.Forks != nil {
		h.server.Forks.Record(childID, forkOrigin{ID: threadID, Host: "local", Title: sourceTitle})
	}

	source := jsoncompat.NewObject()
	source.Set("id", threadID)
	source.Set("host", "local")
	source.Set("title", sourceTitle)
	source.Set("turnId", turnID)
	result := jsoncompat.NewObject()
	result.Set("id", childID)
	result.Set("host", "local")
	result.Set("action", action)
	result.Set("source", source)
	result.Set("status", "created")
	result.Set("draft", nil)

	// Inject the transcript like migrate does for ACP targets: Prompt submits
	// the turn and returns without waiting for it to finish, so the HTTP
	// request never blocks on the agent. A failed submit leaves the branch
	// created but its content uncertain — report that like the codex fork
	// does and pin the result so a retry cannot duplicate the child.
	sendErr := error(nil)
	session, openErr := manager.Open(childID, cwd, manager.InitializeTimeout)
	if openErr != nil {
		sendErr = openErr
	} else {
		sendErr = session.Prompt(message, 3600*time.Second)
	}
	if sendErr != nil {
		result.Set("status", "unknown")
		if action == "edit-fork" {
			result.Set("draft", text)
		}
	}
	messageActionMu.Lock()
	messageActionKnown[identifier] = &messageActionEntry{action: action, key: key,
		version: body.MustGet("version"), source: threadID, text: text, result: result}
	messageActionMu.Unlock()
	h.output(http.StatusOK, result, contentTypeJSON, "")
}
