package team

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const EventIssueClosed = "issue_closed"

// IssueGoal 返回事项级目标记录的深拷贝； nil 表示尚未写入新账本。
func (l *InputLedger) IssueGoal(teamID, issueID string) *GoalRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	issue := l.issues[issueID]
	if issue == nil || issue.TeamID != teamID {
		return nil
	}
	return cloneGoalRecord(l.issueGoals[issueID])
}

// ApplyIssueGoal 将 Run 已接受的目标操作同步到 Issue 账本。重试只追加同一操作，
// 已有更多操作的记录不会被旧请求覆盖。
func (l *InputLedger) ApplyIssueGoal(teamID, runID string, record *GoalRecord) error {
	if record == nil || len(record.Operations) == 0 {
		return fmt.Errorf("Issue 目标记录缺少操作标识")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	issueID := record.Scope.ID
	issue := l.issues[issueID]
	if issue == nil || issue.TeamID != teamID {
		return ErrNotFound
	}
	existing := l.issueGoals[issueID]
	if existing != nil {
		for id, previous := range existing.Operations {
			incoming, exists := record.Operations[id]
			if !exists {
				return nil
			}
			if previous != incoming {
				return fmt.Errorf("%w：同一目标操作标识不能用于不同内容", ErrInputContentMismatch)
			}
		}
	}
	operations := make([]string, 0, len(record.Operations))
	for id := range record.Operations {
		if existing == nil || existing.Operations[id] == "" {
			operations = append(operations, id)
		}
	}
	if len(operations) == 0 {
		return nil
	}
	sort.Strings(operations)
	operationID := operations[0]
	eventType := EventGoalReviewed
	if record.Closed {
		eventType = EventIssueClosed
	}
	event := newEvent("", eventType, "user", map[string]any{
		"teamId": teamID, "issueId": issueID, "runId": runID, "goal": goalPayload(record),
	})
	event.IdempotencyKey = "team:" + teamID + ":issue:" + issueID + ":goal-review:" + operationID
	return l.commitLocked(event, func() { l.applyInputEvent(event) })
}

func (l *InputLedger) applyIssueGoalEvent(event *Event) {
	teamID := payloadString(event.Payload, "teamId")
	issueID := payloadString(event.Payload, "issueId")
	issue := l.issues[issueID]
	if issue == nil || issue.TeamID != teamID {
		return
	}
	record := decodeGoalRecord(event.Payload["goal"])
	if record == nil || record.Scope.ID != issueID {
		return
	}
	if l.issueGoals == nil {
		l.issueGoals = map[string]*GoalRecord{}
	}
	l.issueGoals[issueID] = cloneGoalRecord(record)
	issue.UpdatedAt = event.CreatedAt
}

func issueGoalSnapshotValue(record *GoalRecord) (any, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return jsoncompat.Unmarshal(raw)
}
