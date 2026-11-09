package httpd

// team_inputs_queue.go 是持久输入队列的 HTTP 面（docs/team-demand-queue-plan.md
// §3.2，阶段 1）。与既有 TeamInput 入口按版本分流：请求体带 queueVersion:2
//（POST /inputs）或 teamQueueVersion:2（主控会话发送）才走持久队列，返回
// 202 {inputId, queueStatus:"queued", runIds:[]}；旧请求保持「立即给 runId」
// 的同步契约不变。排队操作（编辑/优先/取消）以操作 UUID 幂等、携带期望版本；
// 容量满 429、内容冲突与版本冲突 409。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// teamQueueVersionGate 是持久队列路径的版本开关取值（§3.2：完成切换且旧
// 客户端兼容窗口结束后，再考虑收敛双入口）。
const teamQueueVersionGate = "2"

// 新版画布消息使用持久输入账本，即使用户没有开启可选的排队发送开关。
const teamIssueRoutingVersionGate = "1"

// queueTeamInputValidate 校验并归一一条排队输入的正文与附件（与旧入口
// createTeamInput 同一口径），返回 (text, attachments, err)。threadID 是附件
// 上传账本的归属会话。
func (h *handler) queueTeamInputValidate(threadID, rawGoal string, attachments []string) (string, error) {
	goal := strings.TrimSpace(rawGoal)
	if goal == "" && len(attachments) > 0 {
		goal = "请查看用户发送的图片和附件。"
	}
	if goal == "" || len([]rune(goal)) > teamGoalMaxRunes {
		return "", &teamInputError{http.StatusBadRequest, "请输入 1–10000 字的团队消息，或附带图片后重试"}
	}
	for _, id := range attachments {
		if h.server.Uploads == nil {
			return "", &teamInputError{http.StatusBadRequest, "当前网关未启用附件上传"}
		}
		if _, err := h.server.Uploads.Get(threadID, id); err != nil {
			return "", &teamInputError{http.StatusBadRequest, err.Error()}
		}
	}
	return goal, nil
}

// acceptQueuedTeamInput 是两条入口（POST /inputs queueVersion:2 与主控会话
// teamQueueVersion:2）的共用受理体：校验 → 账本 Accept（同 ID 幂等）→ 202。
func (h *handler) acceptQueuedTeamInput(teamRow *team.Team, threadID, inputID, rawGoal string, attachments []string, issueRouting bool) (*team.TeamInput, bool, error) {
	if inputID == "" {
		return nil, false, &teamInputError{http.StatusBadRequest, "缺少消息标识，请刷新后重试"}
	}
	goal, validateErr := h.queueTeamInputValidate(threadID, rawGoal, attachments)
	if validateErr != nil {
		return nil, false, validateErr
	}
	capacity := teamRow.Policy.MaxPendingInputs
	input, duplicate, err := h.server.TeamInputs.AcceptWithIssueRouting(teamRow.ID, inputID, goal, attachments, team.PolicyDigest(teamRow.Policy), capacity, issueRouting)
	if err != nil {
		return nil, false, err
	}
	// 受理即泵路由（阶段 2，灰度开关）：无歧义输入自动派发到 Run，停靠
	// 原因落输入投影；关闭时输入保持排队，由 RoutePendingInputs/泵循环驱动。
	if h.server.TeamQueueAutoRoute {
		h.server.Teams.PumpRouting()
	}
	return input, duplicate, nil
}

// outputQueuedTeamInput 输出受理结果（202）：先持久化再返回——runIds 恒为
// 空数组，路由结果是后续投影（阶段 2 起填充）。
func (h *handler) outputQueuedTeamInput(input *team.TeamInput, duplicate bool) {
	result := jsoncompat.NewObject()
	result.Set("status", "queued")
	result.Set("inputId", input.ID)
	result.Set("id", input.ID)
	result.Set("teamId", input.TeamID)
	result.Set("queueStatus", input.QueueStatus)
	result.Set("associationStatus", input.AssociationStatus)
	result.Set("issueRouting", input.IssueRouting)
	result.Set("sequence", jsoncompat.Num(int(input.Sequence)))
	result.Set("revision", jsoncompat.Num(input.Revision))
	result.Set("text", input.Text)
	result.Set("acceptedAt", input.AcceptedAt.Format(time.RFC3339Nano))
	result.Set("version", jsoncompat.Num(int(input.Version)))
	result.Set("runIds", &jsoncompat.Array{})
	if duplicate {
		result.Set("duplicate", true)
	}
	h.output(http.StatusAccepted, result, contentTypeJSON, "")
}

