package httpd

import (
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
	"github.com/google/uuid"
)

// Cross-agent session migration (Python service.Bridge.migrate): copy this
// chat's context into a new session on another (or the same) agent. The
// source chat stays read-only; the transcript is rebuilt as a text message
// and injected as the new session's first turn.

var migrationScopes = map[string]bool{"full": true, "main": true, "latest": true}

func (s *Server) migrationMeta(threadID string) any {
	if s.Creations == nil {
		return nil
	}
	entry := s.Creations.migrationForTarget(threadID)
	if entry == nil {
		return nil
	}
	meta := jsoncompat.NewObject()
	meta.Set("state", entry.State)
	meta.Set("warning", entry.Warning)
	return meta
}

// Restore execution routing without starting threads during gateway startup.
func (s *Server) RegisterCodexCLIThreads() {
	if s.Creations == nil || s.Codex == nil {
		return
	}
	for _, entry := range s.Creations.cliTargets() {
		s.Codex.RegisterCLI(entry.ID, entry.ProjectCwd)
	}
}

func (s *Server) useCodexCLI(requestID, targetID, cwd string) error {
	if s.Creations == nil {
		return errors.New("Codex CLI 迁移需要持久化创建账本")
	}
	if err := s.Creations.setExecution(requestID, "cli"); err != nil {
		return err
	}
	s.Codex.RegisterCLI(targetID, cwd)
	_, err := s.Codex.Watch(targetID, 1500*time.Millisecond)
	return err
}

// max messages / total chars / chars per entry, per scope. The totals stay
// under send's 100k cap including header and footer (Python parity).
var migrationBudgets = map[string][3]int{
	"main":   {50, 60000, 6000},
	"full":   {500, 90000, 12000},
	"latest": {12, 40000, 20000},
}

var migrationNoiseTitles = map[string]bool{
	"read": true, "读取": true, "bash": true, "shell": true,
	"todowrite": true, "todo write": true, "任务清单": true,
}

var migrationReadOnlyCommand = regexp.MustCompile(
	`(?i)^(?:rg|grep|find|fd|sed|awk|cat|head|tail|less|ls|pwd|which|command\s+-v|` +
		`git\s+(?:status|diff|log|show)|python(?:3)?\s+-m\s+(?:pytest|unittest)\s+--collect-only)(?:\s|$)`)

var migrationEncodedImagePrefixes = []string{"iVBORw0KGgo", "/9j/", "R0lGOD", "UklGR"}

const migrationImageMarker = "[image attachment omitted / 图片附件未迁移]"

// isMigrationNoise drops routine inspection/tool bookkeeping from the default
// transcript (scope "main" only).
func isMigrationNoise(message *jsoncompat.Object) bool {
	switch strings.ToLower(strings.TrimSpace(stringField(message, "kind"))) {
	case "toolcall", "tool_call", "mcptoolcall", "dynamictoolcall", "function_call", "custom_tool_call":
		return true
	}
	title := ""
	for _, key := range []string{"title", "kind"} {
		if text, ok := message.MustGet(key).(string); ok && text != "" {
			title = strings.TrimSpace(text)
			break
		}
	}
	text, _ := message.MustGet("text").(string)
	// Native Claude tools use descriptive titles and kind=subagent. Only drop
	// a simple inspection command; compound commands may also build or modify.
	if stringField(message, "kind") == "subagent" &&
		!strings.ContainsAny(text, ";|\n`><") && !strings.Contains(text, "&&") && !strings.Contains(text, "$(") &&
		migrationReadOnlyCommand.MatchString(strings.TrimSpace(text)) {
		return true
	}
	folded := strings.ToLower(strings.TrimSpace(text))
	switch strings.ToLower(title) {
	case "toolcall", "工具调用":
		prefix := folded == "" || strings.HasPrefix(folded, "read:") ||
			strings.HasPrefix(folded, "todowrite:") || strings.HasPrefix(folded, "bash:")
		return prefix && folded != ""
	case "执行命令", "command execution":
		return migrationReadOnlyCommand.MatchString(strings.TrimSpace(text))
	}
	return migrationNoiseTitles[strings.ToLower(title)]
}

