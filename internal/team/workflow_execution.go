package team

import "github.com/google/uuid"

func taskLockKey(runID, taskID string) string {
	return runID + "\x00" + taskID
}

func workflowThreadID(runID, memberID, workflowID string) string {
	return "th_workflow_" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(runID+":"+memberID+":"+workflowID)).String()
}

// workflow 的并发租约按独立执行线程计算，旧任务仍按成员互斥。
func taskLeaseKey(task *Task) string {
	if task == nil {
		return ""
	}
	if task.WorkflowID != "" {
		return task.MemberID + ":" + executionThreadIDForTask(task)
	}
	return task.MemberID
}

func actionLeaseKey(run *Run, action *Action) string {
	if action.WorkflowID == "" {
		return action.Member
	}
	return action.Member + ":" + workflowThreadID(run.ID, action.Member, action.WorkflowID)
}

func addWorkflowTaskPayload(payload map[string]any, task *Task) {
	if task.WorkflowID == "" && !task.GoalFeedback {
		return
	}
	payload["workflowId"] = task.WorkflowID
	payload["goalFeedback"] = task.GoalFeedback
	payload["reduction"] = task.Reduction
	payload["executionThreadId"] = task.ExecutionThreadID
	payload["workspaceId"] = task.WorkspaceID
	payload["workspaceMode"] = task.WorkspaceMode
	payload["writeLock"] = task.WriteLock
	payload["prompt"] = task.Prompt
	payload["dependsOn"] = task.DependsOn
}
