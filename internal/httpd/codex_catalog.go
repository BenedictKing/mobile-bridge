package httpd

import (
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
	if row.MustGet("turnId") != projection.Meta().MustGet("latestUserTurnId") {
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
// edit-fork need the app-server fork runtime and stay unsupported here.
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
		h.fail(http.StatusConflict, errorBody("当前 Go 网关暂不支持分支操作，请在电脑 App 中进行"))
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
	h.server.historyMu.Lock()
	projection := h.server.historyTL[threadID]
	h.server.historyMu.Unlock()
	row, rowErr := validateMessageEdit(projection, stringField(body, "version"), key)
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