func isMigrationThought(message *jsoncompat.Object) bool {
	title := strings.TrimSpace(stringField(message, "title"))
	kind := strings.TrimSpace(stringField(message, "kind"))
	return title == "思考摘要" || strings.EqualFold(kind, "agentThought") ||
		strings.EqualFold(kind, "reasoning")
}

func migrationText(text string) string {
	compact := strings.TrimSpace(text)
	if len(compact) >= 4096 {
		for _, prefix := range migrationEncodedImagePrefixes {
			if strings.HasPrefix(compact, prefix) {
				return migrationImageMarker
			}
		}
	}
	return text
}

type migrationTexts struct {
	header, title, directory, truncated, sep, cont string
	roles                                          map[string]string
	latestHeader, latestContinue, instructions     string
}

var migrationTextsByLocale = map[string]migrationTexts{
	"zh-CN": {
		header: "【从 %s 迁移的会话】", title: "标题：", directory: "目录：",
		truncated: "（转录过长，内容已截断）",
		roles:     map[string]string{"user": "用户", "assistant": "助手", "activity": "操作", "error": "错误"},
		sep:       "：", cont: "以上是从 %s 迁移过来的会话转录，请在此基础上继续。",
		latestHeader:   "【来自 %s 的最新问答】",
		latestContinue: "以上是 %s 中最近一次提问与回答，请在此基础上继续。",
		instructions:   "【用户后续指示】",
	},
	"en-US": {
		header: "[Session migrated from %s]", title: "Title: ", directory: "Directory: ",
		truncated: "(Transcript truncated to fit the selected scope.)",
		roles:     map[string]string{"user": "User", "assistant": "Assistant", "activity": "Activity", "error": "Error"},
		sep:       ": ", cont: "The transcript above was migrated from %s. Please continue from this context.",
		latestHeader:   "[Latest exchange from %s]",
		latestContinue: "The exchange above is the latest question and answer from %s. Please continue from this context.",
		instructions:   "[User instructions for what to do next]",
	},
}

func normalizeUILocale(value string) string {
	if _, ok := migrationTextsByLocale[value]; ok {
		return value
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "en") {
		return "en-US"
	}
	return "zh-CN"
}

func migrationScope(scope string) (string, bool) {
	if scope == "" {
		return "main", true
	}
	if !migrationScopes[scope] {
		return "", false
	}
	return scope, true
}

type migrationEntry struct {
	role string
	text string
}

// migrationTranscript flattens a normalized view into (role, text) entries,
// biased to the tail: user/assistant turns keep full text, activities
// collapse to a one-line summary, outputs drop. "main" also drops routine
// tool calls so they do not displace conversation text from the budget.
// "latest" keeps only the final user question plus what followed.
func migrationTranscript(view *jsoncompat.Object, scope string) ([]migrationEntry, bool) {
	return migrationTranscriptBounded(view, scope, 100000)
}