func (h *handler) outputAcceptedTeamInput(input *team.TeamInput, duplicate bool) {
	result := jsoncompat.NewObject()
	result.Set("status", "accepted")
	result.Set("id", input.ID)
	result.Set("inputId", input.ID)
	result.Set("teamId", input.TeamID)
	result.Set("queueStatus", input.QueueStatus)
	result.Set("associationStatus", input.AssociationStatus)
	result.Set("issueRouting", input.IssueRouting)
	result.Set("sequence", jsoncompat.Num(int(input.Sequence)))
	result.Set("revision", jsoncompat.Num(input.Revision))
	result.Set("text", input.Text)
	result.Set("acceptedAt", input.AcceptedAt.Format(time.RFC3339Nano))
	result.Set("runIds", &jsoncompat.Array{Items: []any{}})
	if duplicate {
		result.Set("duplicate", true)
	}
	h.output(http.StatusAccepted, result, contentTypeJSON, "")
}

// teamQueueInputProject 是排队输入的 API 投影（queueVersion:2 家族）：完整
// 生命周期字段 + 修订历史（每个正文版本不可变）。
func teamQueueInputProject(input *team.TeamInput) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("inputId", input.ID)
	object.Set("id", input.ID)
	object.Set("teamId", input.TeamID)
	object.Set("queueVersion", jsoncompat.Num(2))
	object.Set("queueStatus", input.QueueStatus)
	object.Set("routeStatus", input.RouteStatus)
	associationStatus := input.AssociationStatus
	if associationStatus == "" {
		associationStatus = "unclassified"
	}
	object.Set("associationStatus", associationStatus)
	object.Set("issueRouting", input.IssueRouting)
	if input.AssociationReason != "" {
		object.Set("associationReason", input.AssociationReason)
	}
	object.Set("sequence", jsoncompat.Num(int(input.Sequence)))
	object.Set("revision", jsoncompat.Num(input.Revision))
	object.Set("version", jsoncompat.Num(int(input.Version)))
	object.Set("text", input.Text)
	if len(input.AttachmentIDs) > 0 {
		attachments := &jsoncompat.Array{}
		attachments.Items = append(attachments.Items, toAnySlice(input.AttachmentIDs)...)
		object.Set("attachmentIds", attachments)
	}
	object.Set("priority", input.Priority)
	if input.TargetRunID != "" {
		object.Set("targetRunId", input.TargetRunID)
	}
	if input.TargetTaskID != "" {
		object.Set("targetTaskId", input.TargetTaskID)
	}
	if input.ParentRunID != "" {
		object.Set("parentRunId", input.ParentRunID)
	}
	if len(input.RunIDs) > 0 {
		runIDs := &jsoncompat.Array{}
		runIDs.Items = append(runIDs.Items, toAnySlice(input.RunIDs)...)
		object.Set("runIds", runIDs)
	}
	if len(input.WorkItemIDs) > 0 {
		workItemIDs := &jsoncompat.Array{}
		workItemIDs.Items = append(workItemIDs.Items, toAnySlice(input.WorkItemIDs)...)
		object.Set("workItemIds", workItemIDs)
	}
	if len(input.WorkItems) > 0 {
		workItems := &jsoncompat.Array{}
		for i := range input.WorkItems {
			item := input.WorkItems[i]
			entry := jsoncompat.NewObject()
			entry.Set("id", item.ID)
			entry.Set("sourceInputId", item.SourceInputID)
			entry.Set("sourceRevision", jsoncompat.Num(item.SourceRevision))
			entry.Set("requirementText", item.RequirementText)
			entry.Set("sourceStart", jsoncompat.Num(item.SourceStart))
			entry.Set("sourceEnd", jsoncompat.Num(item.SourceEnd))
			if item.MemberID != "" {
				entry.Set("memberId", item.MemberID)
			}
			if item.ReviewPolicy != "" {
				entry.Set("reviewPolicy", item.ReviewPolicy)
			}
			if item.RunID != "" {
				entry.Set("runId", item.RunID)
			}
			if len(item.TaskIDs) > 0 {
				taskIDs := &jsoncompat.Array{}
				taskIDs.Items = append(taskIDs.Items, toAnySlice(item.TaskIDs)...)
				entry.Set("taskIds", taskIDs)
			}
			if len(item.DependsOnItemIDs) > 0 {
				deps := &jsoncompat.Array{}
				deps.Items = append(deps.Items, toAnySlice(item.DependsOnItemIDs)...)
				entry.Set("dependsOnItemIds", deps)
			}
			entry.Set("status", item.Status)
			if item.EvidenceRef != "" {
				entry.Set("evidenceRef", item.EvidenceRef)
			}
			workItems.Items = append(workItems.Items, entry)
		}
		object.Set("workItems", workItems)
	}
	if len(input.IssueLinks) > 0 {
		links := &jsoncompat.Array{}
		for _, link := range input.IssueLinks {
			entry := jsoncompat.NewObject()
			entry.Set("issueId", link.IssueID)
			entry.Set("inputId", link.InputID)
			entry.Set("revision", jsoncompat.Num(link.Revision))
			entry.Set("sourceStart", jsoncompat.Num(link.SourceStart))
			entry.Set("sourceEnd", jsoncompat.Num(link.SourceEnd))
			entry.Set("sourceText", link.SourceText)
			links.Items = append(links.Items, entry)
		}
		object.Set("issueLinks", links)
	}
	if input.WaitingReason != "" {
		object.Set("waitingReason", input.WaitingReason)
	}
	revisions := &jsoncompat.Array{}
	for i := range input.Revisions {
		revision := input.Revisions[i]
		entry := jsoncompat.NewObject()
		entry.Set("revision", jsoncompat.Num(revision.Revision))
		entry.Set("text", revision.Text)
		if len(revision.AttachmentIDs) > 0 {
			attachments := &jsoncompat.Array{}
			attachments.Items = append(attachments.Items, toAnySlice(revision.AttachmentIDs)...)
			entry.Set("attachmentIds", attachments)
		}
		entry.Set("createdAt", revision.CreatedAt.Format(time.RFC3339Nano))
		if revision.ReplacedBy > 0 {
			entry.Set("replacedBy", jsoncompat.Num(revision.ReplacedBy))
		}
		revisions.Items = append(revisions.Items, entry)
	}
	object.Set("revisions", revisions)
	object.Set("acceptedAt", input.AcceptedAt.Format(time.RFC3339Nano))
	object.Set("updatedAt", input.UpdatedAt.Format(time.RFC3339Nano))
	if input.Historical {
		object.Set("historical", true)
		object.Set("historicalSourceRunId", input.HistoricalSourceRunID)
		object.Set("historicalSourceTaskId", input.HistoricalSourceTaskID)
	}
	return object
}

