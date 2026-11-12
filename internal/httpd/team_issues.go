package httpd

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

type historicalTeamMessage struct {
	runID, taskID, inputID, text string
	createdAt                    time.Time
}

func (h *handler) handleTeamIssueBackfill(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	if h.server.TeamInputs == nil || h.server.Teams == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用团队历史归类"))
		return
	}
	runs := h.server.TeamStore.RunsByTeam(teamID)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.Before(runs[j].CreatedAt) })
	taskTimes := map[string]time.Time{}
	if h.server.TeamJournal != nil {
		if events, err := h.server.TeamJournal.Replay(); err == nil {
			for _, event := range events {
				if event.Type == team.EventTaskPlanned {
					if taskID, _ := event.Payload["taskId"].(string); taskID != "" {
						taskTimes[event.RunID+"\x00"+taskID] = event.CreatedAt
					}
				}
			}
		} else {
			log.Printf("team %s 历史归类无法读取 Run journal 时间戳: %v", teamID, err)
		}
	}
	messages := []historicalTeamMessage{}
	for _, run := range runs {
		if run.IsIssueClassificationOnly() || !run.AutoPlan || run.Mode != team.ModeSingle || run.ScheduledFrom != "" {
			continue
		}
		if h.server.TeamInputs.Get(teamID, run.ID) == nil {
			createdAt := taskTimes[run.ID+"\x00lead"]
			if createdAt.IsZero() {
				createdAt = run.CreatedAt
			}
			messages = append(messages, historicalTeamMessage{
				runID: run.ID, taskID: "lead", inputID: team.HistoricalInputID(teamID, run.ID, "lead"),
				text: run.Goal, createdAt: createdAt,
			})
		}
		for _, task := range run.Tasks {
			if !strings.HasPrefix(task.ID, "input-") || task.IssueClassificationOnly {
				continue
			}
			sourceID := strings.TrimPrefix(task.ID, "input-")
			parsedSourceID, parseErr := uuid.Parse(sourceID)
			if parseErr != nil || parsedSourceID.String() != sourceID {
				continue
			}
			if h.server.TeamInputs.Get(teamID, sourceID) != nil {
				continue
			}
			createdAt := taskTimes[run.ID+"\x00"+task.ID]
			if createdAt.IsZero() {
				createdAt = run.CreatedAt
			}
			messages = append(messages, historicalTeamMessage{
				runID: run.ID, taskID: task.ID, inputID: team.HistoricalInputID(teamID, run.ID, task.ID),
				text: task.Prompt, createdAt: createdAt,
			})
		}
	}

	imported, started, pending, completed, failed := 0, 0, 0, 0, 0
	for _, message := range messages {
		input, duplicate, err := h.server.TeamInputs.ImportHistoricalInput(teamID, message.inputID, message.text, message.runID, message.taskID, message.createdAt)
		if err != nil {
			failed++
			log.Printf("team %s 历史输入 %s 导入失败: %v", teamID, message.inputID, err)
			continue
		}
		if !duplicate {
			imported++
		}
		if input == nil {
			continue
		} // 已删除的历史气泡不重新导入或归类。
		switch input.AssociationStatus {
		case team.IssueAssociationLinked, team.IssueAssociationClarification, team.IssueAssociationIgnored:
			completed++
			continue
		case team.IssueAssociationFailed:
			failed++
			continue
		case team.IssueAssociationPending:
		default:
			failed++
			continue
		}
		if classifier := h.server.TeamStore.Run(input.ID); classifier != nil {
			if classifier.IsIssueClassificationOnly() && !teamRunTerminal(classifier.Status) &&
				classifier.Status != team.RunWaitingInput && classifier.Status != team.RunPaused {
				pending++
			} else {
				reason := classifier.StopReason
				for _, task := range classifier.Tasks {
					if task.IssueClassificationOnly && (task.Error != "" || task.StopReason != "") {
						reason = strings.TrimSpace(task.Error + " " + task.StopReason)
						break
					}
				}
				if classifier.Status == team.RunWaitingInput || classifier.Status == team.RunPaused {
					reason = "歷史歸類任務停在等待狀態，請檢查負責會話後重新整理對話"
					_, _ = h.server.Teams.Cancel(classifier.ID)
				}
				if reason == "" {
					reason = "歷史歸類任務已結束，但沒有提交有效的 Issue 提案"
				}
				_, _, _ = h.server.TeamInputs.FailIssueAssociation(teamID, input.ID, reason)
				failed++
			}
			continue
		}
		if _, err := h.server.Teams.CreateIssueClassificationRun(teamRow, input.ID, input.Text); err != nil {
			failed++
			_, _, _ = h.server.TeamInputs.FailIssueAssociation(teamID, input.ID, err.Error())
			log.Printf("team %s 历史输入 %s 归类任务创建失败: %v", teamID, input.ID, err)
			continue
		}
		started++
		pending++
	}
	result := jsoncompat.NewObject()
	result.Set("status", "accepted")
	result.Set("teamId", teamID)
	result.Set("discovered", jsoncompat.Num(len(messages)))
	result.Set("imported", jsoncompat.Num(imported))
	result.Set("started", jsoncompat.Num(started))
	result.Set("pending", jsoncompat.Num(pending))
	result.Set("completed", jsoncompat.Num(completed))
	result.Set("failed", jsoncompat.Num(failed))
	h.output(http.StatusAccepted, result, contentTypeJSON, "")
}