func migrationTranscriptBounded(view *jsoncompat.Object, scope string, maxBytes int) ([]migrationEntry, bool) {
	entries := []migrationEntry{}
	contentTruncated := false
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns != nil {
		for _, item := range turns.Items {
			turn, ok := item.(*jsoncompat.Object)
			if !ok {
				continue
			}
			messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
			if messages == nil {
				continue
			}
			for _, raw := range messages.Items {
				message, ok := raw.(*jsoncompat.Object)
				if !ok {
					continue
				}
				role, _ := message.MustGet("role").(string)
				text := strings.TrimSpace(migrationText(stringField(message, "text")))
				if role == "user" && (strings.HasPrefix(text, "<command-name>") || strings.HasPrefix(text, "<local-command-stdout>")) {
					// Claude persists local CLI commands as user records even though
					// they are not prompts sent to the model.
					continue
				}
				if text != strings.TrimSpace(stringField(message, "text")) {
					// An attachment belongs to its preceding question. It must not
					// become the last user question in the latest-exchange scope.
					entries = append(entries, migrationEntry{"activity", text})
					continue
				}
				switch {
				case role == "user" || role == "assistant":
					if text != "" {
						history, truncated := extractMigrationHistory(migrationEntry{role, text})
						contentTruncated = contentTruncated || truncated
						for _, entry := range history {
							if scope != "latest" || entry.role == "user" || entry.role == "assistant" || entry.text == migrationImageMarker {
								entries = append(entries, entry)
							}
						}
					}
				case scope != "latest" && (role == "activity" || role == "error"):
					if role == "activity" && scope == "main" && isMigrationNoise(message) {
						continue
					}
					title := ""
					if isMigrationThought(message) {
						title = "思考摘要"
					} else {
						for _, key := range []string{"title", "kind"} {
							if candidate, ok := message.MustGet(key).(string); ok && strings.TrimSpace(candidate) != "" {
								title = strings.TrimSpace(candidate)
								break
							}
						}
					}
					first := text
					if !isMigrationThought(message) {
						if index := strings.IndexByte(text, '\n'); index >= 0 {
							first = text[:index]
						}
						if runes := []rune(first); len(runes) > 120 {
							first = string(runes[:120])
						}
					}
					summary := first
					if title != "" {
						summary = title
						if first != "" {
							summary = title + "：" + first
						}
					}
					if summary != "" {
						entries = append(entries, migrationEntry{role, summary})
					}
				}
			}
		}
	}
	if scope == "latest" {
		lastUser := -1
		for index, entry := range entries {
			if entry.role == "user" {
				lastUser = index
			}
		}
		if lastUser < 0 {
			entries = entries[len(entries):]
		} else {
			entries = entries[lastUser:]
		}
	}
	maxMessages, maxChars, entryChars := migrationBudgets[scope][0], migrationBudgets[scope][1], migrationBudgets[scope][2]
	capped := make([]migrationEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.role != "user" && entry.role != "assistant" && utf8.RuneCountInString(entry.text) > entryChars {
			entry.text = string([]rune(entry.text)[:entryChars]) + "…"
		}
		capped = append(capped, entry)
	}
	selected := make([]bool, len(capped))
	kept := []migrationEntry{}
	total, bytesUsed := 0, 0
	add := func(index, reserve int) bool {
		if index < 0 || index >= len(capped) || selected[index] || len(kept) >= maxMessages {
			return false
		}
		entry := capped[index]
		// Include role labels, separators and the portable history frame.
		charsLeft, bytesLeft := maxChars-total-8, maxBytes-bytesUsed-32
		if utf8.RuneCountInString(entry.text) > charsLeft || len(entry.text) > bytesLeft {
			if reserve == 0 || charsLeft < 2 || bytesLeft < 4 {
				return false
			}
			entry.text = limitMigrationText(entry.text, charsLeft/reserve, bytesLeft/reserve)
			if entry.text == "" {
				return false
			}
			capped[index] = entry
			contentTruncated = true
		}
		selected[index] = true
		kept = append(kept, entry)
		total += utf8.RuneCountInString(entry.text) + 8
		bytesUsed += len(entry.text) + 32
		return true
	}
	firstTask, lastUser, lastAssistant := -1, -1, -1
	for index, entry := range capped {
		if entry.role == "user" {
			lastUser = index
			if firstTask < 0 && !strings.HasPrefix(strings.TrimSpace(entry.text), "/") {
				firstTask = index
			}
		}
		if entry.role == "assistant" {
			lastAssistant = index
		}
	}
	if scope != "latest" {
		// Keep the first actual task and the latest reasoning summaries alongside
		// the recency-biased tail, so long tool-heavy sessions retain their intent.
		if firstTask < 0 {
			for index, entry := range capped {
				if entry.role == "user" {
					firstTask = index
					break
				}
			}
		}
		add(firstTask, 3)
	}
	add(lastAssistant, 2)
	add(lastUser, 1)
	if scope != "latest" {
		// User corrections and the final answer take priority over reasoning;
		// otherwise a long chain of thoughts can consume every message slot.
		for index := len(capped) - 1; index >= 0; index-- {
			if capped[index].role == "user" || capped[index].role == "assistant" || capped[index].role == "error" || capped[index].text == migrationImageMarker {
				add(index, 0)
			}
		}
		for index := len(capped) - 1; index >= 0; index-- {
			if capped[index].role == "activity" && strings.HasPrefix(capped[index].text, "思考摘要：") {
				add(index, 0)
			}
		}
	}
	for index := len(capped) - 1; index >= 0; index-- {
		add(index, 0)
	}
	ordered := make([]migrationEntry, 0, len(kept))
	for index, entry := range capped {
		if selected[index] {
			ordered = append(ordered, entry)
		}
	}
	return ordered, contentTruncated || len(kept) < len(capped)
}