func (h *handler) teamInputProject(input *team.TeamInput) *jsoncompat.Object {
	if input == nil {
		return jsoncompat.NewObject()
	}
	object := teamQueueInputProject(input)
	if !input.IssueRouting || input.AssociationStatus != team.IssueAssociationPending {
		return object
	}
	var run *team.Run
	var rootTask *team.Task
	if input.Historical {
		if candidate := h.server.TeamStore.Run(input.ID); candidate != nil && candidate.IsIssueClassificationOnly() && candidate.TeamID == input.TeamID {
			run = candidate
			for index := range candidate.Tasks {
				if candidate.Tasks[index].ID == "lead" {
					rootTask = &candidate.Tasks[index]
					break
				}
			}
		}
	} else {
		run, rootTask = h.findTeamInput(input.TeamID, input.ID)
	}
	task := rootTask
	if run != nil && rootTask != nil {
		leadID := ""
		if teamRow := h.server.TeamStore.Team(input.TeamID); teamRow != nil {
			leadID = teamRow.LeadID
		}
		repairPrefix := "actfix-" + rootTask.ID + "-"
		for index := range run.Tasks {
			candidate := &run.Tasks[index]
			if candidate.MemberID == leadID && candidate.ExecutionContext == team.ExecutionContextLeadControl &&
				strings.HasPrefix(candidate.ID, repairPrefix) && candidate.Ordinal > task.Ordinal {
				task = candidate
			}
		}
	}
	failedRun := run != nil && (run.Status == team.RunWaitingInput || run.Status == team.RunPaused || teamRunTerminal(run.Status))
	failedTask := task != nil && (task.Status == team.TaskFailed || task.Status == team.TaskStopped || task.Status == team.TaskCancelled)
	if !failedRun && !failedTask {
		return object
	}
	object.Set("associationStatus", team.IssueAssociationFailed)
	reason := ""
	if run != nil {
		reason = strings.TrimSpace(run.StopReason)
	}
	if task != nil && task.Error != "" {
		reason = strings.TrimSpace(task.Error)
	}
	if reason == "" {
		if task != nil {
			reason = strings.TrimSpace(task.StopReason)
		}
	}
	if reason != "" {
		object.Set("associationReason", reason)
	}
	return object
}

