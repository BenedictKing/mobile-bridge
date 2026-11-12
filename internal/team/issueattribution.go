package team

import "strings"

type TaskAttribution struct {
	TaskIDs       []string
	SharedTaskIDs []string
	Known         bool
}

func (a TaskAttribution) Contains(taskID string) bool {
	return containsString(a.TaskIDs, taskID)
}

func (a TaskAttribution) ContainsShared(taskID string) bool {
	return containsString(a.SharedTaskIDs, taskID)
}

func issueInputRunLinked(input *TeamInput, runID string) bool {
	if input == nil {
		return false
	}
	if input.TargetRunID == runID || containsString(input.RunIDs, runID) {
		return true
	}
	for _, item := range input.WorkItems {
		if item.RunID == runID {
			return true
		}
	}
	return false
}

func issueRangesOverlap(startA, endA, startB, endB int) bool {
	return startA < endB && startB < endA
}

func issueSingleLinkOwner(input *TeamInput) string {
	if len(input.IssueLinks) == 1 {
		return input.IssueLinks[0].IssueID
	}
	return ""
}

func addIssueRootOwners(roots map[string]map[string]bool, taskIDs []string, issueID string) {
	if issueID == "" {
		return
	}
	for _, taskID := range taskIDs {
		if taskID == "" {
			continue
		}
		if roots[taskID] == nil {
			roots[taskID] = map[string]bool{}
		}
		roots[taskID][issueID] = true
	}
}

// IssueTaskAttribution 从原话区间和派生血缘推导事项任务；跨事项与无血缘任务保持共享。
func IssueTaskAttribution(issue *Issue, run *Run, inputs []*TeamInput) TaskAttribution {
	var result TaskAttribution
	if issue == nil || run == nil {
		return result
	}
	rootOwners := map[string]map[string]bool{}
	for _, input := range inputs {
		if !issueInputRunLinked(input, run.ID) {
			continue
		}
		singleOwner := issueSingleLinkOwner(input)
		for _, item := range input.WorkItems {
			if item.RunID != run.ID {
				continue
			}
			for _, link := range input.IssueLinks {
				if link.Revision != input.Revision || item.SourceRevision != link.Revision {
					continue
				}
				if item.SourceEnd > item.SourceStart && issueRangesOverlap(link.SourceStart, link.SourceEnd, item.SourceStart, item.SourceEnd) {
					addIssueRootOwners(rootOwners, item.TaskIDs, link.IssueID)
				}
			}
			if item.SourceEnd <= item.SourceStart {
				addIssueRootOwners(rootOwners, item.TaskIDs, singleOwner)
			}
		}
	}

	included := map[string]bool{}
	for root, owners := range rootOwners {
		if owners[issue.ID] {
			included[root] = true
		}
	}
	result.Known = len(included) > 0
	for changed := true; changed; {
		changed = false
		for i := range run.Tasks {
			task := &run.Tasks[i]
			if included[task.ID] || task.Reduction {
				continue
			}
			descends := false
			for root := range included {
				if strings.HasPrefix(task.ID, root+"-") {
					descends = true
					break
				}
			}
			if descends {
				included[task.ID] = true
				changed = true
				continue
			}
			if len(task.ReviewOf) > 0 {
				allOwned := true
				for _, ref := range task.ReviewOf {
					if !included[ref] {
						allOwned = false
						break
					}
				}
				if allOwned {
					included[task.ID] = true
					changed = true
				}
			}
		}
	}

	lineageOwners := func(taskID string) map[string]bool {
		owners := map[string]bool{}
		for root, candidates := range rootOwners {
			if taskID == root || strings.HasPrefix(taskID, root+"-") {
				for owner := range candidates {
					owners[owner] = true
				}
			}
		}
		return owners
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if included[task.ID] {
			result.TaskIDs = append(result.TaskIDs, task.ID)
			continue
		}
		owners := lineageOwners(task.ID)
		if len(task.ReviewOf) > 0 {
			owners = map[string]bool{}
			for _, ref := range task.ReviewOf {
				for owner := range lineageOwners(ref) {
					owners[owner] = true
				}
			}
		}
		shared := task.Reduction || task.IssueClassificationOnly || task.ExecutionContext == ExecutionContextLeadControl || len(owners) == 0 || len(owners) > 1
		if shared {
			result.SharedTaskIDs = append(result.SharedTaskIDs, task.ID)
		}
	}
	return result
}

// RunForIssue 过滤执行证据，保留事项任务与共享任务，排除其他事项分支。
func RunForIssue(issue *Issue, run *Run, inputs []*TeamInput) (*Run, TaskAttribution) {
	attribution := IssueTaskAttribution(issue, run, inputs)
	scoped := run.Clone()
	scoped.Tasks = nil
	allowed := map[string]bool{}
	for _, taskID := range attribution.TaskIDs {
		allowed[taskID] = true
	}
	for _, taskID := range attribution.SharedTaskIDs {
		allowed[taskID] = true
	}
	for i := range run.Tasks {
		if allowed[run.Tasks[i].ID] {
			scoped.Tasks = append(scoped.Tasks, *run.Tasks[i].Clone())
		}
	}
	scoped.PendingActions = nil
	for _, action := range run.PendingActions {
		if action.SourceTaskID == "" || allowed[action.SourceTaskID] {
			scoped.PendingActions = append(scoped.PendingActions, action)
		}
	}
	scoped.PendingQuestions = nil
	for _, question := range run.PendingQuestions {
		if question.TaskID == "" || allowed[question.TaskID] {
			scoped.PendingQuestions = append(scoped.PendingQuestions, *question.Clone())
		}
	}
	return scoped, attribution
}