// limitMigrationText preserves both the opening context and the closing result
// while respecting Unicode character and wire-byte limits.
func limitMigrationText(text string, maxChars, maxBytes int) string {
	if maxChars < 1 || maxBytes < len("…") {
		return ""
	}
	runes := []rune(text)
	render := func(count int) string {
		return string(runes[:(count+1)/2]) + "…" + string(runes[len(runes)-count/2:])
	}
	low, high := 0, min(len(runes)-1, maxChars-1)
	for low < high {
		middle := (low + high + 1) / 2
		if len(render(middle)) <= maxBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return render(low)
}

func migrationMessage(sourceLabel string, view *jsoncompat.Object, uiLocale, scope, instructions string) string {
	texts := migrationTextsByLocale[normalizeUILocale(uiLocale)]
	if scope == "latest" {
		texts.header, texts.cont = texts.latestHeader, texts.latestContinue
	}
	title := textutil.CleanTitle(stringField(view, "title"))
	cwd, _ := view.MustGet("cwd").(string)
	lines := []string{
		sprintf(texts.header, sourceLabel),
		texts.title + title,
		texts.directory + cwd,
	}
	footer := []string{"", sprintf(texts.cont, sourceLabel)}
	if instructions = strings.TrimSpace(instructions); instructions != "" {
		footer = append(footer, "", texts.instructions, instructions)
	}
	overhead := len(strings.Join(lines, "\n")) + len(strings.Join(footer, "\n")) + len(texts.truncated) + len(migrationHistoryFrame(nil, instructions)) + 6
	entries, truncated := migrationTranscriptBounded(view, scope, 100000-overhead)
	if truncated {
		lines = append(lines, texts.truncated)
	}
	lines = append(lines, "", migrationHistoryFrame(entries, instructions))
	lines = append(lines, "")
	for _, entry := range entries {
		role := texts.roles[entry.role]
		if role == "" {
			role = entry.role
		}
		lines = append(lines, role+texts.sep+entry.text)
	}
	lines = append(lines, footer...)
	return strings.Join(lines, "\n")
}

func sprintf(format, value string) string {
	return strings.Replace(format, "%s", value, 1)
}

func stringField(object *jsoncompat.Object, key string) string {
	text, _ := object.MustGet(key).(string)
	return text
}

// targetProjects lists one target agent's projects from /api/projects rows
// (Python adapters expose projects() per agent; Go builds them per kind).
func (h *handler) targetProjects(kind string) []*jsoncompat.Object {
	rows := []*jsoncompat.Object{}
	projects, _ := h.projectList(0).MustGet("projects").(*jsoncompat.Array)
	if projects == nil {
		return rows
	}
	for _, item := range projects.Items {
		project, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if agent, _ := project.MustGet("agent").(string); agent == kind {
			rows = append(rows, project)
		}
	}
	return rows
}

func migrationProjectMatches(entry *creationEntry, targetKind, projectKey, cwd string) bool {
	if entry.ProjectCwd != "" {
		return filepath.Clean(entry.ProjectCwd) == filepath.Clean(cwd)
	}
	if entry.Project == projectKey {
		return true
	}
	legacyHash := hashProject(cwd)
	return entry.Project == targetKind+":"+legacyHash || entry.Project == "acp:"+legacyHash
}

func migrationProjectWasCreated(entry *creationEntry, targetKind, cwd string) bool {
	if entry.ProjectCwd != "" {
		return entry.ProjectCreated
	}
	return entry.Project == targetKind+":"+hashProject(cwd)
}

// migrationSourceView resolves the source chat's normalized view: ACP live
// snapshot first, then the codex live follower, then saved history. Scope
// "full" on a codex thread reloads the complete rollout, not the loaded tail.
func (h *handler) migrationSourceView(threadID, scope string) *jsoncompat.Object {
	resolution := h.server.resolveACP(threadID)
	if resolution.manager != nil {
		if resolution.session == nil {
			return nil
		}
		return h.server.acpViewState(resolution.kind, resolution.manager, resolution.session)
	}
	if scope == "full" && h.server.Store != nil {
		if history, err := h.server.Store.HistoryFor(threadID, nil); err == nil {
			state := jsoncompat.NewObject()
			for _, key := range []string{"id", "title", "cwd", "latestModel", "modelProvider", "turns"} {
				if value, ok := history.Get(key); ok {
					state.Set(key, value)
				}
			}
			if value, ok := history.Get("turnsPagination"); ok {
				state.Set("turnsPagination", value)
			}
			view := model.NormalizeState(state, false)
			view.Set("host", "local")
			view.Set("hostLabel", "此电脑")
			view.Set("agent", "codex")
			h.server.codexSubagentView(threadID, view)
			return view
		}
	}
	view, _ := h.server.View(threadID).(*jsoncompat.Object)
	if view == nil {
		view = jsoncompat.NewObject()
	}
	return view
}

// handleMigrate serves POST /api/sessions/<id>/migrate: create the target
// chat through the shared idempotent creations ledger, then inject the
// transcript as its first message. A retried request never duplicates either
// step (Python parity: same ledger id, uuid5-derived submission id).
func (h *handler) handleMigrate(threadID string) {
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	if stringField(body, "teamMaintenance") == "compact" {
		teamID := stringField(body, "teamId")
		if teamID == "" || h.server.TeamStore == nil {
			h.fail(http.StatusConflict, errorBody("缺少主控所属团队，请刷新后重试"))
			return
		}
		teamRow, resolveErr := h.teamConversation(threadID, teamID)
		if resolveErr != nil {
			h.fail(http.StatusConflict, errorBody(resolveErr.Error()))
			return
		}
		if teamRow == nil {
			h.fail(http.StatusConflict, errorBody("找不到主控所属团队，请刷新后重试"))
			return
		}
		if guardErr := h.leadContextGuard(teamRow, threadID); guardErr != nil {
			h.fail(http.StatusConflict, errorBody(guardErr.Error()))
			return
		}
	}
	requestID := stringField(body, "id")
	if _, parseErr := uuid.Parse(requestID); parseErr != nil {
		h.fail(http.StatusBadRequest, errorBody("迁移请求标识无效"))
		return
	}
	scope, valid := migrationScope(stringField(body, "scope"))
	if !valid {
		h.fail(http.StatusBadRequest, errorBody("未知的迁移范围"))
		return
	}
	uiLocale := stringField(body, "uiLocale")
	instructions := strings.TrimSpace(stringField(body, "instructions"))
	if utf8.RuneCountInString(instructions) > 8000 {
		h.fail(http.StatusBadRequest, errorBody("后续指示最多 8000 字"))
		return
	}
	phase := stringField(body, "phase")
	if phase != "" && phase != "prepare" && phase != "abandon" {
		h.fail(http.StatusBadRequest, errorBody("未知的迁移阶段"))
		return
	}
	modelName := strings.TrimSpace(stringField(body, "model"))
	effort := strings.TrimSpace(stringField(body, "effort"))
	if modelName == "" && effort != "" {
		h.fail(http.StatusBadRequest, errorBody("选择思考强度前请先选择模型"))
		return
	}

	targetKind := stringField(body, "agent")
	var target *acpruntime.Manager
	if targetKind == "codex" {
		if h.server.Codex == nil {
			h.fail(http.StatusBadRequest, errorBody("未知的 Agent 类型"))
			return
		}
	} else if manager := h.server.acpAgentSet()[targetKind]; manager != nil {
		target = manager
	} else {
		h.fail(http.StatusBadRequest, errorBody("未知的 Agent 类型"))
		return
	}

	if phase == "abandon" {
		// The phone backed out of the settings step: drop the empty target
		// session that prepare created. A Codex prepare creates nothing.
		status := "none"
		if h.server.Creations != nil {
			id, ok, err := h.server.Creations.forgetPrepared(requestID, targetKind)
			if err != nil {
				h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
				return
			}
			if ok {
				status = "abandoned"
				if id != "" && target != nil {
					target.Drop(id)
				}
			}
		}
		result := jsoncompat.NewObject()
		result.Set("status", status)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}

	view := h.migrationSourceView(threadID, scope)
	if view == nil {
		h.fail(http.StatusConflict, errorBody("找不到这个会话，请稍后刷新列表"))
		return
	}
	sourceLabel := "Codex"
	if resolution := h.server.resolveACP(threadID); resolution.manager != nil {
		sourceLabel = resolution.manager.Label
		if sourceLabel == "" {
			sourceLabel = resolution.kind
		}
	}

	cwd, _ := view.MustGet("cwd").(string)
	title := textutil.CleanTitle(strings.TrimSpace(stringField(view, "title")))
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:120])
	}
	if title == "" {
		title = "未命名聊天"
	}

	// Project resolution mirrors Python: an explicit key must exist on the
	// target; otherwise match the source cwd, and when the target has never
	// listed it, create directly in that directory (local sources only).
	projectKey := stringField(body, "project")
	projectCreated := false
	if projectKey != "" {
		found := false
		for _, project := range h.targetProjects(targetKind) {
			if key, _ := project.MustGet("key").(string); key == projectKey {
				if dir, _ := project.MustGet("cwd").(string); dir != "" {
					cwd = dir
				}
				found = true
				break
			}
		}
		if !found {
			h.fail(http.StatusBadRequest, errorBody("请选择目标 Agent 中已保存的项目"))
			return
		}
	} else if cwd != "" {
		for _, project := range h.targetProjects(targetKind) {
			if dir, _ := project.MustGet("cwd").(string); dir == cwd {
				projectKey, _ = project.MustGet("key").(string)
				break
			}
		}
		if projectKey == "" {
			projectKey = targetKind + ":" + hashProject(cwd)
			projectCreated = true
		}
	} else {
		h.fail(http.StatusBadRequest, errorBody("源会话缺少目录信息，无法匹配目标项目"))
		return
	}
	ledger := h.server.Creations
	if ledger != nil {
		if existing := ledger.lookup(requestID); existing != nil {
			if existing.Agent != targetKind || existing.Title != title ||
				!migrationProjectMatches(existing, targetKind, projectKey, cwd) {
				h.fail(http.StatusBadRequest, errorBody("同一创建请求不能用于不同内容"))
				return
			}
			projectKey = existing.Project
			projectCreated = migrationProjectWasCreated(existing, targetKind, cwd)
		}
	}

	// Codex lists its models without a thread, so the settings step needs no
	// target session at all; the thread is created when the phone confirms.
	if phase == "prepare" && target == nil {
		result := jsoncompat.NewObject()
		result.Set("host", "local")
		result.Set("agent", targetKind)
		result.Set("project", projectKey)
		result.Set("status", "prepared")
		result.Set("sent", false)
		result.Set("projectCreated", projectCreated)
		result.Set("state", "prepared")
		result.Set("catalog", h.migrationCatalog(nil, nil, cwd))
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}

	createdID := ""
	alreadySent := false
	if ledger != nil {
		entry, claimed, claimErr := ledger.begin(requestID, &creationEntry{Agent: targetKind, Project: projectKey,
			ProjectCwd: cwd, ProjectCreated: projectCreated, Title: title, Host: "local", At: ledger.now(), State: "creating"})
		if claimErr != nil {
			h.fail(http.StatusServiceUnavailable, errorBody(claimErr.Error()))
			return
		}
		if !claimed {
			if entry.Agent != targetKind || entry.Project != projectKey || entry.Title != title ||
				!migrationProjectMatches(entry, targetKind, projectKey, cwd) {
				h.fail(http.StatusBadRequest, errorBody("同一创建请求不能用于不同内容"))
				return
			}
			if entry.ID == "" {
				h.fail(http.StatusConflict, errorBody("上次创建结果尚不确定，请先刷新聊天列表并检查电脑 App，避免重复创建"))
				return
			}
			createdID = entry.ID
			alreadySent = entry.Sent
			if entry.State == "dispatching" {
				h.fail(http.StatusConflict, errorBody("上次迁移发送结果尚不确定，请在目标会话核实，避免重复发送"))
				return
			}
		}
	}
	if target == nil && !alreadySent && modelName == "" {
		if ledger != nil {
			if entry := ledger.lookup(requestID); entry != nil && entry.Model != "" {
				modelName, effort = entry.Model, entry.Effort
			}
		}
		if modelName == "" {
			catalog, settingsErr := h.server.Codex.Catalog().Models(cwd, "", false)
			if settingsErr != nil {
				h.fail(http.StatusConflict, errorBody("无法读取 Codex 默认模型与思考强度: "+settingsErr.Error()))
				return
			}
			modelName = stringField(catalog, "currentModel")
			effort = stringField(catalog, "currentEffort")
			if modelName == "" {
				h.fail(http.StatusConflict, errorBody("无法读取 Codex 默认模型，请手动选择模型与思考强度"))
				return
			}
		}
	}

	// A migrated chat keeps a link back to its source (Python's fork origin
	// ledger; the phone renders it as 来源会话 in the details sheet).
	if h.server.Forks != nil && createdID != "" && threadID != createdID {
		h.server.Forks.Record(createdID, forkOrigin{ID: threadID, Host: "local", Title: title})
	}
	if createdID == "" {
		if target != nil {
			session, createErr := target.Create("sess_"+uuid.NewString(), cwd, title, 60*time.Second)
			if createErr != nil {
				h.fail(http.StatusConflict, errorBody(createErr.Error()))
				return
			}
			createdID = session.ID
		} else {
			id, createErr := h.server.Codex.CreateConversation(cwd, title)
			if createErr != nil {
				h.fail(http.StatusConflict, errorBody(createErr.Error()))
				return
			}
			createdID = id
		}
		if ledger != nil {
			if err := ledger.complete(requestID, createdID); err != nil {
				h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
				return
			}
		}
	}

	// ACP agents only advertise their models once a session exists, so
	// prepare creates the target (or reopens the one a previous prepare made)
	// and returns its real catalog. The transcript is not sent yet.
	if phase == "prepare" && !alreadySent {
		session, openErr := target.Open(createdID, cwd, target.InitializeTimeout)
		if openErr != nil {
			h.fail(http.StatusConflict, errorBody(openErr.Error()))
			return
		}
		if ledger != nil {
			if err := ledger.markPrepared(requestID); err != nil {
				h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
				return
			}
		}
		result := jsoncompat.NewObject()
		result.Set("id", createdID)
		result.Set("host", "local")
		result.Set("agent", targetKind)
		result.Set("project", projectKey)
		result.Set("status", "prepared")
		result.Set("sent", false)
		result.Set("projectCreated", projectCreated)
		result.Set("state", "prepared")
		result.Set("catalog", h.migrationCatalog(target, session, cwd))
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}

	message := migrationMessage(sourceLabel, view, uiLocale, scope, instructions)
	if ledger != nil {
		if err := ledger.setMessage(requestID, message); err != nil {
			h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
			return
		}
	}
	result := jsoncompat.NewObject()
	result.Set("id", createdID)
	result.Set("host", "local")
	result.Set("agent", targetKind)
	result.Set("project", projectKey)
	result.Set("status", "created")
	result.Set("sent", false)
	result.Set("projectCreated", projectCreated)
	result.Set("state", "created")

	if alreadySent {
		result.Set("status", "sent")
		result.Set("sent", true)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}
	if ledger != nil && modelName != "" {
		if err := ledger.setSettings(requestID, modelName, effort); err != nil {
			h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
			return
		}
	}
	// Codex creates the thread through a short-lived app-server, but the
	// desktop App must own it before follower IPC can inject the transcript.
	// Open it first and return promptly; the frontend retries after the target
	// chat has been opened instead of holding the tunnel request for 90s.
	if target == nil && !h.server.Codex.IsCLI(createdID) {
		// The desktop owner protocol is absent in newer Apps. Switch only this
		// never-dispatched migration to the explicitly authorized official CLI.
		if err := h.server.Codex.IPC.Connect(1500 * time.Millisecond); err != nil {
			if cliErr := h.server.useCodexCLI(requestID, createdID, cwd); cliErr != nil {
				if ledger != nil {
					if saveErr := ledger.markWaiting(requestID, cliErr.Error()); saveErr != nil {
						h.fail(http.StatusServiceUnavailable, errorBody(saveErr.Error()))
						return
					}
				}
				result.Set("warning", cliErr.Error())
				result.Set("state", "waiting_for_target")
				h.output(http.StatusOK, result, contentTypeJSON, "")
				return
			}
		}
	}
	if target == nil && !h.server.Codex.IsCLI(createdID) {
		entryState := ""
		if ledger != nil {
			if entry := ledger.lookup(requestID); entry != nil {
				entryState = entry.State
			}
		}
		if entryState != "opened" {
			if err := codex.OpenInDesktop(createdID); err != nil {
				if ledger != nil {
					if saveErr := ledger.markWaiting(requestID, err.Error()); saveErr != nil {
						h.fail(http.StatusServiceUnavailable, errorBody(saveErr.Error()))
						return
					}
				}
				result.Set("warning", err.Error())
				result.Set("state", "waiting_for_target")
				h.output(http.StatusOK, result, contentTypeJSON, "")
				return
			}
			if ledger != nil {
				if err := ledger.markOpened(requestID); err != nil {
					h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
					return
				}
			}
			result.Set("state", "opening")
			result.Set("warning", "请等待桌面 App 接管此聊天")
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
		if session, watchErr := h.server.Codex.Watch(createdID, 1500*time.Millisecond); watchErr != nil || session == nil || h.server.Codex.Owner(session) == "" {
			warning := "请先在桌面 App 打开此聊天"
			if watchErr != nil {
				warning = watchErr.Error()
			}
			if ledger != nil {
				if err := ledger.markWaiting(requestID, warning); err != nil {
					h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
					return
				}
			}
			result.Set("warning", warning)
			result.Set("state", "waiting_for_target")
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
	}
	submissionID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:migrate:"+requestID)).String()
	sendErr := error(nil)
	dispatched := false
	if target != nil {
		session, openErr := target.Open(createdID, cwd, target.InitializeTimeout)
		if openErr != nil {
			sendErr = openErr
		} else {
			// A chosen model/effort that the agent rejects must stop the
			// migration: sending the transcript on the default model would
			// silently ignore the user's choice. The entry stays unsent, so
			// the phone can retry with other settings.
			if modelName != "" {
				if _, settingsErr := target.UpdateSettings(session, modelName, effort, nil); settingsErr != nil {
					h.fail(http.StatusConflict, errorBody(settingsErr.Error()))
					return
				}
			}
			if ledger != nil {
				claimed, err := ledger.beginDispatch(requestID)
				if err != nil {
					h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
					return
				}
				if !claimed {
					h.fail(http.StatusConflict, errorBody("迁移发送已开始，请在目标会话核实，避免重复发送"))
					return
				}
			}
			dispatched = true
			sendErr = session.Prompt(message, 3600*time.Second)
		}
	} else {
		if modelName != "" {
			if _, settingsErr := h.server.Codex.UpdateSettings(createdID, modelName, effort, nil); settingsErr != nil {
				h.fail(http.StatusConflict, errorBody(settingsErr.Error()))
				return
			}
		}
		if ledger != nil {
			claimed, err := ledger.beginDispatch(requestID)
			if err != nil {
				h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
				return
			}
			if !claimed {
				h.fail(http.StatusConflict, errorBody("迁移发送已开始，请在目标会话核实，避免重复发送"))
				return
			}
		}
		dispatched = true
		_, sendErr = h.server.Codex.StartTurnWithOptions(createdID, submissionID, message, "send", "", nil, nil)
	}
	if sendErr != nil {
		if dispatched {
			h.fail(http.StatusConflict, errorBody("迁移发送结果尚不确定，请在目标会话核实，避免重复发送: "+sendErr.Error()))
			return
		}
		if ledger != nil {
			if err := ledger.markWaiting(requestID, sendErr.Error()); err != nil {
				h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
				return
			}
		}
		result.Set("warning", sendErr.Error())
		result.Set("state", "waiting_for_target")
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}
	if ledger != nil {
		if err := ledger.markSent(requestID); err != nil {
			h.fail(http.StatusServiceUnavailable, errorBody(err.Error()))
			return
		}
	}
	result.Set("status", "sent")
	result.Set("sent", true)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// migrationCatalog is the model catalog the phone offers in the settings
// step: the target session's advertised options for ACP agents, the
// thread-independent app-server list for Codex. A failed Codex listing
// degrades to an empty list plus catalogError, so the phone can still
// migrate on the target's defaults and say so.
func (h *handler) migrationCatalog(target *acpruntime.Manager, session *acpruntime.Session, cwd string) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	var catalog *jsoncompat.Object
	if target != nil {
		catalog = target.Catalog(session, "models", "", nil)
	} else {
		value, err := h.server.Codex.Catalog().Models(cwd, "", false)
		if err != nil {
			result.Set("models", &jsoncompat.Array{})
			result.Set("catalogError", err.Error())
			return result
		}
		catalog = value
	}
	for _, key := range []string{"models", "currentModel", "currentEffort", "modelError"} {
		if value, ok := catalog.Get(key); ok {
			result.Set(key, value)
		}
	}
	return result
}
