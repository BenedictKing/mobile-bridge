package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
)

// ProposeRoute 原子持久化工作项提案与 planned 路由意图。
func (l *InputLedger) ProposeRoute(teamID, inputID, opID string, items []WorkItem, expectedVersion uint64) (*TeamInput, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if input, duplicate, err := l.replayOpLocked(teamID, inputID, opID); duplicate || err != nil {
		return input, duplicate, err
	}
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if input.QueueStatus != InputQueued && input.QueueStatus != InputWaitingInput || input.RouteStatus != "" {
		return nil, false, fmt.Errorf("%w：当前输入已有路由意图", ErrInputNotQueued)
	}
	if input.Version != expectedVersion {
		return nil, false, ErrVersionConflict
	}
	if len(items) == 0 || items[0].RunID == "" || len(items[0].TaskIDs) == 0 {
		return nil, false, errors.New("路由提案缺少工作项或目标")
	}
	proposal := newEvent("", EventWorkItemsProposed, "lead", map[string]any{
		"inputId": inputID, "teamId": teamID, "opId": opID,
		"workItems": workItemsPayload(items),
	})
	proposal.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":propose:" + opID
	intent := newEvent("", EventInputRouteIntent, "lead", map[string]any{
		"inputId": inputID, "teamId": teamID, "routeKind": "planned",
		"targetRunId": items[0].RunID, "targetTaskId": items[0].TaskIDs[0], "parentRunId": "",
	})
	intent.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":intent:planned:" + items[0].RunID
	if err := l.commitTwoLocked(proposal, intent, func() {
		l.applyInputEvent(proposal)
		l.applyInputEvent(intent)
	}); err != nil {
		return nil, false, err
	}
	return input.Clone(), false, nil
}

func workItemsPayload(items []WorkItem) []any {
	rows := make([]any, 0, len(items))
	for _, item := range items {
		raw, _ := json.Marshal(item)
		var value map[string]any
		if json.Unmarshal(raw, &value) == nil {
			rows = append(rows, value)
		}
	}
	return rows
}

func (l *InputLedger) applyWorkItemsProposed(event *Event) {
	inputID := payloadString(event.Payload, "inputId")
	input := l.inputs[inputID]
	if opID := payloadString(event.Payload, "opId"); opID != "" {
		l.ops[opID] = inputID
	}
	if input == nil || len(input.WorkItems) > 0 {
		return
	}
	raw, err := json.Marshal(event.Payload["workItems"])
	if err != nil {
		return
	}
	var items []WorkItem
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return
	}
	input.WorkItems = (&TeamInput{WorkItems: items}).Clone().WorkItems
	for _, item := range items {
		input.WorkItemIDs = append(input.WorkItemIDs, item.ID)
	}
	input.Version++
	input.UpdatedAt = event.CreatedAt
}

var ErrRouteProposal = errors.New("路由提案未通过校验")

// ProposeInputRoute 校验 Lead 的结构化提案并转入持久路由。来源区间使用
// rune 偏移，所有非空白原文必须且只能归属一个工作项，防止静默漏项/重复。
func (s *Service) ProposeInputRoute(teamID, inputID, opID string, items []WorkItem, expectedVersion uint64) (*TeamInput, bool, error) {
	team := s.store.Team(teamID)
	input := s.inputs.Get(teamID, inputID)
	if team == nil || input == nil {
		return nil, false, ErrNotFound
	}
	if (input.QueueStatus == InputQueued || input.QueueStatus == InputWaitingInput) &&
		input.PolicyDigest != PolicyDigest(team.Policy) {
		return nil, false, fmt.Errorf("%w: 团队策略已变化，请刷新后重新提案", ErrVersionConflict)
	}
	validated, err := validateRouteProposal(team, input, items)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrRouteProposal, err)
	}
	result, duplicate, err := s.inputs.ProposeRoute(teamID, inputID, opID, validated, expectedVersion)
	if err == nil && !duplicate {
		s.PumpRouting()
	}
	return result, duplicate, err
}

