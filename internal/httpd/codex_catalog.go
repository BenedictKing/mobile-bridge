package httpd

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/google/uuid"
)

// Codex-side feature surface that used to live in the Python bridge:
// the runtime catalog (models/Skills/fast-mode), thread rename and the
// in-place message edit.

// handleCodexCatalog serves GET catalog for codex-owned threads: models with
// fast-mode metadata folded with the live thread's model/effort/provider,
// or the Skill list filtered and paged.
func (h *handler) handleCodexCatalog(threadID string, query map[string][]string) {
	empty := func() {
		result := jsoncompat.NewObject()
		result.Set("kind", queryValue(query, "kind", "models"))
		result.Set("models", &jsoncompat.Array{})
		result.Set("skills", &jsoncompat.Array{})
		result.Set("selectedSkills", &jsoncompat.Array{})
		result.Set("total", jsoncompat.Num(0))
		result.Set("currentModel", nil)
		result.Set("currentEffort", nil)
		h.output(http.StatusOK, result, contentTypeJSON, "")
	}
	if h.server.Codex == nil {
		empty()
		return
	}
	offset := intQuery(query, "offset", 0)
	if offset < 0 {
		h.fail(http.StatusBadRequest, errorBody("Skill 分页参数无效"))
		return
	}
	limit := intQuery(query, "limit", 200)
	if limit < 1 || limit > 500 {
		h.fail(http.StatusBadRequest, errorBody("Skill 分页参数无效"))
		return
	}
	ids := queryIDs(query)
	if len(ids) > 8 {
		h.fail(http.StatusBadRequest, errorBody("最多关联 8 个已选 Skill"))
		return
	}
	refresh := queryValue(query, "refresh", "") == "true"
	cwd := h.codexCatalogCwd(threadID)
	view, _, ok := h.liveView(threadID)
	if !ok {
		view = jsoncompat.NewObject()
	}
	catalog := h.server.Codex.Catalog()
	kind := queryValue(query, "kind", "models")
	if kind == "skills" {
		rows, problems, err := catalog.Skills(cwd, refresh)
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
		needle := ""
		if raw := strings.TrimSpace(queryValue(query, "q", "")); raw != "" {
			needle = strings.ToLower(raw)
		}
		filtered := []*jsoncompat.Object{}
		for _, row := range rows {
			entry := jsoncompat.NewObject()
			entry.Set("id", row.ID)
			entry.Set("name", row.Name)
			entry.Set("displayName", row.DisplayName)
			entry.Set("description", row.Description)
			entry.Set("path", row.Path)
			entry.Set("scope", row.Scope)
			if needle == "" || strings.Contains(strings.ToLower(
				row.Name+" "+row.DisplayName+" "+row.Description), needle) {
				filtered = append(filtered, entry)
			}
		}
		page := filtered
		if offset > len(filtered) {
			page = nil
		} else if offset+limit < len(filtered) {
			page = filtered[offset : offset+limit]
		} else {
			page = filtered[offset:]
		}
		selected := &jsoncompat.Array{}
		wanted := map[string]bool{}
		for _, id := range ids {
			wanted[id] = true
		}
		for _, row := range rows {
			if wanted[row.ID] {
				entry := jsoncompat.NewObject()
				entry.Set("id", row.ID)
				entry.Set("name", row.Name)
				entry.Set("displayName", row.DisplayName)
				entry.Set("description", row.Description)
				entry.Set("path", row.Path)
				entry.Set("scope", row.Scope)
				selected.Items = append(selected.Items, entry)
			}
		}
		errors := &jsoncompat.Array{}
		for _, problem := range problems {
			errors.Items = append(errors.Items, problem)
		}
		result := jsoncompat.NewObject()
		result.Set("kind", "skills")
		result.Set("models", &jsoncompat.Array{})
		skills := &jsoncompat.Array{}
		skills.Items = append(skills.Items, anySlice(page)...)
		result.Set("skills", skills)
		result.Set("selectedSkills", selected)
		result.Set("errors", errors)
		result.Set("total", jsoncompat.Num(len(filtered)))
		result.Set("offset", jsoncompat.Num(offset))
		result.Set("limit", jsoncompat.Num(limit))
		cache := jsoncompat.NewObject()
		cache.Set("state", "fresh")
		cache.Set("updatedAt", jsoncompat.Num(int(time.Now().Unix())))
		cache.Set("bucketId", "go")
		cache.Set("backgroundRefresh", false)
		result.Set("cache", cache)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}
	provider := stringField(view, "provider")
	value, err := catalog.Models(cwd, provider, refresh)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	if model := stringField(view, "model"); model != "" {
		value.Set("currentModel", model)
	}
	if effort := stringField(view, "effort"); effort != "" {
		value.Set("currentEffort", effort)
	}
	if fastMode, ok := value.MustGet("fastMode").(*jsoncompat.Object); ok {
		allowed := false
		if flag, ok := fastMode.MustGet("allowed").(bool); ok {
			allowed = flag
		}
		fastMode.Set("allowed", allowed && provider == "openai")
	}
	if tier := stringField(view, "serviceTier"); tier != "" {
		value.Set("currentServiceTier", tier)
	}
	h.output(http.StatusOK, value, contentTypeJSON, "")
}