func (h *handler) handleTeamIssuesList(teamID string) {
	if h.loadTeam(teamID) == nil {
		return
	}
	if h.server.TeamInputs == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用团队 Issue 账本"))
		return
	}
	rows := &jsoncompat.Array{}
	for _, issue := range h.server.TeamInputs.ListIssues(teamID) {
		rows.Items = append(rows.Items, h.teamIssueProject(issue, false))
	}
	result := jsoncompat.NewObject()
	result.Set("teamId", teamID)
	result.Set("issues", rows)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) handleTeamIssueGet(teamID, issueID string) {
	if h.loadTeam(teamID) == nil {
		return
	}
	if h.server.TeamInputs == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用团队 Issue 账本"))
		return
	}
	issue := h.server.TeamInputs.GetIssue(teamID, issueID)
	if issue == nil {
		h.fail(http.StatusNotFound, errorBody("Issue 不存在"))
		return
	}
	h.output(http.StatusOK, h.teamIssueProject(issue, true), contentTypeJSON, "")
}

func (h *handler) teamIssueRunIDs(issue *team.Issue) map[string]bool {
	runIDs := map[string]bool{}
	for _, link := range issue.Links {
		input := h.server.TeamInputs.Get(issue.TeamID, link.InputID)
		if input == nil {
			continue
		}
		for _, runID := range append(append([]string{}, input.RunIDs...), input.TargetRunID) {
			if runID != "" {
				runIDs[runID] = true
			}
		}
		for _, workItem := range input.WorkItems {
			if workItem.RunID != "" {
				runIDs[workItem.RunID] = true
			}
		}
	}
	return runIDs
}

