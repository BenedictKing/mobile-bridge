package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	issueBlockOpen  = "<team-issues>"
	issueBlockClose = "</team-issues>"

	IssueAssociationPending       = "pending"
	IssueAssociationLinked        = "linked"
	IssueAssociationClarification = "clarification"
	IssueAssociationIgnored       = "ignored"
	IssueAssociationFailed        = "failed"
	IssueAssociationUnclassified  = "unclassified"
	IssueSegmentCreate            = "create"
	IssueSegmentLink              = "link"
	IssueSegmentIgnore            = "ignore"
	IssueSegmentClarify           = "clarify"
	maxIssueProposalSegments      = 64
	maxIssueTitleRunes            = 120
	maxIssueClarificationRunes    = 500
)

// Issue 是稳定的用户可见主题；执行状态由关联的 Run/Task 记录提供。
type Issue struct {
	ID        string           `json:"id"`
	TeamID    string           `json:"teamId"`
	Title     string           `json:"title"`
	Links     []IssueInputLink `json:"links,omitempty"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// IssueInputLink 保存被归类到 Issue 的精确原文区间。
type IssueInputLink struct {
	IssueID     string    `json:"issueId"`
	InputID     string    `json:"inputId"`
	Revision    int       `json:"revision"`
	SourceStart int       `json:"sourceStart"`
	SourceEnd   int       `json:"sourceEnd"`
	SourceText  string    `json:"sourceText"`
	LinkedAt    time.Time `json:"linkedAt"`
}

// IssueProposalSegment 是已受理消息中的一个精确区间；Action 决定该区间的处理方式。
type IssueProposalSegment struct {
	Action        string `json:"action"`
	IssueID       string `json:"issueId,omitempty"`
	NewIssueTitle string `json:"newIssueTitle,omitempty"`
	SourceStart   int    `json:"sourceStart"`
	SourceEnd     int    `json:"sourceEnd"`
	Reason        string `json:"reason,omitempty"`
}

type IssueProposal struct {
	InputID  string                 `json:"inputId"`
	Revision int                    `json:"revision"`
	Segments []IssueProposalSegment `json:"segments"`
}

type issueProposalEvent struct {
	InputID           string                 `json:"inputId"`
	TeamID            string                 `json:"teamId"`
	Revision          int                    `json:"revision"`
	ProposalID        string                 `json:"proposalId"`
	AssociationStatus string                 `json:"associationStatus"`
	AssociationReason string                 `json:"associationReason,omitempty"`
	Segments          []IssueProposalSegment `json:"segments"`
	CreatedIssues     []Issue                `json:"createdIssues,omitempty"`
}

// ExtractIssueProposal 要求恰好一个独立的机器可读块；同一条 Lead 回复可同时包含 team-actions 块。
func ExtractIssueProposal(fullText string) (*IssueProposal, []string) {
	openAt := strings.Index(fullText, issueBlockOpen)
	closeAt := strings.Index(fullText, issueBlockClose)
	if openAt < 0 && closeAt < 0 {
		return nil, []string{"缺少 team-issues 关联提案块"}
	}
	if openAt < 0 || closeAt < openAt {
		return nil, []string{"team-issues 开始与结束标记必须成对出现"}
	}
	if strings.Contains(fullText[openAt+len(issueBlockOpen):], issueBlockOpen) ||
		strings.Contains(fullText[closeAt+len(issueBlockClose):], issueBlockClose) ||
		strings.Contains(fullText[closeAt+len(issueBlockClose):], issueBlockOpen) {
		return nil, []string{"只能输出一个 team-issues 关联提案块"}
	}
	inner := strings.TrimSpace(fullText[openAt+len(issueBlockOpen) : closeAt])
	if inner == "" {
		return nil, []string{"team-issues 提案不能为空"}
	}
	var proposal IssueProposal
	if err := json.Unmarshal([]byte(inner), &proposal); err != nil {
		return nil, []string{fmt.Sprintf("team-issues 提案不是合法 JSON：%v", err)}
	}
	return &proposal, nil
}

func issueStableID(teamID, inputID string, revision int, title string) string {
	seed := fmt.Sprintf("mobile-bridge:issue:%s:%s:%d:%s", teamID, inputID, revision, normalizeIssueTitle(title))
	sum := sha256.Sum256([]byte(seed))
	return "issue-" + hex.EncodeToString(sum[:12])
}

func HistoricalInputID(teamID, sourceRunID, sourceTaskID string) string {
	seed := fmt.Sprintf("mobile-bridge:history-input:%s:%s:%s", teamID, sourceRunID, sourceTaskID)
	sum := sha256.Sum256([]byte(seed))
	return "history-" + hex.EncodeToString(sum[:12])
}

func normalizeIssueTitle(value string) string { return strings.Join(strings.Fields(value), " ") }

func validateIssueProposal(proposal *IssueProposal, input *TeamInput, teamID string, issues map[string]*Issue) ([]IssueProposalSegment, string, error) {
	if proposal == nil || input == nil || proposal.InputID != input.ID || proposal.Revision != input.Revision {
		return nil, "", errors.New("Issue 提案与当前输入版本不匹配")
	}
	if len(proposal.Segments) == 0 || len(proposal.Segments) > maxIssueProposalSegments {
		return nil, "", fmt.Errorf("Issue 提案必须包含 1–%d 个原文片段", maxIssueProposalSegments)
	}
	runes := []rune(input.Text)
	covered := make([]bool, len(runes))
	validated := make([]IssueProposalSegment, len(proposal.Segments))
	linked, clarified := false, false
	for index, segment := range proposal.Segments {
		if segment.SourceStart < 0 || segment.SourceEnd <= segment.SourceStart || segment.SourceEnd > len(runes) {
			return nil, "", fmt.Errorf("第 %d 个片段的来源区间越界", index+1)
		}
		for pos := segment.SourceStart; pos < segment.SourceEnd; pos++ {
			if covered[pos] {
				return nil, "", fmt.Errorf("第 %d 个片段与其他片段重叠", index+1)
			}
			covered[pos] = true
		}
		segment.NewIssueTitle = normalizeIssueTitle(segment.NewIssueTitle)
		switch segment.Action {
		case IssueSegmentCreate:
			if segment.IssueID != "" || segment.NewIssueTitle == "" || len([]rune(segment.NewIssueTitle)) > maxIssueTitleRunes {
				return nil, "", fmt.Errorf("第 %d 个新 Issue 片段必须提供 1–%d 字标题且不能同时提供 issueId", index+1, maxIssueTitleRunes)
			}
			segment.IssueID = issueStableID(teamID, input.ID, input.Revision, segment.NewIssueTitle)
			linked = true
		case IssueSegmentLink:
			existing := issues[segment.IssueID]
			if segment.IssueID == "" || existing == nil || existing.TeamID != teamID || segment.NewIssueTitle != "" {
				return nil, "", fmt.Errorf("第 %d 个关联片段引用了不存在或不属于当前团队的 Issue", index+1)
			}
			linked = true
		case IssueSegmentIgnore:
			if segment.IssueID != "" || segment.NewIssueTitle != "" {
				return nil, "", fmt.Errorf("第 %d 个无需建项片段不能带 Issue 标识或标题", index+1)
			}
		case IssueSegmentClarify:
			segment.Reason = strings.TrimSpace(segment.Reason)
			if segment.IssueID != "" || segment.NewIssueTitle != "" || segment.Reason == "" || len([]rune(segment.Reason)) > maxIssueClarificationRunes {
				return nil, "", fmt.Errorf("第 %d 个待澄清片段必须给出不超过 %d 字的原因", index+1, maxIssueClarificationRunes)
			}
			clarified = true
		default:
			return nil, "", fmt.Errorf("第 %d 个 Issue 片段 action 无效", index+1)
		}
		validated[index] = segment
	}
	for pos, ok := range covered {
		if !ok && !unicode.IsSpace(runes[pos]) {
			return nil, "", fmt.Errorf("Issue 提案漏掉原文位置 %d", pos)
		}
	}
	status := IssueAssociationIgnored
	if clarified {
		status = IssueAssociationClarification
	} else if linked {
		status = IssueAssociationLinked
	}
	return validated, status, nil
}

func (issue *Issue) Clone() *Issue {
	if issue == nil {
		return nil
	}
	copy := *issue
	copy.Links = append([]IssueInputLink(nil), issue.Links...)
	return &copy
}

// ApplyIssueProposal 校验原文覆盖完整性并提交 Issue 创建/关联事实。proposalID 基于输入版本生成，供结算重试幂等去重。
func (l *InputLedger) ApplyIssueProposal(teamID, proposalID string, proposal *IssueProposal) (*TeamInput, bool, error) {
	if teamID == "" || proposalID == "" || proposal == nil {
		return nil, false, errors.New("Issue 提案缺少团队、幂等标识或内容")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if owner, ok := l.ops[proposalID]; ok {
		if owner != proposal.InputID {
			return nil, false, ErrInputContentMismatch
		}
		if input := l.inputs[proposal.InputID]; input != nil && input.TeamID == teamID {
			return input.Clone(), true, nil
		}
		return nil, false, ErrNotFound
	}
	input := l.inputs[proposal.InputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if !input.IssueRouting {
		return nil, false, errors.New("该输入未启用 Issue 归类")
	}
	if input.AssociationStatus != IssueAssociationPending {
		return nil, false, errors.New("该输入当前不处于待归类状态")
	}
	if input.AssociationProposalID != "" {
		return nil, false, errors.New("该输入的 Issue 归类已经完成")
	}
	segments, status, err := validateIssueProposal(proposal, input, teamID, l.issues)
	if err != nil {
		return nil, false, err
	}
	created := []Issue{}
	createdByID := map[string]bool{}
	for _, segment := range segments {
		if segment.Action != IssueSegmentCreate || createdByID[segment.IssueID] || l.issues[segment.IssueID] != nil {
			continue
		}
		createdByID[segment.IssueID] = true
		created = append(created, Issue{ID: segment.IssueID, TeamID: teamID, Title: segment.NewIssueTitle})
	}
	reasons := []string{}
	for _, segment := range segments {
		if segment.Action == IssueSegmentClarify && segment.Reason != "" && !containsString(reasons, segment.Reason) {
			reasons = append(reasons, segment.Reason)
		}
	}
	change := issueProposalEvent{
		InputID: proposal.InputID, TeamID: teamID, Revision: input.Revision,
		ProposalID: proposalID, AssociationStatus: status,
		AssociationReason: strings.Join(reasons, "；"), Segments: segments, CreatedIssues: created,
	}
	raw, err := json.Marshal(change)
	if err != nil {
		return nil, false, err
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, err
	}
	event := newEvent("", EventIssueProposalApplied, "lead", payload)
	event.IdempotencyKey = proposalID
	if err := l.commitLocked(event, func() { l.applyInputEvent(event) }); err != nil {
		return nil, false, err
	}
	return l.inputs[proposal.InputID].Clone(), false, nil
}

func (l *InputLedger) applyIssueProposalEvent(event *Event) {
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		return
	}
	var change issueProposalEvent
	if err := json.Unmarshal(raw, &change); err != nil || change.InputID == "" || change.TeamID == "" || change.ProposalID == "" {
		return
	}
	input := l.inputs[change.InputID]
	if input == nil || input.TeamID != change.TeamID || input.AssociationProposalID == change.ProposalID {
		return
	}
	l.ops[change.ProposalID] = change.InputID
	for _, issue := range change.CreatedIssues {
		if issue.ID == "" || issue.TeamID != change.TeamID || issue.Title == "" || l.issues[issue.ID] != nil {
			continue
		}
		issue.CreatedAt = event.CreatedAt
		issue.UpdatedAt = event.CreatedAt
		l.issues[issue.ID] = issue.Clone()
		l.issuesOrder = append(l.issuesOrder, issue.ID)
	}
	source := []rune(input.Text)
	for _, segment := range change.Segments {
		if segment.Action != IssueSegmentCreate && segment.Action != IssueSegmentLink {
			continue
		}
		issue := l.issues[segment.IssueID]
		if issue == nil || issue.TeamID != change.TeamID || segment.SourceStart < 0 || segment.SourceEnd > len(source) || segment.SourceEnd <= segment.SourceStart {
			continue
		}
		link := IssueInputLink{
			IssueID: issue.ID, InputID: change.InputID, Revision: change.Revision,
			SourceStart: segment.SourceStart, SourceEnd: segment.SourceEnd,
			SourceText: string(source[segment.SourceStart:segment.SourceEnd]), LinkedAt: event.CreatedAt,
		}
		issue.Links = appendIssueInputLink(issue.Links, link)
		issue.UpdatedAt = event.CreatedAt
		input.IssueLinks = appendIssueInputLink(input.IssueLinks, link)
	}
	input.AssociationStatus = change.AssociationStatus
	input.AssociationReason = change.AssociationReason
	input.AssociationProposalID = change.ProposalID
	input.Version++
	input.UpdatedAt = event.CreatedAt
}

func appendIssueInputLink(links []IssueInputLink, link IssueInputLink) []IssueInputLink {
	for _, existing := range links {
		if existing.IssueID == link.IssueID && existing.InputID == link.InputID && existing.Revision == link.Revision &&
			existing.SourceStart == link.SourceStart && existing.SourceEnd == link.SourceEnd {
			return links
		}
	}
	return append(links, link)
}

// DeleteInput 只删除已经离开路由队列且没有关联事项的输入投影。
// 删除事实随 journal 和快照持久化，执行记录保持原样。
func (l *InputLedger) DeleteInput(teamID, inputID string, version uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return ErrNotFound
	}
	if input.Version != version {
		return ErrVersionConflict
	}
	if input.QueueStatus == InputQueued || input.QueueStatus == InputRouting || input.QueueStatus == InputWaitingInput {
		return fmt.Errorf("%w：消息仍在等待路由，请先取消或等待结束", ErrInputNotQueued)
	}
	for _, issue := range l.issues {
		if issue.TeamID != teamID {
			continue
		}
		for _, link := range issue.Links {
			if link.InputID == inputID {
				return fmt.Errorf("%w：消息已关联事项，请从事项详情删除", ErrInputNotQueued)
			}
		}
	}
	event := newEvent("", EventInputDeleted, "api", map[string]any{"teamId": teamID, "inputId": inputID})
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":deleted"
	return l.commitLocked(event, func() { l.applyInputEvent(event) })
}

// DeleteIssue 删除 Issue 及其来源输入投影；journal 与 Run 执行记录保留。
func (l *InputLedger) DeleteIssue(teamID, issueID string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	issue := l.issues[issueID]
	if issue == nil || issue.TeamID != teamID {
		return 0, ErrNotFound
	}
	inputIDs := []string{}
	seen := map[string]bool{}
	for _, link := range issue.Links {
		if seen[link.InputID] {
			continue
		}
		seen[link.InputID] = true
		input := l.inputs[link.InputID]
		if input == nil || input.TeamID != teamID {
			continue
		}
		switch input.QueueStatus {
		case InputQueued, InputRouting, InputWaitingInput:
			return 0, fmt.Errorf("%w：事项仍有待路由对话", ErrInputNotQueued)
		}
		inputIDs = append(inputIDs, link.InputID)
	}
	event := newEvent("", EventIssueDeleted, "api", map[string]any{
		"teamId": teamID, "issueId": issueID, "inputIds": inputIDs,
	})
	event.IdempotencyKey = "team:" + teamID + ":issue:" + issueID + ":deleted"
	if err := l.commitLocked(event, func() { l.applyInputEvent(event) }); err != nil {
		return 0, err
	}
	return len(inputIDs), nil
}

func (l *InputLedger) applyIssueDeletedEvent(event *Event) {
	teamID := payloadString(event.Payload, "teamId")
	issueID := payloadString(event.Payload, "issueId")
	if teamID == "" || issueID == "" {
		return
	}
	deletedInputs := map[string]bool{}
	for _, inputID := range payloadStrings(event.Payload, "inputIds") {
		input := l.inputs[inputID]
		if input == nil || input.TeamID != teamID {
			continue
		}
		delete(l.inputs, inputID)
		deletedInputs[inputID] = true
	}
	for _, issue := range l.issues {
		if issue == nil || issue.TeamID != teamID {
			continue
		}
		links := issue.Links[:0]
		for _, link := range issue.Links {
			if link.IssueID != issueID && !deletedInputs[link.InputID] {
				links = append(links, link)
			}
		}
		if len(links) != len(issue.Links) {
			issue.Links = links
			issue.UpdatedAt = event.CreatedAt
		}
	}
	if issue := l.issues[issueID]; issue != nil && issue.TeamID == teamID {
		delete(l.issues, issueID)
	}
	l.order = removeMissing(l.order, l.inputs)
	l.issuesOrder = removeMissing(l.issuesOrder, l.issues)
}

func (l *InputLedger) GetIssue(teamID, issueID string) *Issue {
	l.mu.Lock()
	defer l.mu.Unlock()
	issue := l.issues[issueID]
	if issue == nil || issue.TeamID != teamID {
		return nil
	}
	return issue.Clone()
}

func (l *InputLedger) ListIssues(teamID string) []*Issue {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows := []*Issue{}
	for _, key := range l.issuesOrder {
		if issue := l.issues[key]; issue != nil && issue.TeamID == teamID {
			rows = append(rows, issue.Clone())
		}
	}
	return rows
}

func (l *InputLedger) IssueAssociationBriefing(teamID, inputID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID || !input.IssueRouting || input.AssociationStatus != IssueAssociationPending || input.AssociationProposalID != "" {
		return ""
	}
	var builder strings.Builder
	if input.Historical {
		builder.WriteString("[Team historical issue routing]\n")
		builder.WriteString("这是从既有团队对话离线整理的历史原话。只归类主题，不要重新执行、委派或修改原要求。必须只输出 team-issues 归类块，不要输出 team-actions。\n")
	} else {
		builder.WriteString("[Team issue routing]\n")
		builder.WriteString("这是新收到的用户输入。你是团队主控，必须先判断每个原文片段应关联已有 Issue、创建新 Issue、无需建项，或需要澄清。归类提案在本轮必须输出，不能只在自然语言里描述关联结果。\n")
	}
	builder.WriteString("输出一个 team-issues JSON 块。格式：\n")
	builder.WriteString("<team-issues>\n")
	builder.WriteString("{\"inputId\":\"<inputId>\",\"revision\":1,\"segments\":[")
	builder.WriteString("{\"action\":\"link\",\"issueId\":\"<已有ID>\",\"sourceStart\":0,\"sourceEnd\":8},")
	builder.WriteString("{\"action\":\"create\",\"newIssueTitle\":\"简短事项标题\",\"sourceStart\":9,\"sourceEnd\":20},")
	builder.WriteString("{\"action\":\"ignore\",\"sourceStart\":0,\"sourceEnd\":4},")
	builder.WriteString("{\"action\":\"clarify\",\"reason\":\"需要确认目标\",\"sourceStart\":21,\"sourceEnd\":28}]}\n")
	builder.WriteString("</team-issues>\n")
	builder.WriteString("sourceStart/sourceEnd 是原消息的 0 起始 Unicode rune 半开区间。所有非空白原文必须且只能被一个片段覆盖；消息可以拆成多个不同主题，每段分别选择 action。只有主题明确连续时才关联已有 Issue；不要因为同属一个项目而合并。寒暄和不需要跟进的纯沟通使用 ignore；只有确实无法判断目标时才用 clarify。create 的标题要短且能代表长期事项。\n")
	builder.WriteString(fmt.Sprintf("当前输入：inputId=%s revision=%d。\n", input.ID, input.Revision))
	builder.WriteString("原始输入全文（片段区间以此文本的 Unicode rune 索引为准）：\n")
	builder.WriteString(input.Text)
	builder.WriteString("\n")
	existing := []*Issue{}
	for _, issueID := range l.issuesOrder {
		if issue := l.issues[issueID]; issue != nil && issue.TeamID == teamID {
			existing = append(existing, issue)
		}
	}
	if len(existing) == 0 {
		builder.WriteString("当前团队没有已有 Issue；实质工作通常新建 Issue，纯沟通标为 ignore。\n")
	} else {
		builder.WriteString("当前团队已有 Issue（只能用以下 ID 关联，不要用标题猜 ID）：\n")
		for _, issue := range existing {
			fmt.Fprintf(&builder, "- issueId=%s · title=%s\n", issue.ID, truncateRunes(issue.Title, maxIssueTitleRunes))
		}
	}
	return builder.String()
}

func IssueAssociationRepairPrompt(violations []string, attempt int) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("上一次历史输入归类提案第 %d 次校验失败，旧任务绝不会被执行。具体问题：\n", attempt))
	for i, violation := range boundedViolations(violations) {
		fmt.Fprintf(&builder, "%d. %s\n", i+1, violation)
	}
	builder.WriteString("请只根据后附原始输入和 Issue 目录，重新输出 team-issues 块；不得输出 team-actions 或委派工作。\n")
	return builder.String()
}

func (l *InputLedger) FailIssueAssociation(teamID, inputID, reason string) (*TeamInput, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if !input.IssueRouting || input.AssociationStatus != IssueAssociationPending || input.AssociationProposalID != "" {
		return input.Clone(), true, nil
	}
	reason = truncateRunes(strings.TrimSpace(reason), maxIssueClarificationRunes)
	if reason == "" {
		reason = "主控未能完成 Issue 归类"
	}
	event := newEvent("", EventIssueAssociationFailed, "lead", map[string]any{
		"teamId": teamID, "inputId": inputID, "revision": jsoncompat.Num(input.Revision), "reason": reason,
	})
	event.IdempotencyKey = fmt.Sprintf("team:%s:input:%s:issue-failed:%d", teamID, inputID, input.Revision)
	if err := l.commitLocked(event, func() { l.applyIssueAssociationFailedEvent(event) }); err != nil {
		return nil, false, err
	}
	return l.inputs[inputID].Clone(), false, nil
}

func (l *InputLedger) applyIssueAssociationFailedEvent(event *Event) {
	inputID := payloadString(event.Payload, "inputId")
	teamID := payloadString(event.Payload, "teamId")
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID || input.Revision != payloadInt(event.Payload, "revision") ||
		!input.IssueRouting || input.AssociationStatus != IssueAssociationPending || input.AssociationProposalID != "" {
		return
	}
	input.AssociationStatus = IssueAssociationFailed
	input.AssociationReason = payloadString(event.Payload, "reason")
	input.Version++
	input.UpdatedAt = event.CreatedAt
}

func issueInputIDForLeadTask(run *Run, task *Task) string {
	if run == nil || task == nil || (task.ExecutionContext == ExecutionContextLeadRun && !task.IssueClassificationOnly) {
		return ""
	}
	id := task.ID
	if strings.HasPrefix(id, actionRepairPrefix) {
		id = actionRepairRoot(id)
	}
	switch {
	case id == "lead":
		return run.ID
	case strings.HasPrefix(id, "input-"):
		return strings.TrimPrefix(id, "input-")
	default:
		return ""
	}
}

func issueProposalID(teamID, inputID string, revision int) string {
	return fmt.Sprintf("team:%s:input:%s:issue-proposal:%d", teamID, inputID, revision)
}