// codexCatalogCwd resolves the directory a catalog read applies to: the live
// thread's cwd, else the stored row's, else the codex home.
func (h *handler) codexCatalogCwd(threadID string) string {
	if view, _, ok := h.liveView(threadID); ok {
		if cwd := stringField(view, "cwd"); cwd != "" {
			return cwd
		}
	}
	if h.server.Store != nil {
		if row, err := h.server.Store.Get(threadID); err == nil && row != nil {
			if cwd := stringField(row, "cwd"); cwd != "" {
				return cwd
			}
		}
	}
	return h.server.Codex.Home
}

func anySlice(items []*jsoncompat.Object) []any {
	values := make([]any, 0, len(items))
	for _, item := range items {
		values = append(values, item)
	}
	return values
}

// pageCatalogSkills slices an ACP skills catalog to the requested page and
// stamps the paging fields the picker reads.
func pageCatalogSkills(result *jsoncompat.Object, offset, limit int) {
	rows, _ := result.MustGet("skills").(*jsoncompat.Array)
	if rows == nil {
		return
	}
	if limit < 1 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	items := rows.Items
	if offset > len(items) {
		items = nil
	} else if offset+limit < len(items) {
		items = items[offset : offset+limit]
	} else {
		items = items[offset:]
	}
	result.Set("skills", &jsoncompat.Array{Items: items})
	result.Set("offset", jsoncompat.Num(offset))
	result.Set("limit", jsoncompat.Num(limit))
	cache := jsoncompat.NewObject()
	cache.Set("state", "fresh")
	cache.Set("updatedAt", jsoncompat.Num(int(time.Now().Unix())))
	cache.Set("bucketId", "go")
	cache.Set("backgroundRefresh", false)
	result.Set("cache", cache)
}

func bodyList(body any, key string) []string {
	object, ok := body.(*jsoncompat.Object)
	if !ok {
		return nil
	}
	list, ok := object.MustGet(key).(*jsoncompat.Array)
	if !ok {
		return nil
	}
	values := []string{}
	for _, item := range list.Items {
		if text, ok := item.(string); ok && text != "" {
			values = append(values, text)
		}
	}
	return values
}

func queryIDs(query map[string][]string) []string {
	values := []string{}
	for _, value := range query["id"] {
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

// validRenameTitle mirrors service.Bridge.rename: 1–120 chars, no line
// separators or control characters. Returns the error message or "".
func validRenameTitle(title string) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) == 0 || utf8.RuneCountInString(title) > 120 {
		return "聊天名称需为 1–120 个字符，不能包含换行或控制字符"
	}
	for _, char := range title {
		if unicode.IsControl(char) || char == '\u2028' || char == '\u2029' {
			return "聊天名称需为 1–120 个字符，不能包含换行或控制字符"
		}
	}
	return ""
}

// validateMessageEdit runs the projection-side checks from Python's
// message_action: the cursor row must exist, match the version the client
// saw, be an editable user turn whose turn finished, and be the latest user
// turn (in-place edits only). Returns the row or the exact error message.
func validateMessageEdit(projection *timeline.Timeline, version, key string) (*jsoncompat.Object, string) {
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
	if editable, ok := row.MustGet("editable").(bool); !ok || !editable {
		return nil, "此消息不支持该操作"
	}
	if turnStatus, _ := row.MustGet("turnStatus").(string); turnStatus == "inProgress" {
		return nil, "请先停止当前任务，再编辑或分支"
	}
	if row.MustGet("turnId") != projection.MetaSnapshot().MustGet("latestUserTurnId") {
		return nil, "只有最近一条消息可原地编辑，请使用编辑并新建分支"
	}
	return row, ""
}