func (h *handler) handleTeamIssueDelete(teamID, issueID string) {
	if h.loadTeam(teamID) == nil {
		return
	}
	if h.server.TeamInputs == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用团队 Issue 账本"))
		return
	}
	issue := h.server.TeamInputs.GetIssue(teamID, issueID)
	if issue == nil {
		h.fail(http.StatusNotFound, errorBody("Issue 不存在"))
		return
	}
	runIDs := h.teamIssueRunIDs(issue)
	for _, link := range issue.Links {
		input := h.server.TeamInputs.Get(teamID, link.InputID)
		if input == nil {
			continue
		}
		if classifier := h.server.TeamStore.Run(input.ID); classifier != nil && classifier.IsIssueClassificationOnly() && !teamRunTerminal(classifier.Status) {
			h.fail(http.StatusConflict, errorBody("事项仍在归类，请等待结束后再删除"))
			return
		}
	}
	shared := map[string]*team.Issue{}
	for _, other := range h.server.TeamInputs.ListIssues(teamID) {
		if other.ID == issueID {
			continue
		}
		otherRunIDs := h.teamIssueRunIDs(other)
		for runID := range runIDs {
			if shared[other.ID] != nil || !otherRunIDs[runID] {
				continue
			}
			if run := h.server.TeamStore.Run(runID); run != nil && !teamRunTerminal(run.Status) {
				shared[other.ID] = other
			}
		}
	}
	if len(shared) > 0 {
		ids := make([]string, 0, len(shared))
		for id := range shared {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		titles := make([]string, 0, len(ids))
		rows := &jsoncompat.Array{}
		for _, id := range ids {
			titles = append(titles, shared[id].Title)
			row := jsoncompat.NewObject()
			row.Set("id", id)
			row.Set("title", shared[id].Title)
			rows.Items = append(rows.Items, row)
		}
		body := errorBody("事项与其他事项共享未结束的 Run，请等待执行结束后再删除：" + strings.Join(titles, "、"))
		body.Set("sharedIssues", rows)
		h.fail(http.StatusConflict, body)
		return
	}
	for runID := range runIDs {
		if run := h.server.TeamStore.Run(runID); run != nil && !teamRunTerminal(run.Status) {
			h.fail(http.StatusConflict, errorBody("事项仍有未结束的 Run，请先取消或等待结束"))
			return
		}
	}
	deleted, err := h.server.TeamInputs.DeleteIssue(teamID, issueID)
	if err != nil {
		h.failTeamInputError(err)
		return
	}
	result := jsoncompat.NewObject()
	result.Set("ok", true)
	result.Set("deletedInputs", jsoncompat.Num(deleted))
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) teamIssueProject(issue *team.Issue, includeDelivery bool) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("id", issue.ID)
	result.Set("teamId", issue.TeamID)
	result.Set("title", issue.Title)
	result.Set("createdAt", issue.CreatedAt.Format(time.RFC3339Nano))
	result.Set("updatedAt", issue.UpdatedAt.Format(time.RFC3339Nano))

	teamRow := h.server.TeamStore.Team(issue.TeamID)
	inputRows := &jsoncompat.Array{}
	hasCurrentInput := false
	runIDs := []string{}
	runIDSet := map[string]bool{}
	clarification := false
	for _, link := range issue.Links {
		input := h.server.TeamInputs.Get(issue.TeamID, link.InputID)
		if input == nil || input.Revision != link.Revision {
			continue
		}
		if !input.Historical {
			hasCurrentInput = true
		}
		if input.AssociationStatus == team.IssueAssociationClarification {
			clarification = true
		}
		row := jsoncompat.NewObject()
		row.Set("inputId", link.InputID)
		row.Set("revision", jsoncompat.Num(link.Revision))
		row.Set("sourceStart", jsoncompat.Num(link.SourceStart))
		row.Set("sourceEnd", jsoncompat.Num(link.SourceEnd))
		row.Set("sourceText", link.SourceText)
		row.Set("text", input.Text)
		row.Set("acceptedAt", input.AcceptedAt.Format(time.RFC3339Nano))
		row.Set("associationStatus", input.AssociationStatus)
		inputRows.Items = append(inputRows.Items, row)
		for _, runID := range input.RunIDs {
			if runID != "" && !runIDSet[runID] {
				runIDSet[runID] = true
				runIDs = append(runIDs, runID)
			}
		}
		if input.TargetRunID != "" && !runIDSet[input.TargetRunID] {
			runIDSet[input.TargetRunID] = true
			runIDs = append(runIDs, input.TargetRunID)
		}
		for _, workItem := range input.WorkItems {
			if workItem.RunID != "" && !runIDSet[workItem.RunID] {
				runIDSet[workItem.RunID] = true
				runIDs = append(runIDs, workItem.RunID)
			}
		}
	}
	result.Set("inputs", inputRows)
	result.Set("historical", len(inputRows.Items) > 0 && !hasCurrentInput)
	runIDRows := &jsoncompat.Array{}
	for _, id := range runIDs {
		runIDRows.Items = append(runIDRows.Items, id)
	}
	result.Set("runIds", runIDRows)

	runsByID := map[string]*team.Run{}
	for _, run := range h.server.TeamStore.RunsByTeam(issue.TeamID) {
		runsByID[run.ID] = run
	}
	linkedRuns := make([]*team.Run, 0, len(runIDs))
	for _, id := range runIDs {
		if run := runsByID[id]; run != nil {
			linkedRuns = append(linkedRuns, run)
		}
	}
	sort.SliceStable(linkedRuns, func(i, j int) bool {
		return linkedRuns[i].CreatedAt.After(linkedRuns[j].CreatedAt)
	})

	runs := &jsoncompat.Array{}
	counts := map[string]int{}
	attribution := team.TaskAttribution{Known: true}
	for index, run := range linkedRuns {
		summary := teamRunSummary(teamRow, run, h.server.TeamFeeds.Peek(run.ID))
		h.decorateRunGoals(run, summary, false)
		if includeDelivery {
			summary.Set("delivery", teamDeliveryEvidence(teamRow, run))
		}
		runs.Items = append(runs.Items, summary)
		if index == 0 {
			attribution = team.IssueTaskAttribution(issue, run, h.server.TeamInputs.ListAll(issue.TeamID))
			for _, task := range run.Tasks {
				if attribution.Contains(task.ID) {
					counts[task.Status]++
				}
			}
		}
	}
	result.Set("runs", runs)
	result.Set("sharedTaskCount", jsoncompat.Num(len(attribution.SharedTaskIDs)))
	taskCounts := jsoncompat.NewObject()
	total, completed := 0, counts[team.TaskCompleted]
	for status, count := range counts {
		total += count
		taskCounts.Set(status, jsoncompat.Num(count))
	}
	progress := jsoncompat.NewObject()
	progress.Set("total", jsoncompat.Num(total))
	progress.Set("completed", jsoncompat.Num(completed))
	progress.Set("known", attribution.Known)
	result.Set("taskCounts", taskCounts)
	result.Set("progress", progress)

	result.Set("status", "open")
	// 事项的解决状态取决于当前目标与最新一轮的有效判定，历史失败不永久拖住修订。
	if len(linkedRuns) > 0 {
		latestRun := linkedRuns[0]
		outcome := h.goalOutcome(latestRun, h.issueGoalScope(issue))
		result.Set("outcome", goalObject(outcome))
		result.Set("executionStatus", latestRun.Status)
		result.Set("reviewRunId", latestRun.ID)
		result.Set("reviewRunVersion", jsoncompat.Num(int(latestRun.Version)))
		businessStatus := "open"
		switch {
		case outcome.Closed:
			businessStatus = "closed"
		case clarification:
			businessStatus = "waiting"
		case latestRun.Status == team.RunWaitingInput || latestRun.Status == team.RunPaused || latestRun.Status == team.RunOutcomeUnknown || latestRun.Status == team.RunFailed:
			businessStatus = "waiting"
		case !team.RunTerminal(latestRun.Status):
			businessStatus = "in_progress"
		case outcome.Status == "satisfied":
			businessStatus = "resolved"
		case latestRun.Status == team.RunCompleted:
			businessStatus = "waiting"
		}
		result.Set("status", businessStatus)
	}
	if len(linkedRuns) > 0 {
		latest := teamRunSummary(teamRow, linkedRuns[0], h.server.TeamFeeds.Peek(linkedRuns[0].ID))
		if owner, exists := latest.Get("currentOwner"); exists {
			result.Set("currentOwner", owner)
		} else if owner, exists := latest.Get("lastOwner"); exists {
			result.Set("lastOwner", owner)
		}
		if update, exists := latest.Get("lastUpdate"); exists {
			result.Set("lastUpdate", update)
		}
		if next, exists := latest.Get("nextAction"); exists {
			result.Set("nextAction", next)
		}
		if linkedRuns[0].Status == team.RunCompleted {
			result.Set("nextAction", goalNextAction(h.goalOutcome(linkedRuns[0], h.issueGoalScope(issue))))
		}
	}
	return result
}
