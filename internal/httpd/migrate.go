package httpd

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/google/uuid"
)

// Cross-agent session migration (Python service.Bridge.migrate): copy this
// chat's context into a new session on another (or the same) agent. The
// source chat stays read-only; the transcript is rebuilt as a text message
// and injected as the new session's first turn.

var migrationScopes = map[string]bool{"full": true, "main": true, "latest": true}

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

// isMigrationNoise drops routine inspection/tool bookkeeping from the default
// transcript (scope "main" only).
func isMigrationNoise(message *jsoncompat.Object) bool {
	title := ""
	for _, key := range []string{"title", "kind"} {
		if text, ok := message.MustGet(key).(string); ok && text != "" {
			title = strings.TrimSpace(text)
			break
		}
	}
	text, _ := message.MustGet("text").(string)
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

type migrationTexts struct {
	header, title, directory, truncated, sep, cont string
	roles                                          map[string]string
	latestHeader, latestContinue                   string
}

var migrationTextsByLocale = map[string]migrationTexts{
	"zh-CN": {
		header: "【从 %s 迁移的会话】", title: "标题：", directory: "目录：",
		truncated: "（转录过长，仅保留最近部分，已截断）",
		roles:     map[string]string{"user": "用户", "assistant": "助手", "activity": "操作", "error": "错误"},
		sep:       "：", cont: "以上是从 %s 迁移过来的会话转录，请在此基础上继续。",
		latestHeader:   "【来自 %s 的最新问答】",
		latestContinue: "以上是 %s 中最近一次提问与回答，请在此基础上继续。",
	},
	"en-US": {
		header: "[Session migrated from %s]", title: "Title: ", directory: "Directory: ",
		truncated: "(Transcript truncated to the most recent messages.)",
		roles:     map[string]string{"user": "User", "assistant": "Assistant", "activity": "Activity", "error": "Error"},
		sep:       ": ", cont: "The transcript above was migrated from %s. Please continue from this context.",
		latestHeader:   "[Latest exchange from %s]",
		latestContinue: "The exchange above is the latest question and answer from %s. Please continue from this context.",
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
// collapse to a one-line summary, outputs drop. "latest" keeps only the
// final user question plus what followed.
func migrationTranscript(view *jsoncompat.Object, scope string) ([]migrationEntry, bool) {
	entries := []migrationEntry{}
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
				text := strings.TrimSpace(stringField(message, "text"))
				switch {
				case role == "user" || role == "assistant":
					if text != "" {
						entries = append(entries, migrationEntry{role, text})
					}
				case scope != "latest" && (role == "activity" || role == "error"):
					if role == "activity" && scope == "main" && isMigrationNoise(message) {
						continue
					}
					title := ""
					for _, key := range []string{"title", "kind"} {
						if candidate, ok := message.MustGet(key).(string); ok && strings.TrimSpace(candidate) != "" {
							title = strings.TrimSpace(candidate)
							break
						}
					}
					first := text
					if index := strings.IndexByte(text, '\n'); index >= 0 {
						first = text[:index]
					}
					if runes := []rune(first); len(runes) > 120 {
						first = string(runes[:120])
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
		if utf8.RuneCountInString(entry.text) > entryChars {
			entry.text = string([]rune(entry.text)[:entryChars]) + "…"
		}
		capped = append(capped, entry)
	}
	tail := capped
	if len(tail) > maxMessages {
		tail = tail[len(tail)-maxMessages:]
	}
	dropped := len(capped) - len(tail)
	kept := []migrationEntry{}
	total := 0
	for index := len(tail) - 1; index >= 0; index-- {
		size := utf8.RuneCountInString(tail[index].text) + 8
		if len(kept) > 0 && total+size > maxChars {
			break
		}
		kept = append(kept, tail[index])
		total += size
	}
	dropped += len(tail) - len(kept)
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	return kept, dropped > 0
}

func migrationMessage(sourceLabel string, view *jsoncompat.Object, uiLocale, scope string) string {
	texts := migrationTextsByLocale[normalizeUILocale(uiLocale)]
	if scope == "latest" {
		texts.header, texts.cont = texts.latestHeader, texts.latestContinue
	}
	entries, truncated := migrationTranscript(view, scope)
	title, _ := view.MustGet("title").(string)
	cwd, _ := view.MustGet("cwd").(string)
	lines := []string{
		sprintf(texts.header, sourceLabel),
		texts.title + title,
		texts.directory + cwd,
	}
	if truncated {
		lines = append(lines, texts.truncated)
	}
	lines = append(lines, "")
	for _, entry := range entries {
		role := texts.roles[entry.role]
		if role == "" {
			role = entry.role
		}
		lines = append(lines, role+texts.sep+entry.text)
	}
	lines = append(lines, "", sprintf(texts.cont, sourceLabel))
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
	projects, _ := h.projectList().MustGet("projects").(*jsoncompat.Array)
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
	title := strings.TrimSpace(stringField(view, "title"))
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
	createdID := ""
	alreadySent := false
	if ledger != nil {
		if entry := ledger.lookup(requestID); entry != nil {
			if entry.Agent != targetKind || entry.Project != projectKey || entry.Title != title {
				h.fail(http.StatusBadRequest, errorBody("同一创建请求不能用于不同内容"))
				return
			}
			if entry.ID == "" {
				h.fail(http.StatusConflict, errorBody("上次创建结果尚不确定，请先刷新聊天列表并检查电脑 App，避免重复创建"))
				return
			}
			createdID = entry.ID
			alreadySent = entry.Sent
		}
	}
	if createdID == "" {
		if ledger != nil {
			ledger.begin(requestID, &creationEntry{Agent: targetKind, Project: projectKey,
				Title: title, Host: "local", At: ledger.now(), State: "creating"})
		}
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
			ledger.complete(requestID, createdID)
		}
	}

	message := migrationMessage(sourceLabel, view, uiLocale, scope)
	if ledger != nil {
		ledger.setMessage(requestID, message)
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
	// Codex creates the thread through a short-lived app-server, but the
	// desktop App must own it before follower IPC can inject the transcript.
	// Open it first and return promptly; the frontend retries after the target
	// chat has been opened instead of holding the tunnel request for 90s.
	if target == nil {
		entryState := ""
		if ledger != nil {
			if entry := ledger.lookup(requestID); entry != nil {
				entryState = entry.State
			}
		}
		if entryState != "opened" {
			if err := codex.OpenInDesktop(createdID); err != nil {
				if ledger != nil {
					ledger.markWaiting(requestID, err.Error())
				}
				result.Set("warning", err.Error())
				result.Set("state", "waiting_for_target")
				h.output(http.StatusOK, result, contentTypeJSON, "")
				return
			}
			if ledger != nil {
				ledger.markOpened(requestID)
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
				ledger.markWaiting(requestID, warning)
			}
			result.Set("warning", warning)
			result.Set("state", "waiting_for_target")
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
	}
	submissionID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:migrate:"+requestID)).String()
	sendErr := error(nil)
	if target != nil {
		session, openErr := target.Open(createdID, cwd, target.InitializeTimeout)
		if openErr != nil {
			sendErr = openErr
		} else {
			sendErr = session.Prompt(message, 3600*time.Second)
		}
	} else {
		_, sendErr = h.server.Codex.StartTurnWithOptions(createdID, submissionID, message, "send", "", nil, nil)
	}
	if sendErr != nil {
		if ledger != nil {
			ledger.markWaiting(requestID, sendErr.Error())
		}
		result.Set("warning", sendErr.Error())
		result.Set("state", "waiting_for_target")
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return
	}
	if ledger != nil {
		ledger.markSent(requestID)
	}
	result.Set("status", "sent")
	result.Set("sent", true)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}
