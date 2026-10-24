package httpd

// teams.go serves /api/teams*（docs/team-orchestration-plan.md §10.1/10.2/10.3
// 的阶段一子集）：Team/成员/Run 的认证、输入校验、幂等创建与乐观版本冲突
//（§8.4），cancel/resume 调用编排服务，以及每 Run 的事件投影——changes
// 长轮询 + events SSE（照 sessions.go 的 SSE 帧约定）。本文件不含任何调度或
// 恢复决策：那全部在 internal/team，这里只做参数校验、调用服务、返回投影。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// teamIDPattern 与 sessions.go threadRoute 的会话 id 同款形状；member id 走
// team.ValidMemberID（^[A-Za-z0-9_-]{1,64}$）。
var teamIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{4,127}$`)

const (
	// teamMaxMembers 限制单支团队的成员数（防误操作的松护栏）。
	teamMaxMembers = 50
	// teamGoalMaxRunes 是 Run 目标的字数上限（§10.2 校验层）。
	teamGoalMaxRunes = 10000
)

// handleTeams 分发 /api/teams* 路由。method 由挂载点传入（PATCH/DELETE 是
// web/team.js 契约使用的真实 HTTP 方法）；认证与 CSRF 已在挂载点完成。
func (h *handler) handleTeams(method string) {
	rest := strings.Trim(strings.TrimPrefix(h.request.URL.Path, "/api/teams"), "/")
	if rest == "" {
		switch method {
		case http.MethodGet:
			h.handleTeamsList()
		case http.MethodPost:
			h.handleTeamsCreate()
		default:
			h.fail(http.StatusNotFound, errorBody("页面不存在"))
		}
		return
	}
	segments := strings.Split(rest, "/")
	teamID := segments[0]
	if !teamIDPattern.MatchString(teamID) {
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
		return
	}
	switch {
	case len(segments) == 1:
		switch method {
		case http.MethodGet:
			h.handleTeamGet(teamID)
		case http.MethodPatch:
			h.handleTeamUpdate(teamID)
		case http.MethodDelete:
			h.handleTeamDelete(teamID)
		default:
			h.fail(http.StatusNotFound, errorBody("页面不存在"))
		}
	case len(segments) == 2 && segments[1] == "members" && method == http.MethodPost:
		h.handleMemberAdd(teamID)
	case len(segments) == 3 && segments[1] == "members":
		memberID := segments[2]
		if !team.ValidMemberID(memberID) {
			h.fail(http.StatusNotFound, errorBody("页面不存在"))
			return
		}
		switch method {
		case http.MethodPatch:
			h.handleMemberUpdate(teamID, memberID)
		case http.MethodDelete:
			h.handleMemberDelete(teamID, memberID)
		default:
			h.fail(http.StatusNotFound, errorBody("页面不存在"))
		}
	case len(segments) == 2 && segments[1] == "runs":
		switch method {
		case http.MethodGet:
			h.handleRunList(teamID)
		case http.MethodPost:
			h.handleRunCreate(teamID)
		default:
			h.fail(http.StatusNotFound, errorBody("页面不存在"))
		}
	case len(segments) == 3 && segments[1] == "runs" && method == http.MethodGet:
		h.handleRunGet(teamID, segments[2])
	case len(segments) == 4 && segments[1] == "runs":
		h.handleRunAction(method, teamID, segments[2], segments[3])
	default:
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
	}
}

// ---- Team 集合与单体 ----

func (h *handler) handleTeamsList() {
	rows := &jsoncompat.Array{}
	for _, teamRow := range h.server.TeamStore.Teams() {
		rows.Items = append(rows.Items, h.teamProject(teamRow))
	}
	result := jsoncompat.NewObject()
	result.Set("teams", rows)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// handleTeamsCreate 创建 Team（§10.1）。请求 id（可选 UUID）是幂等键：同 id
// 同内容重放返回已建团队 + duplicate:true（creations 模式）；异内容 400。
func (h *handler) handleTeamsCreate() {
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	requestID, _ := body.MustGet("id").(string)
	if requestID != "" {
		if _, parseErr := uuid.Parse(requestID); parseErr != nil {
			h.fail(http.StatusBadRequest, errorBody("创建请求标识无效"))
			return
		}
	}
	name := strings.TrimSpace(stringField(body, "name"))
	if name == "" || len([]rune(name)) > 120 {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–120 字的团队名称"))
		return
	}
	rawMembers, _ := body.MustGet("members").(*jsoncompat.Array)
	memberItems := []any{}
	if rawMembers != nil {
		memberItems = rawMembers.Items
	}
	if len(memberItems) > teamMaxMembers {
		h.fail(http.StatusBadRequest, errorBody(fmt.Sprintf("成员数量超过上限：最多 %d 名", teamMaxMembers)))
		return
	}
	members := make([]team.TeamMember, 0, len(memberItems))
	seen := map[string]bool{}
	bound := map[string]string{}
	for index, item := range memberItems {
		entry, ok := item.(*jsoncompat.Object)
		if !ok {
			h.fail(http.StatusBadRequest, errorBody("成员记录必须是对象"))
			return
		}
		member, memberErr := h.parseTeamMember(entry, index)
		if memberErr != nil {
			h.fail(http.StatusBadRequest, errorBody(memberErr.Error()))
			return
		}
		if seen[member.ID] {
			h.fail(http.StatusBadRequest, errorBody("成员 ID 已存在："+member.ID))
			return
		}
		if holder, held := bound[member.SessionID]; held {
			h.fail(http.StatusBadRequest, errorBody(
				"会话已被成员 "+holder+" 绑定，一个会话同一时间只能属于一个成员"))
			return
		}
		seen[member.ID] = true
		bound[member.SessionID] = member.ID
		members = append(members, member)
	}
	leadID := stringField(body, "leadId")
	if leadID != "" && !seen[leadID] {
		h.fail(http.StatusBadRequest, errorBody("负责人必须是团队成员"))
		return
	}
	if leadID == "" && len(members) > 0 {
		leadID = members[0].ID
	}
	policy, policyErr := parseTeamPolicy(body.MustGet("policy"), team.DefaultPolicy())
	if policyErr != nil {
		h.fail(http.StatusBadRequest, errorBody(policyErr.Error()))
		return
	}

	teamID := requestID
	if teamID != "" {
		if existing := h.server.TeamStore.Team(teamID); existing != nil {
			same := existing.Name == name && existing.LeadID == leadID &&
				sameTeamMembers(existing.Members, members) && existing.Policy == policy
			if !same {
				h.fail(http.StatusBadRequest, errorBody("同一创建请求不能用于不同内容"))
				return
			}
			result := h.teamProject(existing)
			result.Set("duplicate", true)
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
	} else {
		teamID = uuid.NewString()
	}
	now := time.Now()
	created := &team.Team{ID: teamID, Name: name, Members: members,
		LeadID: leadID, Policy: policy, CreatedAt: now, UpdatedAt: now}
	if err := h.server.TeamStore.SaveTeam(created); err != nil {
		h.fail(http.StatusConflict, errorBody("团队创建失败："+err.Error()))
		return
	}
	h.output(http.StatusOK, h.teamProject(created), contentTypeJSON, "")
}

func (h *handler) handleTeamGet(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	h.output(http.StatusOK, h.teamProject(teamRow), contentTypeJSON, "")
}

// handleTeamUpdate 处理 PATCH /api/teams/:id：name/leadId/policy 部分更新，
// 携带期望版本（§8.4，不符 409）。
func (h *handler) handleTeamUpdate(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	version, versionErr := parseTeamVersion(body)
	if versionErr != nil {
		h.fail(http.StatusBadRequest, errorBody(versionErr.Error()))
		return
	}
	if version != teamRow.Version {
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
		return
	}
	if raw, exists := body.Get("name"); exists {
		name := strings.TrimSpace(objectString(raw))
		if name == "" || len([]rune(name)) > 120 {
			h.fail(http.StatusBadRequest, errorBody("请输入 1–120 字的团队名称"))
			return
		}
		teamRow.Name = name
	}
	if raw, exists := body.Get("leadId"); exists {
		leadID := objectString(raw)
		if leadID != "" && teamMemberIndex(teamRow, leadID) < 0 {
			h.fail(http.StatusBadRequest, errorBody("负责人必须是团队成员"))
			return
		}
		teamRow.LeadID = leadID
	}
	policy, policyErr := parseTeamPolicy(body.MustGet("policy"), teamRow.Policy)
	if policyErr != nil {
		h.fail(http.StatusBadRequest, errorBody(policyErr.Error()))
		return
	}
	teamRow.Policy = policy
	teamRow.UpdatedAt = time.Now()
	if err := h.server.TeamStore.SaveTeam(teamRow); err != nil {
		h.failTeamStoreError(err)
		return
	}
	h.output(http.StatusOK, h.teamProject(teamRow), contentTypeJSON, "")
}

// handleTeamDelete 删除 Team 及其 Run 记录（§10.1：仍有非终态 Run 时 409，
// 用户必须先取消或等待结束）。期望版本经 query ?version= 或 JSON body 提供，
// 缺省视为无条件删除（阶段一前端未携带版本，活动 Run 拦截才是关键保护）。
func (h *handler) handleTeamDelete(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	version, hasVersion, versionErr := h.teamRequestVersion()
	if versionErr != nil {
		h.fail(http.StatusBadRequest, errorBody(versionErr.Error()))
		return
	}
	if hasVersion && version != teamRow.Version {
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
		return
	}
	for _, run := range h.server.TeamStore.RunsByTeam(teamID) {
		if !teamRunTerminal(run.Status) {
			h.fail(http.StatusConflict, errorBody("团队仍有未结束的 Run，请先取消或等待结束"))
			return
		}
	}
	if err := h.server.TeamStore.DeleteTeam(teamID, teamRow.Version); err != nil {
		h.failTeamStoreError(err)
		return
	}
	result := jsoncompat.NewObject()
	result.Set("ok", true)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// ---- 成员管理 ----

// handleMemberAdd 绑定一个既有 ACP 会话为新成员（§10.1）。
func (h *handler) handleMemberAdd(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	version, versionErr := parseTeamVersion(body)
	if versionErr != nil {
		h.fail(http.StatusBadRequest, errorBody(versionErr.Error()))
		return
	}
	if version != teamRow.Version {
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
		return
	}
	if len(teamRow.Members) >= teamMaxMembers {
		h.fail(http.StatusBadRequest, errorBody(fmt.Sprintf("成员数量超过上限：最多 %d 名", teamMaxMembers)))
		return
	}
	member, memberErr := h.parseTeamMember(body, len(teamRow.Members))
	if memberErr != nil {
		h.fail(http.StatusBadRequest, errorBody(memberErr.Error()))
		return
	}
	if teamMemberIndex(teamRow, member.ID) >= 0 {
		h.fail(http.StatusBadRequest, errorBody("成员 ID 已存在："+member.ID))
		return
	}
	if holder := teamSessionHolder(teamRow, member.SessionID); holder != "" {
		h.fail(http.StatusBadRequest, errorBody("会话已被成员 "+holder+" 绑定，一个会话同一时间只能属于一个成员"))
		return
	}
	teamRow.Members = append(teamRow.Members, member)
	teamRow.UpdatedAt = time.Now()
	if err := h.server.TeamStore.SaveTeam(teamRow); err != nil {
		h.failTeamStoreError(err)
		return
	}
	h.output(http.StatusOK, h.teamProject(teamRow), contentTypeJSON, "")
}

// handleMemberUpdate 处理 PATCH member：name/role/enabled 与换绑 sessionId
// （换绑重新解析 agentKind，非 ACP 会话仍拒绝）。
func (h *handler) handleMemberUpdate(teamID, memberID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	index := teamMemberIndex(teamRow, memberID)
	if index < 0 {
		h.fail(http.StatusNotFound, errorBody("成员不存在，请刷新团队"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	version, versionErr := parseTeamVersion(body)
	if versionErr != nil {
		h.fail(http.StatusBadRequest, errorBody(versionErr.Error()))
		return
	}
	if version != teamRow.Version {
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
		return
	}
	member := &teamRow.Members[index]
	if raw, exists := body.Get("name"); exists {
		name := strings.TrimSpace(objectString(raw))
		if len([]rune(name)) > 120 {
			h.fail(http.StatusBadRequest, errorBody("成员备注名过长：最多 120 字"))
			return
		}
		member.Name = name
	}
	if raw, exists := body.Get("role"); exists {
		role := objectString(raw)
		switch role {
		case team.RoleLead, team.RoleMember, team.RoleReviewer:
			member.Role = role
		default:
			h.fail(http.StatusBadRequest, errorBody("成员角色无效："+role))
			return
		}
	}
	if raw, exists := body.Get("enabled"); exists {
		enabled, ok := raw.(bool)
		if !ok {
			h.fail(http.StatusBadRequest, errorBody("成员启用状态必须是布尔值"))
			return
		}
		if !enabled && teamRow.LeadID == memberID {
			h.fail(http.StatusBadRequest, errorBody("不能停用负责人成员，请先更换负责人"))
			return
		}
		member.Enabled = enabled
	}
	if raw, exists := body.Get("sessionId"); exists {
		sessionID := objectString(raw)
		if !teamIDPattern.MatchString(sessionID) {
			h.fail(http.StatusBadRequest, errorBody("成员会话标识无效"))
			return
		}
		if sessionID != member.SessionID {
			if holder := teamSessionHolder(teamRow, sessionID); holder != "" {
				h.fail(http.StatusBadRequest, errorBody("会话已被成员 "+holder+" 绑定，一个会话同一时间只能属于一个成员"))
				return
			}
			kind, ok := h.server.teamMemberKind(sessionID)
			if !ok {
				h.fail(http.StatusBadRequest, errorBody("阶段一 Team 成员仅支持 ACP Agent 会话，请从会话列表选择"))
				return
			}
			member.SessionID = sessionID
			member.AgentKind = kind
		}
	}
	teamRow.UpdatedAt = time.Now()
	if err := h.server.TeamStore.SaveTeam(teamRow); err != nil {
		h.failTeamStoreError(err)
		return
	}
	h.output(http.StatusOK, h.teamProject(teamRow), contentTypeJSON, "")
}

func (h *handler) handleMemberDelete(teamID, memberID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	index := teamMemberIndex(teamRow, memberID)
	if index < 0 {
		h.fail(http.StatusNotFound, errorBody("成员不存在，请刷新团队"))
		return
	}
	if teamRow.LeadID == memberID {
		h.fail(http.StatusBadRequest, errorBody("不能移除负责人成员，请先更换负责人"))
		return
	}
	version, hasVersion, versionErr := h.teamRequestVersion()
	if versionErr != nil {
		h.fail(http.StatusBadRequest, errorBody(versionErr.Error()))
		return
	}
	if hasVersion && version != teamRow.Version {
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
		return
	}
	teamRow.Members = append(teamRow.Members[:index], teamRow.Members[index+1:]...)
	teamRow.UpdatedAt = time.Now()
	if err := h.server.TeamStore.SaveTeam(teamRow); err != nil {
		h.failTeamStoreError(err)
		return
	}
	h.output(http.StatusOK, h.teamProject(teamRow), contentTypeJSON, "")
}

// ---- Run 管理 ----

func (h *handler) handleRunList(teamID string) {
	if h.loadTeam(teamID) == nil {
		return
	}
	rows := &jsoncompat.Array{}
	for _, run := range h.server.TeamStore.RunsByTeam(teamID) {
		rows.Items = append(rows.Items, teamRunSummary(run))
	}
	result := jsoncompat.NewObject()
	result.Set("runs", rows)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// handleRunCreate 创建 Run（§10.2）：goal/mode/plan 校验后交 team.Service，
// 计划合法性（循环依赖、超任务数、prompt 余量上限）由 planner 的中文报错
// 直接透出为 400。
func (h *handler) handleRunCreate(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	goal := strings.TrimSpace(stringField(body, "goal"))
	if goal == "" || len([]rune(goal)) > teamGoalMaxRunes {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–10000 字的运行目标"))
		return
	}
	inputs, planErr := parsePlanInputs(body.MustGet("plan"))
	if planErr != nil {
		h.fail(http.StatusBadRequest, errorBody(planErr.Error()))
		return
	}
	mode := stringField(body, "mode")
	if mode == "" {
		// 未指定模式时按计划形状推断：多条任务即流水线，否则单任务。
		if len(inputs) > 1 {
			mode = team.ModePipeline
		} else {
			mode = team.ModeSingle
		}
	}
	// 先经 planner 纯校验，把计划类错误稳定映射为 400（CreateRun 内部还会
	// 再解析一次；两次之间团队被并发改动的窗口由第二次的报错兜底）。
	if _, err := team.ParsePlan(teamRow, goal, mode, inputs); err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	run, createErr := h.server.Teams.CreateRun(teamRow, goal, mode, inputs)
	if createErr != nil {
		h.fail(http.StatusConflict, errorBody(createErr.Error()))
		return
	}
	h.output(http.StatusOK, teamRunDetail(run), contentTypeJSON, "")
}

func (h *handler) handleRunGet(teamID, runID string) {
	run := h.loadRun(teamID, runID)
	if run == nil {
		return
	}
	h.output(http.StatusOK, teamRunDetail(run), contentTypeJSON, "")
}

// handleRunAction 分发 run 级动作与事件流端点（§10.2/§10.3）。amend 属阶段三，
// 显式 409；decision/questions 不注册（落到 404）。
func (h *handler) handleRunAction(method, teamID, runID, action string) {
	if !teamIDPattern.MatchString(runID) {
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
		return
	}
	if method == http.MethodGet {
		switch action {
		case "changes":
			h.handleTeamRunChanges(teamID, runID)
		case "events":
			h.handleTeamRunEvents(teamID, runID)
		default:
			h.fail(http.StatusNotFound, errorBody("页面不存在"))
		}
		return
	}
	if method != http.MethodPost {
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
		return
	}
	if h.loadRun(teamID, runID) == nil {
		return
	}
	switch action {
	case "cancel":
		run, err := h.server.Teams.Cancel(runID)
		if err != nil {
			h.failTeamError(err)
			return
		}
		h.output(http.StatusOK, teamRunDetail(run), contentTypeJSON, "")
	case "resume":
		run, err := h.server.Teams.Resume(runID)
		if err != nil {
			h.failTeamError(err)
			return
		}
		h.output(http.StatusOK, teamRunDetail(run), contentTypeJSON, "")
	case "amend":
		h.fail(http.StatusConflict, errorBody("修订 Run 在后续阶段提供"))
	default:
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
	}
}

// ---- 事件流（§10.3）----

// handleTeamRunChanges 是 changes 长轮询读（sessions.go sessionRouteChanges
// 的同款契约：web/transport.js 每 200ms 打点、恒带 start:” 与首次空 epoch）
// ——只认 after/epoch，其余未知参数一律忽略。游标已最新且 epoch 匹配时把
// 请求挂到 feed 有变更或等待上限（Notify 通道，投影 Add 时投递），不再
// 立即返回空页——否则轮询传输模式下每个打开的 Run 页都是稳定 5 QPS。
func (h *handler) handleTeamRunChanges(teamID, runID string) {
	if h.loadRun(teamID, runID) == nil {
		return
	}
	feed := h.server.TeamFeeds.Feed(runID)
	if feed == nil {
		h.fail(http.StatusNotFound, errorBody("运行不存在，请刷新列表"))
		return
	}
	query := h.request.URL.Query()
	after := uint64(0)
	if raw := query.Get("after"); raw != "" && raw != "-1" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody("事件游标无效，请刷新后重试"))
			return
		}
		after = parsed
	}
	epoch := query.Get("epoch")
	if after != feed.Sequence() || epoch != feed.Epoch() {
		// 已有增量（或 epoch 失配会走 reset 页）：立即返回。
		h.output(http.StatusOK, feed.Changes(after, epoch), contentTypeJSON, "")
		return
	}
	wait := h.server.TeamChangesWait
	if wait <= 0 {
		wait = 12 * time.Second
	}
	// 挂到「有新事件或等待上限」。Notify 通道容量 1 且不排干旧信号（创建期
	// 事件留下的残留会让首个 select 立即伪唤醒），醒来后必须复核游标确实
	// 前进，否则继续等——SSE 循环（teamStreamRunEvents）同款防伪唤醒。
	deadline := time.Now().Add(wait)
	for {
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-feed.Notify():
		case <-timer.C:
			h.output(http.StatusOK, feed.Changes(after, epoch), contentTypeJSON, "")
			return
		case <-h.request.Context().Done():
			timer.Stop()
			return // 客户端已断开：不再写响应。
		}
		timer.Stop()
		if after != feed.Sequence() || epoch != feed.Epoch() {
			h.output(http.StatusOK, feed.Changes(after, epoch), contentTypeJSON, "")
			return
		}
	}
}

// handleTeamRunEvents 是 events SSE 端点（transport SSE 模式打
// url('events')?incremental=1&since=ep.seq）；帧约定照 sessions.go 的
// streamIncremental：先 ready 帧、变化发 changes/reset 帧、帧 id=epoch.sequence、
// 12s 无变化心跳、每轮等待前后各查一次登录态、登出发 logout 帧。
func (h *handler) handleTeamRunEvents(teamID, runID string) {
	if h.loadRun(teamID, runID) == nil {
		return
	}
	feed := h.server.TeamFeeds.Feed(runID)
	if feed == nil {
		h.fail(http.StatusNotFound, errorBody("运行不存在，请刷新列表"))
		return
	}
	if h.request.URL.Query().Get("incremental") != "1" {
		// 非 incremental 读取保守兜底：返回一页当前增量而非 404。
		h.output(http.StatusOK, feed.Changes(0, ""), contentTypeJSON, "")
		return
	}
	h.teamStreamRunEvents(feed)
}

func (h *handler) teamStreamRunEvents(feed *team.EventFeed) {
	epoch, sequence := "", uint64(0)
	since := h.request.Header.Get("Last-Event-ID")
	if since == "" {
		since = h.request.URL.Query().Get("since")
	}
	if head, tail, found := strings.Cut(since, "."); found && head != "" && isDigits(tail) {
		epoch = head
		sequence, _ = strconv.ParseUint(tail, 10, 64)
	}
	header := h.writer.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("X-Accel-Buffering", "no")
	header.Set("Connection", "close")
	h.writer.WriteHeader(http.StatusOK)
	h.closeConnection = true
	welcome := jsoncompat.NewObject()
	welcome.Set("epoch", feed.Epoch())
	welcome.Set("sequence", jsoncompat.Num(int(feed.Sequence())))
	h.sseFrame("ready", welcome, "")
	lastCursor := feed.Cursor()
	for {
		if h.server.Auth.Get(h.cookieToken()) == nil {
			h.sseFrame("logout", &jsoncompat.Object{}, "")
			return
		}
		timer := time.NewTimer(12 * time.Second)
		select {
		case <-feed.Notify():
		case <-timer.C:
		case <-h.request.Context().Done():
			timer.Stop()
			return
		}
		timer.Stop()
		if h.server.Auth.Get(h.cookieToken()) == nil {
			h.sseFrame("logout", &jsoncompat.Object{}, "")
			return
		}
		if feed.Cursor() == lastCursor {
			h.writeRaw(": heartbeat\n\n")
			continue
		}
		payload := feed.Changes(sequence, epoch)
		event := "changes"
		if _, reset := payload.Get("reset"); reset {
			event = "reset"
		}
		epochValue, _ := payload.MustGet("epoch").(string)
		sequenceNumber, _ := payload.MustGet("sequence").(json.Number)
		parsed, _ := strconv.ParseUint(sequenceNumber.String(), 10, 64)
		epoch, sequence = epochValue, parsed
		lastCursor = epochValue + "." + sequenceNumber.String()
		h.sseFrame(event, payload, lastCursor)
	}
}

// ---- 事件投影装配（§10.3）----

// TeamFeedRegistry 为每个 Run 维护 EventFeed：Service.OnEvent 在 journal 落账
// 后同步调用 Ingest 把事实并入 feed；meta 注入 Run 状态摘要（status/
// stopReason/workspace/taskCounts，web/team.js 的 meta 消费面）。
type TeamFeedRegistry struct {
	mu      sync.Mutex
	journal *team.Journal
	store   team.TeamStore
	feeds   map[string]*team.EventFeed
}

// NewTeamFeedRegistry 创建空投影注册表；journal 仅用于启动期 prime。
func NewTeamFeedRegistry(journal *team.Journal, store team.TeamStore) *TeamFeedRegistry {
	return &TeamFeedRegistry{journal: journal, store: store, feeds: map[string]*team.EventFeed{}}
}

// PrimeFromJournal 在网关启动（RecoverAll 之前）重放 journal，为出现过事件的
// Run 预建 feed（环形缓冲保留各 Run 最近 500 条）。此后恢复期产生的事件经
// Ingest 继续并入。
func (r *TeamFeedRegistry) PrimeFromJournal() error {
	events, err := r.journal.Replay()
	if err != nil {
		return err
	}
	byRun := map[string][]*team.Event{}
	for _, event := range events {
		byRun[event.RunID] = append(byRun[event.RunID], event)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for runID, seed := range byRun {
		feed := team.NewEventFeed(runID, seed)
		feed.SetMeta(teamRunMeta(r.store.Run(runID)))
		r.feeds[runID] = feed
	}
	return nil
}

// Feed 返回（或惰性创建）runID 的事件投影并刷新 meta；run 不存在返回 nil。
// 读路径绝不因一次 changes 请求重放整个 journal——启动 prime 已覆盖历史。
func (r *TeamFeedRegistry) Feed(runID string) *team.EventFeed {
	r.mu.Lock()
	defer r.mu.Unlock()
	feed := r.feeds[runID]
	if feed == nil {
		if r.store.Run(runID) == nil {
			return nil
		}
		feed = team.NewEventFeed(runID, nil)
		r.feeds[runID] = feed
	}
	feed.SetMeta(teamRunMeta(r.store.Run(runID)))
	return feed
}

// Ingest 是 Service.OnEvent 的装配点：首条事件（run_created）即建 feed，
// 之后按 journal 序号单调并入（EventFeed.Add 自行拒绝乱序/重复）。
func (r *TeamFeedRegistry) Ingest(event *team.Event) {
	if event == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	feed := r.feeds[event.RunID]
	if feed == nil {
		feed = team.NewEventFeed(event.RunID, nil)
		r.feeds[event.RunID] = feed
	}
	if feed.Add(event) {
		feed.SetMeta(teamRunMeta(r.store.Run(event.RunID)))
	}
}

// ---- 投影 ----

// teamProject 输出 Team 的 API 投影：账本全字段 + 每成员的 available 标记
// （§4.1：引用失效保留记录并标记，不静默改派）。
func (h *handler) teamProject(teamRow *team.Team) *jsoncompat.Object {
	raw, err := teamRow.MarshalJSON()
	if err != nil {
		return jsoncompat.NewObject()
	}
	value, _ := jsoncompat.Unmarshal(raw)
	object, _ := value.(*jsoncompat.Object)
	if object == nil {
		return jsoncompat.NewObject()
	}
	if members, ok := object.MustGet("members").(*jsoncompat.Array); ok {
		for _, item := range members.Items {
			member, ok := item.(*jsoncompat.Object)
			if !ok {
				continue
			}
			member.Set("available", h.server.teamMemberAvailable(objectString(member.MustGet("sessionId"))))
		}
	}
	return object
}

// teamRunDetail 组装 RunDetail 投影：Run 全字段 + workspace.writeLockHeldBy
// （§12 工作项 5）+ 每任务 result:{sessionId,turnId,result,resultChars,
// truncated}|null（§6.5，由 TaskResult 组装；无结果为 null）。
func teamRunDetail(run *team.Run) *jsoncompat.Object {
	raw, err := run.MarshalJSON()
	if err != nil {
		return jsoncompat.NewObject()
	}
	value, _ := jsoncompat.Unmarshal(raw)
	detail, _ := value.(*jsoncompat.Object)
	if detail == nil {
		return jsoncompat.NewObject()
	}
	if tasks, ok := detail.MustGet("tasks").(*jsoncompat.Array); ok {
		for _, item := range tasks.Items {
			task, isObject := item.(*jsoncompat.Object)
			if !isObject {
				continue
			}
			if rawDetail, hasDetail := task.Get("resultDetail"); hasDetail {
				if detailObject, isObject := rawDetail.(*jsoncompat.Object); isObject {
					result := jsoncompat.NewObject()
					for _, key := range []string{"sessionId", "turnId", "result", "resultChars", "truncated"} {
						if value, exists := detailObject.Get(key); exists {
							result.Set(key, value)
						}
					}
					task.Set("result", result)
				}
			} else if text, isString := task.MustGet("result").(string); !isString || text == "" {
				// 无结果引用也无结果文本：显式 null（前端按 null 渲染空卡）。
				task.Set("result", nil)
			}
			task.Delete("resultDetail")
		}
	}
	workspace := jsoncompat.NewObject()
	workspace.Set("writeLockHeldBy", teamWriteLockHolder(run))
	detail.Set("workspace", workspace)
	return detail
}

// teamRunSummary 是 Run 列表行的轻量投影（不含任务与结果本体）。
func teamRunSummary(run *team.Run) *jsoncompat.Object {
	summary := jsoncompat.NewObject()
	summary.Set("id", run.ID)
	summary.Set("teamId", run.TeamID)
	summary.Set("goal", run.Goal)
	summary.Set("status", run.Status)
	if run.StopReason != "" {
		summary.Set("stopReason", run.StopReason)
	}
	summary.Set("round", jsoncompat.Num(run.Round))
	summary.Set("planHash", run.PlanHash)
	summary.Set("taskCounts", teamTaskCounts(run))
	summary.Set("createdAt", run.CreatedAt.Format(time.RFC3339Nano))
	summary.Set("updatedAt", run.UpdatedAt.Format(time.RFC3339Nano))
	summary.Set("version", json.Number(strconv.FormatUint(run.Version, 10)))
	return summary
}

// teamRunMeta 是 Changes/SSE 随行 meta 的 Run 状态摘要。
func teamRunMeta(run *team.Run) *jsoncompat.Object {
	meta := jsoncompat.NewObject()
	if run == nil {
		return meta
	}
	meta.Set("runId", run.ID)
	meta.Set("teamId", run.TeamID)
	meta.Set("status", run.Status)
	meta.Set("stopReason", run.StopReason)
	meta.Set("taskCounts", teamTaskCounts(run))
	workspace := jsoncompat.NewObject()
	workspace.Set("writeLockHeldBy", teamWriteLockHolder(run))
	meta.Set("workspace", workspace)
	return meta
}

func teamTaskCounts(run *team.Run) *jsoncompat.Object {
	counts := jsoncompat.NewObject()
	if run == nil {
		return counts
	}
	seen := map[string]int{}
	order := []string{}
	for i := range run.Tasks {
		status := run.Tasks[i].Status
		if _, exists := seen[status]; !exists {
			order = append(order, status)
		}
		seen[status]++
	}
	for _, status := range order {
		counts.Set(status, jsoncompat.Num(seen[status]))
	}
	return counts
}

// teamWriteLockHolder 从 Run 快照推导共享工作区写锁持有者（§12 工作项 5 的
// 读写状态投影）：处于 dispatching/running 且要求独占写锁的任务即当前持锁者
// （LockRegistry 在 team.Service 内部，持锁期与该状态区间一致）；多个 cwd
// 并存时按创建序（拓扑序）取第一个。无人持锁返回 nil。
func teamWriteLockHolder(run *team.Run) any {
	if run == nil {
		return nil
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.WriteLock == team.TaskWriteLockNone {
			continue
		}
		switch task.Status {
		case team.TaskDispatching, team.TaskRunning:
			return task.ID
		}
	}
	return nil
}

// teamRunTerminal 与 team.terminalRun 同集（Run 终态判定未导出，此处按
// model.go 的状态常量镜像）。
func teamRunTerminal(status string) bool {
	switch status {
	case team.RunCompleted, team.RunFailed, team.RunCancelled, team.RunStopped, team.RunOutcomeUnknown:
		return true
	}
	return false
}

// ---- 校验与共享小件 ----

// loadTeam 读取并校验 teamID，失败时已写出 404。
func (h *handler) loadTeam(teamID string) *team.Team {
	if !teamIDPattern.MatchString(teamID) {
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
		return nil
	}
	teamRow := h.server.TeamStore.Team(teamID)
	if teamRow == nil {
		h.fail(http.StatusNotFound, errorBody("团队不存在，请刷新列表"))
		return nil
	}
	return teamRow
}

// loadRun 读取并校验 run 的归属（teamID 必须匹配），失败时已写出 404。
func (h *handler) loadRun(teamID, runID string) *team.Run {
	if !teamIDPattern.MatchString(runID) {
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
		return nil
	}
	run := h.server.TeamStore.Run(runID)
	if run == nil || run.TeamID != teamID {
		h.fail(http.StatusNotFound, errorBody("运行不存在，请刷新列表"))
		return nil
	}
	return run
}

// failTeamError 映射编排服务的哨兵错误：NotFound→404、版本冲突→409『版本
// 冲突，请刷新后重试』；ErrRunFinished/ErrRunNotWaiting 等状态冲突走默认 409
// 并透出 team 侧中文文案。
func (h *handler) failTeamError(err error) {
	switch {
	case errors.Is(err, team.ErrNotFound):
		h.fail(http.StatusNotFound, errorBody("运行不存在，请刷新列表"))
	case errors.Is(err, team.ErrVersionConflict):
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
	default:
		h.fail(http.StatusConflict, errorBody(err.Error()))
	}
}

// failTeamStoreError 映射账本写入错误（外部写者抢先更新等）。仍有非终态
// Run 的删除被账本层原子拒绝（ErrActiveRuns，与 HTTP 预检同文案的 409）。
func (h *handler) failTeamStoreError(err error) {
	switch {
	case errors.Is(err, team.ErrVersionConflict):
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
	case errors.Is(err, team.ErrActiveRuns):
		h.fail(http.StatusConflict, errorBody("团队仍有未结束的 Run，请先取消或等待结束"))
	default:
		h.fail(http.StatusConflict, errorBody(err.Error()))
	}
}

// parseTeamVersion 读取 body.version（uint64）；缺失/非法报中文错误。
func parseTeamVersion(body *jsoncompat.Object) (uint64, error) {
	raw, ok := body.Get("version")
	if !ok {
		return 0, errors.New("缺少版本号，请刷新后重试")
	}
	number, isNumber := raw.(json.Number)
	if !isNumber {
		return 0, errors.New("版本号必须是整数")
	}
	parsed, err := strconv.ParseUint(number.String(), 10, 64)
	if err != nil {
		return 0, errors.New("版本号必须是整数")
	}
	return parsed, nil
}

// teamRequestVersion 读取 DELETE 的期望版本：query ?version= 优先，缺省再试
// JSON body（团队面板的 DELETE 携带 JSON body 但当前不带版本）。两者都缺省
// 表示无条件删除。
func (h *handler) teamRequestVersion() (uint64, bool, error) {
	if raw := h.request.URL.Query().Get("version"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return 0, false, errors.New("版本号必须是整数")
		}
		return parsed, true, nil
	}
	if value, err := h.readJSON(); err == nil {
		if number, ok := value.(*jsoncompat.Object).MustGet("version").(json.Number); ok {
			parsed, parseErr := strconv.ParseUint(number.String(), 10, 64)
			if parseErr != nil {
				return 0, false, errors.New("版本号必须是整数")
			}
			return parsed, true, nil
		}
	}
	return 0, false, nil
}

// parseTeamMember 校验并组装单个成员（创建数组元素与 member 新增共用）：
// id 过成员正则、sessionId 过会话 id 形状、角色枚举（默认 member）、备注名
// ≤120 字，并解析 agentKind——阶段一仅接受本网关可归属的 ACP 会话。
func (h *handler) parseTeamMember(entry *jsoncompat.Object, index int) (team.TeamMember, error) {
	id := stringField(entry, "id")
	if !team.ValidMemberID(id) {
		return team.TeamMember{}, fmt.Errorf(
			"第 %d 个成员 ID 无效：%q（字母/数字/下划线/连字符，1-64 字符）", index+1, id)
	}
	sessionID := stringField(entry, "sessionId")
	if !teamIDPattern.MatchString(sessionID) {
		return team.TeamMember{}, fmt.Errorf("成员 %s 的会话标识无效", id)
	}
	name := strings.TrimSpace(stringField(entry, "name"))
	if len([]rune(name)) > 120 {
		return team.TeamMember{}, fmt.Errorf("成员 %s 的备注名过长：最多 120 字", id)
	}
	role := stringField(entry, "role")
	switch role {
	case "":
		role = team.RoleMember
	case team.RoleLead, team.RoleMember, team.RoleReviewer:
	default:
		return team.TeamMember{}, fmt.Errorf("成员 %s 的角色无效：%s", id, role)
	}
	kind, ok := h.server.teamMemberKind(sessionID)
	if !ok {
		return team.TeamMember{}, errors.New("阶段一 Team 成员仅支持 ACP Agent 会话，请从会话列表选择")
	}
	enabled := true
	if raw, exists := entry.Get("enabled"); exists {
		value, isBool := raw.(bool)
		if !isBool {
			return team.TeamMember{}, fmt.Errorf("成员 %s 的启用状态必须是布尔值", id)
		}
		enabled = value
	}
	return team.TeamMember{ID: id, SessionID: sessionID, AgentKind: kind,
		Name: name, Role: role, Enabled: enabled}, nil
}

// teamPolicyIntField 描述 policy 的一个整数字段（校验下限与写入目标），
// 固定顺序遍历保证报错稳定。
type teamPolicyIntField struct {
	key   string
	min   int64
	apply func(policy *team.TeamPolicy, value int)
}

var teamPolicyIntFields = []teamPolicyIntField{
	{"maxParallelTasks", 1, func(p *team.TeamPolicy, v int) { p.MaxParallelTasks = v }},
	{"minParallelTasks", 1, func(p *team.TeamPolicy, v int) { p.MinParallelTasks = v }},
	{"maxProviderConcurrency", 1, func(p *team.TeamPolicy, v int) { p.MaxProviderConcurrency = v }},
	{"maxRounds", 1, func(p *team.TeamPolicy, v int) { p.MaxRounds = v }},
	{"maxTasksPerRun", 1, func(p *team.TeamPolicy, v int) { p.MaxTasksPerRun = v }},
	{"maxContextChars", 0, func(p *team.TeamPolicy, v int) { p.MaxContextChars = v }},
	// maxResultChars 下限 1：0 会被 ExtractResult 解释为「不截断」，一条
	// {"policy":{"maxResultChars":0}} 就让任务结果完全不设界（Task.Result、
	// team-runs.json、事件 payload 与 SSE 帧全带 Agent 全文输出，违反
	// §10.3/§15.3 的有界 payload）。与 maxContextChars 的 0=不注入语义
	// 相反，这里 0 一律拒绝。
	{"maxResultChars", 1, func(p *team.TeamPolicy, v int) { p.MaxResultChars = v }},
	{"taskTimeoutSecs", 1, func(p *team.TeamPolicy, v int) { p.TaskTimeoutSecs = v }},
	{"runTimeoutSecs", 1, func(p *team.TeamPolicy, v int) { p.RunTimeoutSecs = v }},
	{"budgetUnits", 0, func(p *team.TeamPolicy, v int) { p.BudgetUnits = v }},
}

// parseTeamPolicy 把请求里的 policy 部分字段合并进 base（未提供的字段保持
// base 值）。isolated_worktree 与 autoApplyChanges 属后续阶段，显式拒绝；
// 未知字段忽略。
func parseTeamPolicy(raw any, base team.TeamPolicy) (team.TeamPolicy, error) {
	policy := base
	if raw == nil {
		return policy, nil
	}
	object, ok := raw.(*jsoncompat.Object)
	if !ok {
		return base, errors.New("策略必须是对象")
	}
	for _, field := range teamPolicyIntFields {
		value, exists := object.Get(field.key)
		if !exists {
			continue
		}
		number, isNumber := value.(json.Number)
		if !isNumber {
			return base, fmt.Errorf("策略字段 %s 必须是整数", field.key)
		}
		parsed, err := number.Int64()
		if err != nil || parsed < field.min {
			return base, fmt.Errorf("策略字段 %s 必须是不小于 %d 的整数", field.key, field.min)
		}
		field.apply(&policy, int(parsed))
	}
	if value, exists := object.Get("workspaceMode"); exists {
		text := objectString(value)
		switch text {
		case team.WorkspaceShared:
			policy.WorkspaceMode = text
		case team.WorkspaceIsolatedWorktree:
			return base, errors.New("独立工作区在后续阶段提供")
		default:
			return base, fmt.Errorf("工作区模式无效：%s", text)
		}
	}
	if value, exists := object.Get("writePolicy"); exists {
		text := objectString(value)
		switch text {
		case team.WritePolicyExclusive, team.WritePolicyParallel:
			policy.WritePolicy = text
		default:
			return base, fmt.Errorf("写策略无效：%s", text)
		}
	}
	if value, exists := object.Get("autoDispatch"); exists {
		enabled, isBool := value.(bool)
		if !isBool {
			return base, errors.New("策略字段 autoDispatch 必须是布尔值")
		}
		policy.AutoDispatch = enabled
	}
	if value, exists := object.Get("autoApplyChanges"); exists {
		if enabled, isBool := value.(bool); isBool && enabled {
			return base, errors.New("自动应用改动在后续阶段提供")
		}
		policy.AutoApplyChanges = false
	}
	return policy, nil
}

// parsePlanInputs 解析创建 Run 请求的 plan 数组（§10.2）；字段合法性
// （成员存在、依赖闭环、长度上限等）由 team.ParsePlan 统一裁决。
func parsePlanInputs(raw any) ([]team.PlanTaskInput, error) {
	if raw == nil {
		return nil, nil
	}
	array, ok := raw.(*jsoncompat.Array)
	if !ok {
		return nil, errors.New("计划必须是数组")
	}
	inputs := make([]team.PlanTaskInput, 0, len(array.Items))
	for _, item := range array.Items {
		object, isObject := item.(*jsoncompat.Object)
		if !isObject {
			return nil, errors.New("计划任务必须是对象")
		}
		input := team.PlanTaskInput{
			ID:            stringField(object, "id"),
			MemberID:      stringField(object, "memberId"),
			Prompt:        stringField(object, "prompt"),
			WriteLock:     stringField(object, "writeLock"),
			WorkspaceMode: stringField(object, "workspaceMode"),
		}
		if deps, hasDeps := object.MustGet("dependsOn").(*jsoncompat.Array); hasDeps {
			for _, dep := range deps.Items {
				text, isString := dep.(string)
				if !isString {
					return nil, errors.New("依赖列表必须全是任务 ID")
				}
				input.DependsOn = append(input.DependsOn, text)
			}
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

func teamMemberIndex(teamRow *team.Team, memberID string) int {
	for i := range teamRow.Members {
		if teamRow.Members[i].ID == memberID {
			return i
		}
	}
	return -1
}

func teamSessionHolder(teamRow *team.Team, sessionID string) string {
	for i := range teamRow.Members {
		if teamRow.Members[i].SessionID == sessionID {
			return teamRow.Members[i].ID
		}
	}
	return ""
}

func sameTeamMembers(a, b []team.TeamMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func objectString(value any) string {
	text, _ := value.(string)
	return text
}