type messageActionEntry struct {
	action  string
	key     string
	version any
	source  string
	text    string
	result  *jsoncompat.Object
}

var (
	messageActionMu    sync.Mutex
	messageActionKnown = map[string]*messageActionEntry{}
)

// handleCodexMessageAction serves POST message-action. The in-place edit
// (latest user turn only) replays through the follower IPC; fork and
// edit-fork copy the thread through the app-server's thread/fork.
func (h *handler) handleCodexMessageAction(threadID, acpKind string, body *jsoncompat.Object) {
	action := stringField(body, "action")
	identifier := stringField(body, "id")
	if action != "edit" && action != "fork" && action != "edit-fork" {
		h.fail(http.StatusBadRequest, errorBody("消息操作无效"))
		return
	}
	if _, parseErr := uuid.Parse(identifier); parseErr != nil {
		h.fail(http.StatusBadRequest, errorBody("消息操作无效"))
		return
	}
	if acpKind != "" {
		h.fail(http.StatusConflict, errorBody("当前 Agent 不支持消息编辑或分支"))
		return
	}
	if action != "edit" {
		h.handleCodexForkAction(threadID, action, body)
		return
	}
	text := stringField(body, "text")
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 100000 {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–100000 字的消息"))
		return
	}
	key := stringField(body, "key")
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
	// The projection is shared with every other live-timeline reader and the
	// SSE incremental loop; its internals mutate under historyMu. The
	// validation must hold the same lock or a concurrent projection rebuild
	// races the cursor/version reads (a process-fatal map read/write).
	h.server.historyMu.Lock()
	row, rowErr := validateMessageEdit(h.server.historyTL[threadID],
		stringField(body, "version"), key)
	h.server.historyMu.Unlock()
	if rowErr != "" {
		h.fail(http.StatusBadRequest, errorBody(rowErr))
		return
	}
	if _, callErr := h.server.Codex.EditLastUserTurn(threadID, stringField(row, "turnId"),
		text, stringField(view, "serviceTier")); callErr != nil {
		h.fail(http.StatusConflict, errorBody(callErr.Error()))
		return
	}
	title := stringField(view, "title")
	if title == "" {
		title = "未命名聊天"
	}
	source := jsoncompat.NewObject()
	source.Set("id", threadID)
	source.Set("title", title)
	source.Set("turnId", row.MustGet("turnId"))
	result := jsoncompat.NewObject()
	result.Set("status", "accepted")
	result.Set("id", threadID)
	result.Set("host", "local")
	result.Set("action", action)
	result.Set("draft", nil)
	result.Set("source", source)
	messageActionMu.Lock()
	messageActionKnown[identifier] = &messageActionEntry{action: action, key: key,
		version: body.MustGet("version"), source: threadID, text: text, result: result}
	messageActionMu.Unlock()
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// objectOf narrows a JSON value to an object (nil when it is not one).
func objectOf(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

// validateMessageFork runs the fork-side row checks from Python's
// message_action: the cursor row must exist, match the client's version, be
// a forkable user turn and have finished (thread/fork refuses in-progress
// turn boundaries).
func validateMessageFork(projection *timeline.Timeline, version, key string) (*jsoncompat.Object, string) {
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
	if forkable, ok := row.MustGet("forkable").(bool); !ok || !forkable {
		return nil, "此消息不支持该操作"
	}
	if turnStatus, _ := row.MustGet("turnStatus").(string); turnStatus == "inProgress" {
		return nil, "请先停止当前任务，再编辑或分支"
	}
	return row, ""
}

// codexForkSettings mirrors the Codex adapter's fork_settings: the fork
// carries the source thread's provider, model, reasoning effort, service
// tier and permission profile so the branch behaves like the original.
func codexForkSettings(state *jsoncompat.Object) (map[string]any, error) {
	provider, _ := state.MustGet("modelProvider").(string)
	modelName, _ := state.MustGet("latestModel").(string)
	if provider == "" || modelName == "" {
		return nil, errors.New("尚未取得原会话的模型配置，请重新连接")
	}
	settings := map[string]any{"modelProvider": provider, "model": modelName}
	effort, _ := state.MustGet("latestReasoningEffort").(string)
	latest := objectOf(state.MustGet("latestThreadSettings"))
	if effort == "" && latest != nil {
		effort, _ = latest.MustGet("effort").(string)
	}
	if effort != "" {
		settings["config"] = map[string]any{"model_reasoning_effort": effort}
	}
	if latest != nil {
		if tier, _ := latest.MustGet("serviceTier").(string); tier != "" {
			settings["serviceTier"] = tier
		}
	}
	permissions := objectOf(state.MustGet("currentPermissions"))
	if permissions != nil {
		if profile := objectOf(permissions.MustGet("activePermissionProfile")); profile != nil {
			if id, _ := profile.MustGet("id").(string); id != "" {
				settings["permissions"] = id
			}
		}
		for _, key := range []string{"approvalPolicy", "approvalsReviewer", "runtimeWorkspaceRoots"} {
			if value, ok := permissions.Get(key); ok && value != nil {
				settings[key] = value
			}
		}
	}
	return settings, nil
}

// handleCodexForkAction serves the fork / edit-fork message actions: the
// app-server copies the thread up to the chosen turn into a new persisted
// chat; edit-fork then replaces the copy's last user turn through the
// follower IPC once a desktop owner is attached. An edit that cannot reach a
// owner yet returns the draft (the phone retries it after opening the chat).
func (h *handler) handleCodexForkAction(threadID, action string, body *jsoncompat.Object) {
	if h.server.Codex == nil {
		h.fail(http.StatusConflict, errorBody("当前网关没有 Codex 会话来源"))
		return
	}
	identifier := stringField(body, "id")
	key := stringField(body, "key")
	text := stringField(body, "text")
	if action != "fork" && (strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 100000) {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–100000 字的消息"))
		return
	}
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
	_, _, _ = h.server.refreshLiveProjection(threadID)
	h.server.historyMu.Lock()
	projection := h.server.historyTL[threadID]
	h.server.historyMu.Unlock()
	row, rowErr := validateMessageFork(projection, stringField(body, "version"), key)
	if rowErr != "" {
		h.fail(http.StatusBadRequest, errorBody(rowErr))
		return
	}
	turnID, _ := row.MustGet("turnId").(string)
	rawState, _, _, _ := h.server.Codex.Snapshot(threadID)
	if rawState == nil {
		rawState = jsoncompat.NewObject()
	}
	settings, settingsErr := codexForkSettings(rawState)
	if settingsErr != nil {
		h.fail(http.StatusConflict, errorBody(settingsErr.Error()))
		return
	}
	cwd, _ := view.MustGet("cwd").(string)
	sourceTitle := stringField(view, "title")
	if strings.TrimSpace(sourceTitle) == "" {
		sourceTitle = "未命名聊天"
	}
	childTitle := truncateSubText(sourceTitle, 90) + " · 分支"
	child, forkErr := h.server.Codex.ForkThread(threadID, turnID, cwd, childTitle, settings)
	if forkErr != nil {
		h.fail(http.StatusConflict, errorBody(forkErr.Error()))
		return
	}
	if h.server.Forks != nil {
		h.server.Forks.Record(child, forkOrigin{ID: threadID, Host: "local", Title: sourceTitle})
	}
	source := jsoncompat.NewObject()
	source.Set("id", threadID)
	source.Set("title", sourceTitle)
	source.Set("turnId", turnID)
	result := jsoncompat.NewObject()
	result.Set("id", child)
	result.Set("host", "local")
	result.Set("action", action)
	result.Set("source", source)
	if action == "fork" {
		result.Set("status", "created")
		result.Set("draft", nil)
	} else if _, editErr := h.editForkChildTurn(child, turnID, text); editErr == nil {
		result.Set("status", "accepted")
		result.Set("draft", nil)
	} else {
		// The branch exists but no desktop owner accepted the replacement
		// yet; the phone keeps the text as an edit draft on the child.
		result.Set("status", "unknown")
		result.Set("draft", text)
	}
	messageActionMu.Lock()
	messageActionKnown[identifier] = &messageActionEntry{action: action, key: key,
		version: body.MustGet("version"), source: threadID, text: text, result: result}
	messageActionMu.Unlock()
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// editForkChildTurn pushes the replacement text into the branched thread.
// The fork was minted by a short-lived app-server, so the desktop App only
// owns it after the user opens the chat — until then this reports failure
// and the draft flows back to the phone.
func (h *handler) editForkChildTurn(childID, turnID, text string) (bool, error) {
	session, err := h.server.Codex.Watch(childID, 1500*time.Millisecond)
	if err != nil || session == nil || h.server.Codex.Owner(session) == "" {
		return false, errors.New("请先在桌面 App 打开此聊天")
	}
	if _, callErr := h.server.Codex.EditLastUserTurn(childID, turnID, text, ""); callErr != nil {
		return false, callErr
	}
	return true, nil
}