func toAnySlice(values []string) []any {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}

// failTeamInputError 把输入账本错误映射为 HTTP 状态（内容/版本/生命周期冲
// 突 409，容量 429，缺失 404）。
func (h *handler) failTeamInputError(err error) {
	var inputErr *teamInputError
	if errors.As(err, &inputErr) {
		h.fail(inputErr.status, errorBody(inputErr.message))
		return
	}
	switch {
	case errors.Is(err, team.ErrInputContentMismatch),
		errors.Is(err, team.ErrVersionConflict),
		errors.Is(err, team.ErrInputNotQueued):
		h.fail(http.StatusConflict, errorBody(err.Error()))
	case errors.Is(err, team.ErrQueueFull):
		h.fail(http.StatusTooManyRequests, errorBody(err.Error()))
	case errors.Is(err, team.ErrNotFound):
		h.fail(http.StatusNotFound, errorBody("输入不存在，请刷新后重试"))
	default:
		h.failTeamError(err)
	}
}

// handleTeamInputsList 是 GET /api/teams/:teamId/inputs?cursor=&limit=：按
// 接受序分页返回输入投影与队列容量概况。
func (h *handler) handleTeamInputsList(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	if h.server.TeamInputs == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用输入队列"))
		return
	}
	query := h.request.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			h.fail(http.StatusBadRequest, errorBody("limit 必须是正整数"))
			return
		}
		limit = parsed
	}
	rows, next := h.server.TeamInputs.ListInputs(teamID, query.Get("cursor"), limit)
	result := jsoncompat.NewObject()
	result.Set("teamId", teamID)
	result.Set("queueVersion", jsoncompat.Num(2))
	result.Set("pendingCount", jsoncompat.Num(h.server.TeamInputs.PendingCount(teamID)))
	result.Set("maxPendingInputs", jsoncompat.Num(teamRow.Policy.MaxPendingInputs))
	inputs := &jsoncompat.Array{}
	for _, input := range rows {
		inputs.Items = append(inputs.Items, h.teamInputProject(input))
	}
	result.Set("inputs", inputs)
	if next != "" {
		result.Set("nextCursor", next)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// handleTeamInputRevise 是 PATCH /api/teams/:teamId/inputs/:inputId：仅 queued
// 可编辑，产生新 revision；操作 UUID 幂等，版本冲突 409。
func (h *handler) handleTeamInputRevise(teamID, inputID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	if h.server.TeamInputs == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用输入队列"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	opID := stringField(body, "id")
	if opID == "" {
		h.fail(http.StatusBadRequest, errorBody("缺少操作标识，请刷新后重试"))
		return
	}
	version, hasVersion := bodyVersion(body)
	if !hasVersion {
		h.fail(http.StatusBadRequest, errorBody("缺少期望版本，请携带输入当前 version"))
		return
	}
	text := stringField(body, "text")
	attachments := submissionAttachmentIDs(body.MustGet("attachments"))
	if attachments == nil {
		// 未提供附件时保持当前附件（编辑正文不隐式清空附件）。
		if current := h.server.TeamInputs.Get(teamID, inputID); current != nil {
			attachments = current.AttachmentIDs
		}
	}
	threadID := teamLeadSessionID(teamRow)
	if _, validateErr := h.queueTeamInputValidate(threadID, text, attachments); validateErr != nil {
		var inputErr *teamInputError
		if errors.As(validateErr, &inputErr) {
			h.fail(inputErr.status, errorBody(inputErr.message))
		} else {
			h.fail(http.StatusBadRequest, errorBody(validateErr.Error()))
		}
		return
	}
	input, duplicate, reviseErr := h.server.TeamInputs.Revise(teamID, inputID, opID, strings.TrimSpace(text), attachments, version)
	if reviseErr != nil {
		h.failTeamInputError(reviseErr)
		return
	}
	result := h.teamInputProject(input)
	if duplicate {
		result.Set("duplicate", true)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// handleTeamInputPropose 接收 Lead 的结构化工作项提案；语义分类只是提案，
// 来源、依赖、容量与策略版本由服务端校验后才形成路由意图。
func (h *handler) handleTeamInputPropose(teamID, inputID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	if h.server.TeamInputs == nil || h.server.Teams == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用输入队列"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	opID := stringField(body, "id")
	if opID == "" {
		h.fail(http.StatusBadRequest, errorBody("缺少操作标识，请刷新后重试"))
		return
	}
	version, ok := bodyVersion(body)
	if !ok {
		h.fail(http.StatusBadRequest, errorBody("缺少期望版本，请携带输入当前 version"))
		return
	}
	raw, exists := body.Get("workItems")
	if !exists {
		h.fail(http.StatusBadRequest, errorBody("缺少工作项提案"))
		return
	}
	var items []team.WorkItem
	if err := json.Unmarshal(jsoncompat.EncodeCompact(raw), &items); err != nil {
		h.fail(http.StatusBadRequest, errorBody("工作项提案格式无效"))
		return
	}
	input, duplicate, err := h.server.Teams.ProposeInputRoute(teamID, inputID, opID, items, version)
	if err != nil {
		if errors.Is(err, team.ErrRouteProposal) {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
		} else {
			h.failTeamInputError(err)
		}
		return
	}
	result := h.teamInputProject(input)
	if duplicate {
		result.Set("duplicate", true)
	}
	h.output(http.StatusAccepted, result, contentTypeJSON, "")
}

// handleTeamInputPrioritize 是 POST .../inputs/:inputId/prioritize：「立即发送」
// 只提升路由顺序，不绕过容量、并发与写锁。
func (h *handler) handleTeamInputPrioritize(teamID, inputID string) {
	h.teamInputQueueOp(teamID, inputID, func(ledger *team.InputLedger, opID string, version uint64) (*team.TeamInput, bool, error) {
		return ledger.Prioritize(teamID, inputID, opID, version)
	})
}

// handleTeamInputCancel 是 POST .../inputs/:inputId/cancel：取消未派发部分；
// 已进入 routing 的部分按血缘修订处理（阶段 2），排队期直接取消。
func (h *handler) handleTeamInputCancel(teamID, inputID string) {
	h.teamInputQueueOp(teamID, inputID, func(ledger *team.InputLedger, opID string, version uint64) (*team.TeamInput, bool, error) {
		return ledger.Cancel(teamID, inputID, opID, version)
	})
}

// handleTeamInputRevokeRoute 撤销未落成的路由意图，允许人工确认后重新规划。
func (h *handler) handleTeamInputRevokeRoute(teamID, inputID string) {
	h.teamInputQueueOp(teamID, inputID, func(ledger *team.InputLedger, opID string, version uint64) (*team.TeamInput, bool, error) {
		return ledger.RevokeRoutingIntent(teamID, inputID, opID, version)
	})
}

// teamInputQueueOp 是 prioritize/cancel 的共用外壳：操作 UUID + 期望版本 →
// 账本幂等操作 → 投影回显。
func (h *handler) teamInputQueueOp(teamID, inputID string, op func(ledger *team.InputLedger, opID string, version uint64) (*team.TeamInput, bool, error)) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	if h.server.TeamInputs == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用输入队列"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	opID := stringField(body, "id")
	if opID == "" {
		h.fail(http.StatusBadRequest, errorBody("缺少操作标识，请刷新后重试"))
		return
	}
	version, hasVersion := bodyVersion(body)
	if !hasVersion {
		h.fail(http.StatusBadRequest, errorBody("缺少期望版本，请携带输入当前 version"))
		return
	}
	input, duplicate, opErr := op(h.server.TeamInputs, opID, version)
	if opErr != nil {
		h.failTeamInputError(opErr)
		return
	}
	result := h.teamInputProject(input)
	if duplicate {
		result.Set("duplicate", true)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// bodyVersion 读取请求体的期望版本字段（version，整数）。
func bodyVersion(body *jsoncompat.Object) (uint64, bool) {
	raw, exists := body.Get("version")
	if !exists {
		return 0, false
	}
	number, ok := raw.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 {
		return 0, false
	}
	return uint64(parsed), true
}

// submissionAttachmentIDs 从请求的 attachments 字段提取附件 ID 列表（对象
// 形 {id} 或旧字符串形；nil 表示字段缺失）。
func submissionAttachmentIDs(raw any) []string {
	array, ok := raw.(*jsoncompat.Array)
	if !ok {
		return nil
	}
	ids := []string{}
	for _, item := range array.Items {
		if entry, isObject := item.(*jsoncompat.Object); isObject {
			if id, isString := entry.MustGet("id").(string); isString && id != "" {
				ids = append(ids, id)
			}
		} else if id, isString := item.(string); isString && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