func validateRouteProposal(team *Team, input *TeamInput, items []WorkItem) ([]WorkItem, error) {
	if team == nil || input == nil || len(items) == 0 || len(items) > 64 {
		return nil, errors.New("路由提案必须包含 1–64 个工作项")
	}
	runes := []rune(input.Text)
	covered := make([]bool, len(runes))
	ids := map[string]bool{}
	reviewer := team.ReviewerMemberID()
	validated := make([]WorkItem, len(items))
	for index, item := range items {
		if !ValidMemberID(item.ID) || ids[item.ID] {
			return nil, fmt.Errorf("工作项 ID 无效或重复: %s", item.ID)
		}
		ids[item.ID] = true
		if item.SourceStart < 0 || item.SourceEnd <= item.SourceStart || item.SourceEnd > len(runes) ||
			item.RequirementText != string(runes[item.SourceStart:item.SourceEnd]) {
			return nil, fmt.Errorf("工作项 %s 的来源区间与原文不一致", item.ID)
		}
		for position := item.SourceStart; position < item.SourceEnd; position++ {
			if covered[position] {
				return nil, fmt.Errorf("工作项 %s 与其他工作项重复覆盖原文", item.ID)
			}
			covered[position] = true
		}
		member := memberOf(team, item.MemberID)
		if member == nil || !member.Enabled {
			return nil, fmt.Errorf("工作项 %s 的成员不存在或已停用", item.ID)
		}
		switch item.ReviewPolicy {
		case "", "off", "best_effort", "required":
		default:
			return nil, fmt.Errorf("工作项 %s 的审查策略无效", item.ID)
		}
		if item.ReviewPolicy == "required" && (reviewer == "" || reviewer == item.MemberID) {
			return nil, fmt.Errorf("工作项 %s 需要独立评审成员", item.ID)
		}
		for _, dependency := range item.DependsOnItemIDs {
			if !ids[dependency] {
				return nil, fmt.Errorf("工作项 %s 的依赖必须引用前面的工作项", item.ID)
			}
		}
		item.SourceInputID = input.ID
		item.SourceRevision = input.Revision
		item.Status = "proposed"
		item.TaskIDs = []string{item.ID}
		if item.ReviewPolicy == "" {
			item.ReviewPolicy = "off"
		}
		validated[index] = item
	}
	for position, ok := range covered {
		if !ok && !unicode.IsSpace(runes[position]) {
			return nil, fmt.Errorf("路由提案未覆盖原文位置 %d，请补充或澄清该要求", position)
		}
	}

	capacity := policyOf(team).MaxTasksPerRun
	batch, used := 0, 0
	batchOf := map[string]int{}
	for index := range validated {
		item := &validated[index]
		needsReview := item.ReviewPolicy == "required" || item.ReviewPolicy == "best_effort" && reviewer != "" && reviewer != item.MemberID
		size := 1
		if needsReview {
			size++
		}
		if used > 0 && used+size > capacity {
			if len(item.DependsOnItemIDs) > 0 {
				return nil, fmt.Errorf("工作项 %s 的依赖不能跨 Run 抢跑；请缩小批次或调整容量", item.ID)
			}
			batch++
			used = 0
		}
		if batch == 0 {
			item.RunID = input.ID
		} else {
			item.RunID = fmt.Sprintf("%s-batch-%d", input.ID, batch+1)
		}
		batchOf[item.ID] = batch
		for _, dependency := range item.DependsOnItemIDs {
			if batchOf[dependency] != batch {
				return nil, fmt.Errorf("工作项 %s 的依赖不能跨 Run 抢跑", item.ID)
			}
		}
		if needsReview {
			item.TaskIDs = append(item.TaskIDs, "review-"+item.ID)
		}
		used += size
	}
	return validated, nil
}

func plannedRouteBatches(input *TeamInput) [][]WorkItem {
	if input == nil {
		return nil
	}
	byRun := map[string]int{}
	batches := [][]WorkItem{}
	for _, item := range input.WorkItems {
		index, ok := byRun[item.RunID]
		if !ok {
			index = len(batches)
			byRun[item.RunID] = index
			batches = append(batches, []WorkItem{})
		}
		batches[index] = append(batches[index], item)
	}
	return batches
}

func plannedRunTasks(team *Team, items []WorkItem) []PlanTaskInput {
	reviewer := team.ReviewerMemberID()
	plan := []PlanTaskInput{}
	for _, item := range items {
		depends := []string{}
		for _, dependency := range item.DependsOnItemIDs {
			for _, other := range items {
				if other.ID == dependency {
					depends = append(depends, other.TaskIDs[0])
				}
			}
		}
		plan = append(plan, PlanTaskInput{ID: item.ID, MemberID: item.MemberID,
			Prompt: item.RequirementText, DependsOn: depends, Review: reviewFlag(false)})
		if len(item.TaskIDs) > 1 {
			plan = append(plan, PlanTaskInput{ID: item.TaskIDs[1], MemberID: reviewer,
				Prompt: "审查工作项 " + item.ID + " 的当前产物，只读核对并给出明确裁决。",
				Review: reviewFlag(true), ReviewOf: []string{item.TaskIDs[0]}, WriteLock: TaskWriteLockNone})
		}
	}
	return plan
}

func (s *Service) materializePlannedRoute(team *Team, input *TeamInput) error {
	for _, items := range plannedRouteBatches(input) {
		runID := items[0].RunID
		plan := plannedRunTasks(team, items)
		if existing := s.store.Run(runID); existing != nil {
			if existing.TeamID != team.ID || existing.Goal != input.Text || existing.Mode != ModePipeline || len(existing.Tasks) != len(plan) {
				return errors.New("planned 路由目标已存在但计划不一致")
			}
			for _, expected := range plan {
				actual := findTaskIn(existing, expected.ID)
				if actual == nil || actual.MemberID != expected.MemberID || actual.Prompt != expected.Prompt {
					return errors.New("planned 路由目标任务不一致")
				}
			}
			continue
		}
		if _, err := s.CreateRun(team, runID, input.Text, ModePipeline, "", nil, plan, false, ""); err != nil {
			return err
		}
	}
	return nil
}

func plannedRouteTargets(input *TeamInput) []struct{ runID, taskID string } {
	targets := []struct{ runID, taskID string }{}
	seen := map[string]bool{}
	for _, item := range input.WorkItems {
		if !seen[item.RunID] && len(item.TaskIDs) > 0 {
			seen[item.RunID] = true
			targets = append(targets, struct{ runID, taskID string }{item.RunID, item.TaskIDs[0]})
		}
	}
	return targets
}
