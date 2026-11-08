package httpd

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

func teamTaskSummaryProject(row team.TaskSummary) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("taskId", row.TaskID)
	object.Set("runId", row.RunID)
	if row.WorkItemID != "" {
		object.Set("workItemId", row.WorkItemID)
	}
	object.Set("memberId", row.MemberID)
	object.Set("status", row.Status)
	object.Set("runStatus", row.RunStatus)
	if row.WaitingReason != "" {
		object.Set("waitingReason", row.WaitingReason)
	}
	if row.Attempt > 0 {
		object.Set("attempt", jsoncompat.Num(row.Attempt))
	}
	object.Set("latestEventSequence", jsoncompat.Num(int(row.LatestEventSequence)))
	return object
}

func teamTaskNotificationProject(row team.TaskNotification) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("eventId", row.EventID)
	object.Set("sequence", jsoncompat.Num(int(row.Sequence)))
	object.Set("runId", row.RunID)
	if row.TaskID != "" {
		object.Set("taskId", row.TaskID)
	}
	if row.WorkItemID != "" {
		object.Set("workItemId", row.WorkItemID)
	}
	object.Set("type", row.Type)
	object.Set("status", row.Status)
	if row.Reason != "" {
		object.Set("reason", row.Reason)
	}
	return object
}

// handleTeamTaskList 是主控的 list/check 只读面，默认只返回在途任务。
func (h *handler) handleTeamTaskList(teamID string) {
	if h.loadTeam(teamID) == nil {
		return
	}
	rows, err := h.server.Teams.ListTasks(teamID, h.request.URL.Query().Get("all") != "1")
	if err != nil {
		h.failTeamError(err)
		return
	}
	result := jsoncompat.NewObject()
	result.Set("teamId", teamID)
	tasks := &jsoncompat.Array{}
	for _, row := range rows {
		tasks.Items = append(tasks.Items, teamTaskSummaryProject(row))
	}
	result.Set("tasks", tasks)
	result.Set("count", jsoncompat.Num(len(rows)))
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) handleTeamTaskCheck(teamID, runID, taskID string) {
	if h.loadRun(teamID, runID) == nil {
		return
	}
	row, err := h.server.Teams.CheckTask(teamID, runID, taskID)
	if err != nil {
		h.failTeamError(err)
		return
	}
	h.output(http.StatusOK, teamTaskSummaryProject(*row), contentTypeJSON, "")
}

// handleTeamTaskEvents 按持久 journal 序号补读主控通知；重启无需重置游标。
func (h *handler) handleTeamTaskEvents(teamID string) {
	if h.loadTeam(teamID) == nil {
		return
	}
	query := h.request.URL.Query()
	after := uint64(0)
	var err error
	if raw := query.Get("after"); raw != "" {
		after, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody("after 必须是非负事件序号"))
			return
		}
	}
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			h.fail(http.StatusBadRequest, errorBody("limit 必须是正整数"))
			return
		}
	}
	rows, cursor, err := h.server.Teams.TaskNotifications(teamID, after, limit)
	if err != nil {
		h.failTeamError(err)
		return
	}
	result := jsoncompat.NewObject()
	result.Set("teamId", teamID)
	events := &jsoncompat.Array{}
	for _, row := range rows {
		events.Items = append(events.Items, teamTaskNotificationProject(row))
	}
	result.Set("events", events)
	result.Set("cursor", strconv.FormatUint(cursor, 10))
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) handleTeamTaskUpdate(teamID, runID, taskID string) {
	if h.loadRun(teamID, runID) == nil {
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	opID := stringField(body, "id")
	if _, err := uuid.Parse(opID); err != nil {
		h.fail(http.StatusBadRequest, errorBody("更新操作标识无效"))
		return
	}
	version, ok := bodyVersion(body)
	if !ok {
		h.fail(http.StatusBadRequest, errorBody("缺少期望 Run 版本"))
		return
	}
	strategy := stringField(body, "strategy")
	if strategy != "enqueue" && strategy != "interrupt" {
		h.fail(http.StatusBadRequest, errorBody("任务更新策略必须是 enqueue 或 interrupt"))
		return
	}
	text := strings.TrimSpace(stringField(body, "text"))
	if text == "" || len([]rune(text)) > teamGoalMaxRunes {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–10000 字的更新内容"))
		return
	}
	updated, err := h.server.Teams.UpdateTask(teamID, runID, taskID, opID, strategy, text, version)
	if err != nil {
		switch {
		case errors.Is(err, team.ErrTaskUpdateConflict):
			h.fail(http.StatusConflict, errorBody(err.Error()))
		default:
			h.failTeamError(err)
		}
		return
	}
	result := jsoncompat.NewObject()
	result.Set("status", updated.Status)
	result.Set("updateStatus", updated.Task.UpdateStatus)
	result.Set("strategy", strategy)
	if updated.Duplicate {
		result.Set("duplicate", true)
	}
	raw, err := updated.Task.MarshalJSON()
	if err != nil {
		h.fail(http.StatusInternalServerError, errorBody("服务内部错误"))
		return
	}
	taskValue, _ := jsoncompat.Unmarshal(raw)
	result.Set("task", taskValue)
	status := http.StatusAccepted
	if updated.Duplicate {
		status = http.StatusOK
	}
	h.output(status, result, contentTypeJSON, "")
}
