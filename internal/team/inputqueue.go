package team

// inputqueue.go 是团队输入的持久队列账本（docs/team-demand-queue-plan.md
// §3，阶段 1）：先记账后规划——输入正文、附件引用与幂等键先落 team-inputs
// journal（追加事实），再写 team-inputs.json 快照（可重建投影）。崩溃后
// 重放 journal 幂等补齐，已接受输入在规划前重启仍可查询和继续。
//
//   - 事件：接受、修订、排队操作、路由意图/撤销/确认、工作项提案与链接
//     均追加落账，快照只是可重建投影；
//   - 幂等：Accept 以全局 inputID 为幂等键——同 Team 同 ID 同内容重放返回
//     原状态，异 Team 或异内容返回 ErrInputContentMismatch（HTTP 映射 409）；修订/优先/取消
//     以操作 UUID 幂等，重复操作返回既有结果，版本冲突返回 ErrVersionConflict；
//   - 排队中的「编辑」新增 revision 并保留旧版本（修订不可变），routing 后
//     拒绝原地编辑；
//   - 容量：pending（queued 且未取消）达到上限返回 ErrQueueFull（HTTP 映射
//     429），不自动丢弃旧输入。
//
// 输入 journal 与 Run journal 是两份独立账本，没有跨文件原子事务：所有
// 事件先落 journal 再改内存、最后落快照；快照只是重建缓存，损坏时按
// journal 事实重建（store.go 同款隔离-重建语义）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// 输入生命周期取值（§3.1）：queueStatus 只表示入口生命周期；Task/Run 的
// 执行状态仍由 Run 账本负责。
const (
	InputQueued       = "queued"
	InputRouting      = "routing"
	InputRouted       = "routed"
	InputWaitingInput = "waiting_input"
	InputFailed       = "failed"
	InputCancelled    = "cancelled"
)

// 输入事件类型（journal 只增不删，§9.2）。
const (
	EventInputAccepted           = "input_accepted"
	EventInputRevised            = "input_revised"
	EventInputPrioritized        = "input_prioritized"
	EventInputCancelled          = "input_cancelled"
	EventInputRouteIntent        = "input_route_intent"
	EventInputRouteIntentRevoked = "input_route_intent_revoked"
	EventInputRouted             = "input_routed"
	EventInputWaitingInput       = "input_waiting_input"
	EventWorkItemLinked          = "work_item_linked"
	EventWorkItemsProposed       = "work_items_proposed"
	EventIssueProposalApplied    = "issue_proposal_applied"
	EventIssueAssociationFailed  = "issue_association_failed"
	EventIssueDeleted            = "issue_deleted"
	EventInputDeleted            = "input_deleted"
	EventInputTeamPurged         = "input_team_purged"
)

// 输入账本错误（HTTP 层按类型映射状态码）。
var (
	// ErrInputContentMismatch 是同 ID 异内容重放（409）。
	ErrInputContentMismatch = errors.New("同一消息标识不能用于不同内容")
	// ErrInputNotQueued 表示输入已离开 queued 生命周期（routing/routed/
	// cancelled 等），排队态操作（编辑/优先/取消）被拒绝（409）。
	ErrInputNotQueued = errors.New("输入已进入路由或已取消，排队操作不可用")
	// ErrQueueFull 表示待路由输入达到 MaxPendingInputs 上限（429）。
	ErrQueueFull = errors.New("待处理输入已达上限，请等待路由或调整团队容量")
)

// DefaultMaxPendingInputs 是每 Team 待路由输入的默认上限（§3.2：独立于
// MaxTasksPerRun 的可调上限，容量与速率应依据真实负载再调）。
const DefaultMaxPendingInputs = 64

// 阶段 3 默认值与常量（§5，docs/team-demand-queue-plan.md）。
const (
	// DefaultMaxActiveRuns 是每 Team 活跃 Run 准入上限的建议值。
	DefaultMaxActiveRuns = 2
	// DefaultQueueDispatchBudget 是 Team 队列滚动 24 小时派发总额的默认值
	//（任务派发次数而非 token 额度）。
	DefaultQueueDispatchBudget = 48
	// queueDispatchWindow 是队列预算的滚动窗口。
	queueDispatchWindow = 24 * time.Hour
	// routerAgingAfter 是普通输入的反饥饿提升线（等待超过即视同优先，
	// §5「普通输入等待超过 10 分钟即提升到下一次路由」）。
	routerAgingAfter = 10 * time.Minute
	// routerPriorityQuota 是优先/普通配额（§5「每处理 3 条优先输入至少
	// 处理 1 条普通输入」）：连续 routed 的优先输入达到该数后，下一条
	// 强制取最早的普通输入。
	routerPriorityQuota = 3
)

// InputRevision 是输入正文的一个不可变版本（§3.1「修订不改历史」）：编辑
// 新增版本并将旧版本标记为被替换，不原地改写已接受事实。
type InputRevision struct {
	Revision      int       `json:"revision"`
	Text          string    `json:"text"`
	AttachmentIDs []string  `json:"attachmentIds,omitempty"`
	ContentHash   string    `json:"contentHash"`
	CreatedAt     time.Time `json:"createdAt"`
	// ReplacedBy 是替换本版本的后继修订号；0=当前版本。
	ReplacedBy int `json:"replacedBy,omitempty"`
}

// WorkItem 是需求到 Run/Task 的持久索引（§3.1，阶段 2）：不持有第二份执行
// 状态，Task/Run 的执行状态仍由 Run 账本负责。
type WorkItem struct {
	ID               string   `json:"id"`
	SourceInputID    string   `json:"sourceInputId"`
	SourceRevision   int      `json:"sourceRevision"`
	RequirementText  string   `json:"requirementText"`
	SourceStart      int      `json:"sourceStart"`
	SourceEnd        int      `json:"sourceEnd"`
	MemberID         string   `json:"memberId,omitempty"`
	ReviewPolicy     string   `json:"reviewPolicy,omitempty"`
	RunID            string   `json:"runId,omitempty"`
	TaskIDs          []string `json:"taskIds,omitempty"`
	DependsOnItemIDs []string `json:"dependsOnItemIds,omitempty"`
	Status           string   `json:"status"`
	EvidenceRef      string   `json:"evidenceRef,omitempty"`
}

// TeamInput 是一条已受理的团队输入（§3.1 最小字段）。
type TeamInput struct {
	ID                     string           `json:"id"`
	TeamID                 string           `json:"teamId"`
	Sequence               uint64           `json:"sequence"` // 每 Team 的接受序号（FIFO 依据）
	Revision               int              `json:"revision"`
	Text                   string           `json:"text"`
	AttachmentIDs          []string         `json:"attachmentIds,omitempty"`
	ContentHash            string           `json:"contentHash"`
	AcceptedAt             time.Time        `json:"acceptedAt"`
	QueueStatus            string           `json:"queueStatus"`
	RouteStatus            string           `json:"routeStatus"`
	TargetRunID            string           `json:"targetRunId,omitempty"`
	TargetTaskID           string           `json:"targetTaskId,omitempty"`
	SourceTaskID           string           `json:"sourceTaskId,omitempty"`
	ParentRunID            string           `json:"parentRunId,omitempty"`
	Priority               bool             `json:"priority,omitempty"`
	PolicyDigest           string           `json:"policyDigest,omitempty"`
	Version                uint64           `json:"version"` // 乐观版本：接受=1，每次状态变更 +1
	RunIDs                 []string         `json:"runIds,omitempty"`
	WorkItemIDs            []string         `json:"workItemIds,omitempty"`
	WorkItems              []WorkItem       `json:"workItems,omitempty"`
	AssociationStatus      string           `json:"associationStatus,omitempty"`
	IssueRouting           bool             `json:"issueRouting,omitempty"`
	AssociationReason      string           `json:"associationReason,omitempty"`
	AssociationProposalID  string           `json:"associationProposalId,omitempty"`
	IssueLinks             []IssueInputLink `json:"issueLinks,omitempty"`
	Historical             bool             `json:"historical,omitempty"`
	HistoricalSourceRunID  string           `json:"historicalSourceRunId,omitempty"`
	HistoricalSourceTaskID string           `json:"historicalSourceTaskId,omitempty"`
	// WaitingReason 是 waiting_input 停靠的原因（阶段 2：多活跃 Run、Run 收
	// 集中等），UI/API 据此提示下一步。
	WaitingReason string `json:"waitingReason,omitempty"`
	// ExplicitTarget 只用于尚无路由意图的输入；一旦意图落账，重泵始终使用
	// 持久化的 RouteStatus/TargetRunID/TargetTaskID/ParentRunID，不再重新规划。
	ExplicitTarget bool            `json:"explicitTarget,omitempty"`
	Revisions      []InputRevision `json:"revisions,omitempty"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// Clone 深拷贝 TeamInput（账本出入库都返回副本）。
func (i *TeamInput) Clone() *TeamInput {
	if i == nil {
		return nil
	}
	copied := *i
	copied.AttachmentIDs = append([]string(nil), i.AttachmentIDs...)
	copied.RunIDs = append([]string(nil), i.RunIDs...)
	copied.WorkItemIDs = append([]string(nil), i.WorkItemIDs...)
	copied.WorkItems = append([]WorkItem(nil), i.WorkItems...)
	for index := range copied.WorkItems {
		copied.WorkItems[index].TaskIDs = append([]string(nil), i.WorkItems[index].TaskIDs...)
		copied.WorkItems[index].DependsOnItemIDs = append([]string(nil), i.WorkItems[index].DependsOnItemIDs...)
	}
	copied.IssueLinks = append([]IssueInputLink(nil), i.IssueLinks...)
	copied.Revisions = append([]InputRevision(nil), i.Revisions...)
	for index := range copied.Revisions {
		copied.Revisions[index].AttachmentIDs = append([]string(nil), i.Revisions[index].AttachmentIDs...)
	}
	return &copied
}

// InputContentHash 是输入内容的规范化摘要：text 与附件 ID 列表（保序）逐段
// 拼接后取 sha256——同 ID 同正文同附件重放命中，任何差异冲突。
func InputContentHash(text string, attachments []string) string {
	digest := sha256.New()
	digest.Write([]byte(text))
	for _, id := range attachments {
		digest.Write([]byte{0})
		digest.Write([]byte(id))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// InputLedger 是 team-inputs.jsonl（事实）+ team-inputs.json（快照）的输入
// 账本。并发安全；读取返回深拷贝。
type InputLedger struct {
	mu           sync.Mutex
	snapshotPath string
	journal      *Journal
	order        []string // 全局插入序（分页游标的基础）
	inputs       map[string]*TeamInput
	issues       map[string]*Issue
	issuesOrder  []string
	issueGoals   map[string]*GoalRecord
	ops          map[string]string // 操作 UUID → inputID（重复操作幂等回放）
	teamsSeq     map[string]uint64 // 每 Team 的接受序号发生器
	deleted      map[string]string // inputID → teamID，防止历史导入恢复已删除气泡
}

// LoadInputLedger 打开输入账本：读快照（损坏时隔离原件、按空投影启动），
// 再重放 journal 全量事件幂等补齐（快照落后于 journal 的崩溃窗口由此收敛）。
func LoadInputLedger(snapshotPath string, journal *Journal) *InputLedger {
	ledger := &InputLedger{
		snapshotPath: snapshotPath,
		journal:      journal,
		inputs:       map[string]*TeamInput{},
		issues:       map[string]*Issue{},
		issueGoals:   map[string]*GoalRecord{},
		ops:          map[string]string{},
		teamsSeq:     map[string]uint64{},
		deleted:      map[string]string{},
	}
	object, ok := readLedgerObject(snapshotPath, "team-inputs.json")
	if !ok {
		// 整文件损坏：原件隔离留存（防后续写盘覆盖原始字节），下方按
		// journal 事实重建（store.go 同款语义）。
		if quarantineCorruptFile(snapshotPath) {
			log.Printf("team-inputs.json 损坏，原文件已留存，按 journal 重建: path=%s", snapshotPath)
		}
	}
	if object != nil {
		for _, key := range object.Keys() {
			raw, _ := object.Get(key)
			if key == "_deletedInputs" {
				if rows, ok := raw.(*jsoncompat.Object); ok {
					for _, id := range rows.Keys() {
						if owner, ok := rows.MustGet(id).(string); ok {
							ledger.deleted[id] = owner
						}
					}
				}
				continue
			}
			if key == "_issues" {
				issueRows, _ := raw.(*jsoncompat.Object)
				if issueRows != nil {
					for _, issueID := range issueRows.Keys() {
						issueRaw, _ := issueRows.Get(issueID)
						var issue Issue
						if err := json.Unmarshal(jsoncompat.EncodeCompact(issueRaw), &issue); err != nil || issue.ID != issueID || issue.TeamID == "" {
							log.Printf("team-inputs.json Issue 条目解析失败，已跳过: key=%s", issueID)
							continue
						}
						ledger.issues[issueID] = issue.Clone()
						ledger.issuesOrder = append(ledger.issuesOrder, issueID)
					}
				}
				continue
			}
			if key == "_issueGoals" {
				rows, _ := raw.(*jsoncompat.Object)
				if rows != nil {
					for _, issueID := range rows.Keys() {
						value, _ := rows.Get(issueID)
						record := decodeGoalRecord(value)
						issue := ledger.issues[issueID]
						if record == nil || record.Scope.ID != issueID || issue == nil {
							log.Printf("team-inputs.json Issue 目标记录解析失败，已跳过: key=%s", issueID)
							continue
						}
						ledger.issueGoals[issueID] = record
					}
				}
				continue
			}
			var input TeamInput
			if err := json.Unmarshal(jsoncompat.EncodeCompact(raw), &input); err != nil {
				log.Printf("team-inputs.json 条目解析失败，已跳过: key=%s err=%v", key, err)
				continue
			}
			if input.ID == "" || input.ID != key {
				log.Printf("team-inputs.json 条目缺少 id 或与键不一致，已跳过: key=%s id=%s", key, input.ID)
				continue
			}
			if input.AssociationStatus == "" || (!input.IssueRouting && input.AssociationStatus == IssueAssociationPending) {
				input.AssociationStatus = IssueAssociationUnclassified
			}
			ledger.inputs[key] = &input
			ledger.order = append(ledger.order, key)
			// 序号发生器随快照恢复（journal 事件已被快照体现、重放会跳过，
			// 不补这里会让重启后的首条输入拿到重复 Sequence）。
			if input.Sequence > ledger.teamsSeq[input.TeamID] {
				ledger.teamsSeq[input.TeamID] = input.Sequence
			}
		}
	}
	if journal != nil {
		events, err := journal.Replay()
		if err != nil {
			log.Printf("team-inputs journal 重放失败，输入账本以快照为准: %v", err)
			return ledger
		}
		for _, event := range events {
			ledger.applyInputEvent(event)
		}
	}
	return ledger
}

// pendingCountLocked 统计团队的未路由积压（queued/routing/waiting_input——
// 已接受但尚未落成 Run/Task 的全部形态；routed/cancelled/failed 不计）。
// §3.2 的 maxPendingInputs 背压按此口径：停靠等待的输入同样占容量。
func (l *InputLedger) pendingCountLocked(teamID string) int {
	count := 0
	for _, key := range l.order {
		input := l.inputs[key]
		if input == nil || input.TeamID != teamID {
			continue
		}
		switch input.QueueStatus {
		case InputQueued, InputRouting, InputWaitingInput:
			count++
		}
	}
	return count
}

// Accept 受理一条输入：同 ID 同内容重放返回原状态（duplicate=true）；异内容
// 返回 ErrInputContentMismatch；容量（capacity>0 时）已满返回 ErrQueueFull。
// journal 先行（input_accepted），快照随后；两者之间的崩溃窗口由重放补齐。
func (l *InputLedger) Accept(teamID, inputID, text string, attachments []string, policyDigest string, capacity int) (*TeamInput, bool, error) {
	return l.AcceptWithIssueRouting(teamID, inputID, text, attachments, policyDigest, capacity, false)
}

// AcceptWithIssueRouting 仅把携带 Lead 分类协议版本的输入标记为待归类。
func (l *InputLedger) AcceptWithIssueRouting(teamID, inputID, text string, attachments []string, policyDigest string, capacity int, issueRouting bool) (*TeamInput, bool, error) {
	if teamID == "" || inputID == "" {
		return nil, false, errors.New("输入受理缺少团队或消息标识")
	}
	hash := InputContentHash(text, attachments)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deleted[inputID] != "" {
		return nil, false, ErrNotFound
	}
	if existing := l.inputs[inputID]; existing != nil {
		if existing.TeamID != teamID || existing.ContentHash != hash {
			return nil, false, ErrInputContentMismatch
		}
		return existing.Clone(), true, nil
	}
	pending := l.pendingCountLocked(teamID)
	if capacity > 0 && pending >= capacity {
		return nil, false, fmt.Errorf("%w：当前待处理 %d 条，上限 %d 条", ErrQueueFull, pending, capacity)
	}
	sequence := l.teamsSeq[teamID] + 1
	now := time.Now()
	associationStatus := IssueAssociationUnclassified
	if issueRouting {
		associationStatus = IssueAssociationPending
	}
	input := &TeamInput{
		ID: inputID, TeamID: teamID, Sequence: sequence,
		Revision: 1, Text: text, AttachmentIDs: append([]string(nil), attachments...),
		ContentHash: hash, AcceptedAt: now, QueueStatus: InputQueued,
		AssociationStatus: associationStatus,
		IssueRouting:      issueRouting,
		PolicyDigest:      policyDigest, Version: 1,
		Revisions: []InputRevision{{
			Revision: 1, Text: text, AttachmentIDs: append([]string(nil), attachments...),
			ContentHash: hash, CreatedAt: now,
		}},
		UpdatedAt: now,
	}
	event := newEvent("", EventInputAccepted, "api", map[string]any{
		"inputId": inputID, "teamId": teamID, "sequence": jsoncompat.Num(int(sequence)),
		"text": text, "contentHash": hash, "policyDigest": policyDigest,
	})
	if len(attachments) > 0 {
		event.Payload["attachmentIds"] = append([]string(nil), attachments...)
	}
	if issueRouting {
		event.Payload["issueRouting"] = true
	}
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":accept"
	if err := l.commitLocked(event, func() {
		l.applyInputEvent(event)
	}); err != nil {
		return nil, false, err
	}
	return input.Clone(), false, nil
}

func (l *InputLedger) ImportHistoricalInput(teamID, inputID, text, sourceRunID, sourceTaskID string, createdAt time.Time) (*TeamInput, bool, error) {
	if teamID == "" || inputID == "" || sourceRunID == "" || strings.TrimSpace(text) == "" {
		return nil, false, errors.New("历史输入缺少团队、来源或正文")
	}
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	hash := InputContentHash(text, nil)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deleted[inputID] == teamID {
		return nil, true, nil
	}
	if existing := l.inputs[inputID]; existing != nil {
		if existing.TeamID != teamID || existing.ContentHash != hash ||
			existing.HistoricalSourceRunID != sourceRunID || existing.HistoricalSourceTaskID != sourceTaskID {
			return nil, false, ErrInputContentMismatch
		}
		return existing.Clone(), true, nil
	}
	sequence := l.teamsSeq[teamID] + 1
	event := newEvent("", EventInputAccepted, "history_import", map[string]any{
		"inputId": inputID, "teamId": teamID, "sequence": jsoncompat.Num(int(sequence)),
		"text": text, "contentHash": hash, "issueRouting": true, "historical": true,
		"historicalSourceRunId": sourceRunID, "historicalSourceTaskId": sourceTaskID,
		"historicalAt": createdAt.Format(time.RFC3339Nano), "sourceRunId": sourceRunID,
	})
	event.IdempotencyKey = "team:" + teamID + ":history:" + sourceRunID + ":" + sourceTaskID
	if err := l.commitLocked(event, func() { l.applyInputEvent(event) }); err != nil {
		return nil, false, err
	}
	return l.inputs[inputID].Clone(), false, nil
}

// Revise 修订排队中的输入（仅 queued）：新增不可变 revision 并替换当前正文。
// 操作 UUID 幂等；期望版本不匹配返回 ErrVersionConflict。
func (l *InputLedger) Revise(teamID, inputID, opID, text string, attachments []string, expectedVersion uint64) (*TeamInput, bool, error) {
	hash := InputContentHash(text, attachments)
	l.mu.Lock()
	defer l.mu.Unlock()
	if input, duplicate, err := l.replayOpLocked(teamID, inputID, opID); duplicate || err != nil {
		return input, duplicate, err
	}
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if input.QueueStatus != InputQueued {
		return nil, false, fmt.Errorf("%w：当前 %s", ErrInputNotQueued, input.QueueStatus)
	}
	if input.Version != expectedVersion {
		return nil, false, ErrVersionConflict
	}
	revision := input.Revision + 1
	event := newEvent("", EventInputRevised, "api", map[string]any{
		"inputId": inputID, "teamId": teamID, "revision": jsoncompat.Num(revision),
		"text": text, "contentHash": hash, "opId": opID,
	})
	if len(attachments) > 0 {
		event.Payload["attachmentIds"] = append([]string(nil), attachments...)
	}
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":revise:" + opID
	if err := l.commitLocked(event, func() {
		l.applyInputEvent(event)
	}); err != nil {
		return nil, false, err
	}
	return l.inputs[inputID].Clone(), false, nil
}

// Prioritize 把排队输入提升为优先（「立即发送」只提前路由顺序，不绕过任何
// 资源约束；顺序语义由阶段 3 的路由循环执行）。已是优先时幂等无事件。
func (l *InputLedger) Prioritize(teamID, inputID, opID string, expectedVersion uint64) (*TeamInput, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if input, duplicate, err := l.replayOpLocked(teamID, inputID, opID); duplicate || err != nil {
		return input, duplicate, err
	}
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if input.QueueStatus != InputQueued {
		return nil, false, fmt.Errorf("%w：当前 %s", ErrInputNotQueued, input.QueueStatus)
	}
	if input.Priority {
		return input.Clone(), false, nil
	}
	if input.Version != expectedVersion {
		return nil, false, ErrVersionConflict
	}
	event := newEvent("", EventInputPrioritized, "api", map[string]any{
		"inputId": inputID, "teamId": teamID, "opId": opID,
	})
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":prioritize:" + opID
	if err := l.commitLocked(event, func() {
		l.applyInputEvent(event)
	}); err != nil {
		return nil, false, err
	}
	return l.inputs[inputID].Clone(), false, nil
}

// Cancel 取消输入：阶段 1 全部输入都未派发，直接落 cancelled；阶段 2 起
// 仅未派发部分可取消（routing 中的部分按血缘修订处理）。
func (l *InputLedger) Cancel(teamID, inputID, opID string, expectedVersion uint64) (*TeamInput, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if input, duplicate, err := l.replayOpLocked(teamID, inputID, opID); duplicate || err != nil {
		return input, duplicate, err
	}
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if input.QueueStatus != InputQueued {
		return nil, false, fmt.Errorf("%w：当前 %s", ErrInputNotQueued, input.QueueStatus)
	}
	if input.Version != expectedVersion {
		return nil, false, ErrVersionConflict
	}
	event := newEvent("", EventInputCancelled, "api", map[string]any{
		"inputId": inputID, "teamId": teamID, "opId": opID,
	})
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":cancel:" + opID
	if err := l.commitLocked(event, func() {
		l.applyInputEvent(event)
	}); err != nil {
		return nil, false, err
	}
	return l.inputs[inputID].Clone(), false, nil
}

// replayOpLocked 是操作幂等回放：opID 已落账且属于本输入时返回既有结果
// （duplicate=true）；opID 被其他输入占用（跨输入复用操作键）按内容冲突
// 拒绝；opID 为空不回放（必填校验在 HTTP 层）。
func (l *InputLedger) replayOpLocked(teamID, inputID, opID string) (*TeamInput, bool, error) {
	if opID == "" {
		return nil, false, nil
	}
	owner, seen := l.ops[opID]
	if !seen {
		return nil, false, nil
	}
	if owner != inputID {
		return nil, true, ErrInputContentMismatch
	}
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	return input.Clone(), true, nil
}

// Get 返回输入的深拷贝；不存在或不属于该团队返回 nil。
func (l *InputLedger) Get(teamID, inputID string) *TeamInput {
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil
	}
	return input.Clone()
}

// ListInputs 按接受序返回团队的输入分页：cursor 是上一页最后一条 inputId
// （空串从头开始），limit ≤0 按 50、>200 收敛到 200。nextCursor 非空表示
// 还有后续页。
func (l *InputLedger) ListInputs(teamID, cursor string, limit int) ([]*TeamInput, string) {
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	teamOrder := make([]string, 0, len(l.order))
	for _, key := range l.order {
		if input := l.inputs[key]; input != nil && input.TeamID == teamID {
			teamOrder = append(teamOrder, key)
		}
	}
	start := 0
	if cursor != "" {
		for index, key := range teamOrder {
			if key == cursor {
				start = index + 1
				break
			}
		}
	}
	end := start + limit
	if end > len(teamOrder) {
		end = len(teamOrder)
	}
	rows := make([]*TeamInput, 0, end-start)
	for _, key := range teamOrder[start:end] {
		rows = append(rows, l.inputs[key].Clone())
	}
	next := ""
	if end < len(teamOrder) && end > start {
		next = teamOrder[end-1]
	}
	return rows, next
}

// ListAll 返回 Team 全部输入的深拷贝，供内部任务血缘投影使用。
func (l *InputLedger) ListAll(teamID string) []*TeamInput {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows := []*TeamInput{}
	for _, key := range l.order {
		if input := l.inputs[key]; input != nil && input.TeamID == teamID {
			rows = append(rows, input.Clone())
		}
	}
	return rows
}

// PendingCount 返回团队当前待路由（queued）输入数。
func (l *InputLedger) PendingCount(teamID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pendingCountLocked(teamID)
}

// PurgeTeam 清理已删团队的输入（保留 journal 原件，只落一条清理事实并从
// 投影移除——输入账本不跟随团队复活）。
func (l *InputLedger) PurgeTeam(teamID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	purged := 0
	for _, key := range l.order {
		if input := l.inputs[key]; input != nil && input.TeamID == teamID {
			purged++
		}
	}
	issueCount := 0
	for _, issue := range l.issues {
		if issue != nil && issue.TeamID == teamID {
			issueCount++
		}
	}
	if purged == 0 && issueCount == 0 {
		return nil
	}
	event := newEvent("", EventInputTeamPurged, "api", map[string]any{
		"teamId": teamID, "count": jsoncompat.Num(purged),
	})
	return l.commitLocked(event, func() {
		l.applyInputEvent(event)
	})
}

// ---- 路由生命周期（阶段 2，docs/team-demand-queue-plan.md §3.1/§4）----
//
// 顺序契约：先持久化路由意图（input_route_intent，携带 routeKind 与确定性
// 目标），网关幂等创建 Run/Task，最后确认 input_routed；崩溃后重放把停在
// routing 的输入按 intent 对账补齐（router 的重泵路径），不得永久停在
// routing 或出现「已 routed 却无任务」。waiting_input 是可恢复停靠：原因
// 记账，随重泵复核。

// MarkRoutingIntent 落完整路由意图（queued → routing）。routing/waiting_input
// 中已有意图只允许完整字段一致的重放；改派必须先显式撤销旧意图。
func (l *InputLedger) MarkRoutingIntent(teamID, inputID, kind, targetRunID, targetTaskID, parentRunID string, sourceTaskIDs ...string) error {
	sourceTaskID := ""
	if len(sourceTaskIDs) > 0 {
		sourceTaskID = sourceTaskIDs[0]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return ErrNotFound
	}
	switch kind {
	case "new_task", "follow_up", "amend", "planned":
	default:
		return errors.New("未知路由种类：" + kind)
	}
	if (input.QueueStatus == InputRouting || input.QueueStatus == InputWaitingInput) && input.RouteStatus != "" {
		if input.RouteStatus == kind && input.TargetRunID == targetRunID &&
			input.TargetTaskID == targetTaskID && input.ParentRunID == parentRunID && input.SourceTaskID == sourceTaskID {
			return nil
		}
		return errors.New("输入已有不同的路由意图，请先撤销原意图")
	}
	if input.QueueStatus != InputQueued && input.QueueStatus != InputWaitingInput && input.QueueStatus != InputRouting {
		return fmt.Errorf("%w：当前 %s", ErrInputNotQueued, input.QueueStatus)
	}
	event := newEvent("", EventInputRouteIntent, "router", map[string]any{
		"inputId": inputID, "teamId": teamID, "routeKind": kind,
		"targetRunId": targetRunID, "targetTaskId": targetTaskID,
		"parentRunId": parentRunID, "sourceTaskId": sourceTaskID,
	})
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":intent:" + kind + ":" + targetRunID
	return l.commitLocked(event, func() {
		l.applyInputEvent(event)
	})
}

// RevokeRoutingIntent 显式撤销未落成的路由意图，使输入回到 queued 重新规划。
// 操作 UUID 与期望版本保证人工处置可重放且不会误撤销后续状态。
func (l *InputLedger) RevokeRoutingIntent(teamID, inputID, opID string, expectedVersion uint64) (*TeamInput, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if input, duplicate, err := l.replayOpLocked(teamID, inputID, opID); duplicate || err != nil {
		return input, duplicate, err
	}
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return nil, false, ErrNotFound
	}
	if input.QueueStatus != InputRouting && input.QueueStatus != InputWaitingInput || input.RouteStatus == "" {
		return nil, false, fmt.Errorf("%w：当前没有可撤销的路由意图", ErrInputNotQueued)
	}
	if input.Version != expectedVersion {
		return nil, false, ErrVersionConflict
	}
	event := newEvent("", EventInputRouteIntentRevoked, "api", map[string]any{
		"inputId": inputID, "teamId": teamID, "opId": opID,
		"routeKind": input.RouteStatus, "targetRunId": input.TargetRunID,
		"targetTaskId": input.TargetTaskID, "parentRunId": input.ParentRunID,
	})
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":revoke-intent:" + opID
	if err := l.commitLocked(event, func() { l.applyInputEvent(event) }); err != nil {
		return nil, false, err
	}
	return input.Clone(), false, nil
}

// MarkRouted 确认路由完成（routing → routed）：runID/taskID 落账并同批追加
// work_item_linked（WorkItem 是需求到 Run/Task 的索引）。幂等：同 runID 的
// routed 重放跳过。
func (l *InputLedger) MarkRouted(teamID, inputID, kind, runID, taskID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return ErrNotFound
	}
	if input.QueueStatus == InputRouted && containsString(input.RunIDs, runID) {
		return nil // 已确认（崩溃恢复重放）
	}
	if input.QueueStatus != InputRouting && input.QueueStatus != InputWaitingInput &&
		!(kind == "planned" && input.QueueStatus == InputRouted) {
		return fmt.Errorf("%w：当前 %s", ErrInputNotQueued, input.QueueStatus)
	}
	matches := input.RouteStatus == kind && input.TargetRunID == runID && input.TargetTaskID == taskID
	if kind == "planned" && !matches {
		for _, target := range plannedRouteTargets(input) {
			if target.runID == runID && target.taskID == taskID {
				matches = true
				break
			}
		}
	}
	if !matches {
		return errors.New("路由确认与已持久化意图不一致")
	}
	routed := newEvent("", EventInputRouted, "router", map[string]any{
		"inputId": inputID, "teamId": teamID, "routeKind": kind,
		"runId": runID, "taskId": taskID,
		"sourceRevision": jsoncompat.Num(input.Revision),
	})
	routed.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":routed:" + runID
	if len(input.WorkItems) > 0 {
		return l.commitLocked(routed, func() { l.applyInputEvent(routed) })
	}
	workItem := WorkItem{
		ID: "wi-" + inputID, SourceInputID: inputID, SourceRevision: input.Revision,
		RequirementText: InstructionsHead(input.Text), RunID: runID,
		TaskIDs: []string{taskID}, Status: "linked",
	}
	linked := newEvent("", EventWorkItemLinked, "router", map[string]any{
		"inputId": inputID, "teamId": teamID, "workItemId": workItem.ID,
		"sourceRevision":  jsoncompat.Num(workItem.SourceRevision),
		"requirementHead": workItem.RequirementText, "runId": runID,
		"taskIds": []any{taskID}, "status": workItem.Status,
	})
	linked.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":workitem:" + workItem.ID
	// input_routed 与 work_item_linked 原子一批（journal 连续两条、快照一次）。
	return l.commitTwoLocked(routed, linked, func() {
		l.applyInputEvent(routed)
		l.applyInputEvent(linked)
	})
}

// commitTwoLocked 是双事件版 commitLocked（input_routed 与 work_item_linked
// 原子一批：journal 连续两条，内存应用，快照一次）。
func (l *InputLedger) commitTwoLocked(first, second *Event, apply func()) error {
	if l.journal != nil {
		if err := l.journal.Append(first); err != nil {
			return fmt.Errorf("输入事件落账失败: %w", err)
		}
		if err := l.journal.Append(second); err != nil {
			return fmt.Errorf("输入事件落账失败: %w", err)
		}
	}
	apply()
	if err := l.saveSnapshotLocked(); err != nil {
		log.Printf("team-inputs.json 快照写盘失败（下次变更重写，journal 已有事实）: %v", err)
	}
	return nil
}

// MarkWaitingInput 停靠等待用户（queued/routing → waiting_input）：reason 记
// 账供 UI/API 提示。同因重放幂等跳过；原因变化时更新。
func (l *InputLedger) MarkWaitingInput(teamID, inputID, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	input := l.inputs[inputID]
	if input == nil || input.TeamID != teamID {
		return ErrNotFound
	}
	if input.QueueStatus == InputWaitingInput && input.WaitingReason == reason {
		return nil
	}
	if input.QueueStatus == InputRouted || input.QueueStatus == InputCancelled {
		return nil // 终态不回退
	}
	event := newEvent("", EventInputWaitingInput, "router", map[string]any{
		"inputId": inputID, "teamId": teamID, "reason": reason,
	})
	event.IdempotencyKey = "team:" + teamID + ":input:" + inputID + ":waiting:" + reason
	return l.commitLocked(event, func() {
		l.applyInputEvent(event)
	})
}

// RouteOrderAt 返回按公平策略排序的待路由输入（阶段 3，§5）：优先输入在
// 前、各自按接受序；普通输入等待超过 routerAgingAfter 视同优先（反饥饿）；
// 连续 routerPriorityQuota 条优先输入已 routed 后，最早的普通输入强制插队
// （「每处理 3 条优先输入至少处理 1 条普通输入」）。排序只依赖账本状态——
// 重启后同一状态得到同一顺序（§5「所有排序决定落事件，重启后顺序相同」：
// 状态本身即事件投影）。只含 queued/routing/waiting_input。
func (l *InputLedger) RouteOrderAt(teamID string, now time.Time) []*TeamInput {
	l.mu.Lock()
	defer l.mu.Unlock()
	priorityOrder := make([]string, 0, 8)
	normalOrder := make([]string, 0, 8)
	for _, key := range l.order {
		input := l.inputs[key]
		if input == nil || input.TeamID != teamID {
			continue
		}
		switch input.QueueStatus {
		case InputQueued, InputRouting, InputWaitingInput:
		default:
			continue
		}
		if input.Priority || now.Sub(input.AcceptedAt) > routerAgingAfter {
			priorityOrder = append(priorityOrder, key)
		} else {
			normalOrder = append(normalOrder, key)
		}
	}
	// 配额：最近 routerPriorityQuota 条 routed 输入全为优先时，最早普通输入
	// 提到队首（按 UpdatedAt 倒序取最近 routed）。
	if len(normalOrder) > 0 {
		routed := make([]string, 0, routerPriorityQuota)
		for index := len(l.order) - 1; index >= 0 && len(routed) < routerPriorityQuota; index-- {
			input := l.inputs[l.order[index]]
			if input == nil || input.TeamID != teamID || input.QueueStatus != InputRouted {
				continue
			}
			routed = append(routed, l.order[index])
		}
		allPriority := len(routed) == routerPriorityQuota
		for _, key := range routed {
			if !l.inputs[key].Priority && now.Sub(l.inputs[key].AcceptedAt) <= routerAgingAfter {
				allPriority = false // 该优先位来自老化提升，不算连续插队
				break
			}
		}
		if allPriority {
			priorityOrder = append([]string{normalOrder[0]}, priorityOrder...)
			normalOrder = normalOrder[1:]
		}
	}
	rows := make([]*TeamInput, 0, len(priorityOrder)+len(normalOrder))
	for _, key := range append(priorityOrder, normalOrder...) {
		rows = append(rows, l.inputs[key].Clone())
	}
	return rows
}

// 追加失败时不改内存（事实未落，绝不静默丢事件）；快照失败只警告（下次
// 变更重写，重放也能补齐）。
func (l *InputLedger) commitLocked(event *Event, apply func()) error {
	if l.journal != nil {
		if err := l.journal.Append(event); err != nil {
			return fmt.Errorf("输入事件落账失败: %w", err)
		}
	}
	apply()
	if err := l.saveSnapshotLocked(); err != nil {
		log.Printf("team-inputs.json 快照写盘失败（下次变更重写，journal 已有事实）: %v", err)
	}
	return nil
}

// saveSnapshotLocked 把全量输入写为 team-inputs.json（原子替换）。
func (l *InputLedger) saveSnapshotLocked() error {
	object := jsoncompat.NewObject()
	for _, key := range l.order {
		if input := l.inputs[key]; input != nil {
			raw, err := json.Marshal(input)
			if err != nil {
				return err
			}
			value, err := jsoncompat.Unmarshal(raw)
			if err != nil {
				return err
			}
			object.Set(key, value)
		}
	}
	issues := jsoncompat.NewObject()
	for _, key := range l.issuesOrder {
		if issue := l.issues[key]; issue != nil {
			raw, err := json.Marshal(issue)
			if err != nil {
				return err
			}
			value, err := jsoncompat.Unmarshal(raw)
			if err != nil {
				return err
			}
			issues.Set(key, value)
		}
	}
	object.Set("_issues", issues)
	issueGoals := jsoncompat.NewObject()
	for _, key := range sortedGoalIDs(l.issueGoals) {
		value, err := issueGoalSnapshotValue(l.issueGoals[key])
		if err != nil {
			return err
		}
		issueGoals.Set(key, value)
	}
	object.Set("_issueGoals", issueGoals)
	deleted := jsoncompat.NewObject()
	for id, teamID := range l.deleted {
		deleted.Set(id, teamID)
	}
	object.Set("_deletedInputs", deleted)
	return atomicWriteFile(l.snapshotPath, append(jsoncompat.EncodePythonIndent2(object), '\n'))
}

// applyInputEvent 把一条输入事件幂等应用到内存投影（重放与在线同源）：
// 已体现的事实（已存在的 accepted、同 revision 的修订、已 priority、已
// cancelled）跳过，保证「快照 + 全量重放」收敛到同一状态。
func (l *InputLedger) applyInputEvent(event *Event) {
	if event == nil {
		return
	}
	switch event.Type {
	case EventInputAccepted:
		inputID := payloadString(event.Payload, "inputId")
		teamID := payloadString(event.Payload, "teamId")
		if inputID == "" || teamID == "" {
			return
		}
		if sequence := uint64(payloadInt(event.Payload, "sequence")); sequence > l.teamsSeq[teamID] {
			l.teamsSeq[teamID] = sequence
		}
		if l.deleted[inputID] == teamID {
			return
		}
		if existing := l.inputs[inputID]; existing != nil {
			return // 已体现（快照先行）；同 ID 异内容由 Accept 在线校验拦截
		}
		attachments := payloadStrings(event.Payload, "attachmentIds")
		sequence := uint64(payloadInt(event.Payload, "sequence"))
		text := payloadString(event.Payload, "text")
		hash := payloadString(event.Payload, "contentHash")
		now := event.CreatedAt
		input := &TeamInput{
			ID: inputID, TeamID: teamID, Sequence: sequence,
			Revision: 1, Text: text, AttachmentIDs: attachments, ContentHash: hash,
			AcceptedAt: now, QueueStatus: InputQueued,
			AssociationStatus: IssueAssociationUnclassified,
			IssueRouting:      payloadBool(event.Payload, "issueRouting"),
			PolicyDigest:      payloadString(event.Payload, "policyDigest"), Version: 1,
			Revisions: []InputRevision{{
				Revision: 1, Text: text, AttachmentIDs: attachments,
				ContentHash: hash, CreatedAt: now,
			}},
			UpdatedAt: now,
		}
		if input.IssueRouting {
			input.AssociationStatus = IssueAssociationPending
		}
		if input.Historical = payloadBool(event.Payload, "historical"); input.Historical {
			input.QueueStatus = InputRouted
			input.RouteStatus = "historical"
			input.HistoricalSourceRunID = payloadString(event.Payload, "historicalSourceRunId")
			input.HistoricalSourceTaskID = payloadString(event.Payload, "historicalSourceTaskId")
			if at := payloadString(event.Payload, "historicalAt"); at != "" {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, at); parseErr == nil {
					input.AcceptedAt = parsed
				}
			}
			if sourceRunID := payloadString(event.Payload, "sourceRunId"); sourceRunID != "" {
				input.TargetRunID = sourceRunID
				input.RunIDs = []string{sourceRunID}
			}
		}
		l.inputs[inputID] = input
		l.order = append(l.order, inputID)
		if sequence > l.teamsSeq[teamID] {
			l.teamsSeq[teamID] = sequence
		}
	case EventInputRevised:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if input == nil {
			log.Printf("输入事件重放：input_revised 找不到输入 %s，已跳过", inputID)
			return
		}
		// 操作键先登记再去重：快照已体现修订时下方提前返回，不补这里会让
		// 重启后的操作重放被当成新操作（版本冲突而非幂等回放）。
		if opID := payloadString(event.Payload, "opId"); opID != "" {
			l.ops[opID] = inputID
		}
		revision := payloadInt(event.Payload, "revision")
		for i := range input.Revisions {
			if input.Revisions[i].Revision == revision {
				return // 已体现
			}
		}
		attachments := payloadStrings(event.Payload, "attachmentIds")
		text := payloadString(event.Payload, "text")
		hash := payloadString(event.Payload, "contentHash")
		for i := range input.Revisions {
			if input.Revisions[i].ReplacedBy == 0 {
				input.Revisions[i].ReplacedBy = revision
			}
		}
		input.Revisions = append(input.Revisions, InputRevision{
			Revision: revision, Text: text, AttachmentIDs: attachments,
			ContentHash: hash, CreatedAt: event.CreatedAt,
		})
		input.Revision = revision
		input.Text = text
		input.AttachmentIDs = attachments
		input.ContentHash = hash
		// 已完成归类的修订保留提案与链接；待归类或失败输入仍可借修订重试。
		if input.AssociationProposalID == "" {
			input.AssociationStatus = IssueAssociationUnclassified
			if input.IssueRouting {
				input.AssociationStatus = IssueAssociationPending
			}
			input.AssociationReason = ""
			input.IssueLinks = nil
		}
		input.Version++
		input.UpdatedAt = event.CreatedAt
		if opID := payloadString(event.Payload, "opId"); opID != "" {
			l.ops[opID] = inputID
		}
	case EventInputPrioritized:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if opID := payloadString(event.Payload, "opId"); opID != "" {
			l.ops[opID] = inputID // 先登记再去重（input_revised 同理）
		}
		if input == nil || input.Priority {
			return
		}
		input.Priority = true
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventInputCancelled:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if opID := payloadString(event.Payload, "opId"); opID != "" {
			l.ops[opID] = inputID // 先登记再去重（input_revised 同理）
		}
		if input == nil || input.QueueStatus == InputCancelled {
			return
		}
		input.QueueStatus = InputCancelled
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventInputRouteIntent:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if input == nil {
			return
		}
		kind := payloadString(event.Payload, "routeKind")
		runID := payloadString(event.Payload, "targetRunId")
		taskID := payloadString(event.Payload, "targetTaskId")
		parentRunID := payloadString(event.Payload, "parentRunId")
		if (input.QueueStatus == InputRouting || input.QueueStatus == InputWaitingInput) &&
			input.RouteStatus == kind && input.TargetRunID == runID &&
			input.TargetTaskID == taskID && input.ParentRunID == parentRunID &&
			input.SourceTaskID == payloadString(event.Payload, "sourceTaskId") {
			return // 同意图重放
		}
		if input.QueueStatus == InputRouted || input.QueueStatus == InputCancelled {
			return // 终态不回退
		}
		input.QueueStatus = InputRouting
		input.RouteStatus = kind
		input.TargetRunID = runID
		input.TargetTaskID = taskID
		input.SourceTaskID = payloadString(event.Payload, "sourceTaskId")
		input.ParentRunID = parentRunID
		input.WaitingReason = ""
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventInputRouteIntentRevoked:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if opID := payloadString(event.Payload, "opId"); opID != "" {
			l.ops[opID] = inputID
		}
		if input == nil || input.QueueStatus == InputRouted || input.QueueStatus == InputCancelled {
			return
		}
		if input.QueueStatus == InputQueued && input.RouteStatus == "" {
			return
		}
		input.QueueStatus = InputQueued
		input.RouteStatus = ""
		input.TargetRunID = ""
		input.TargetTaskID = ""
		input.SourceTaskID = ""
		input.ParentRunID = ""
		input.ExplicitTarget = false
		input.WorkItems = nil
		input.WorkItemIDs = nil
		input.WaitingReason = ""
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventInputRouted:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if input == nil {
			return
		}
		runID := payloadString(event.Payload, "runId")
		if input.QueueStatus == InputRouted && containsString(input.RunIDs, runID) {
			return // 已确认
		}
		input.QueueStatus = InputRouted
		input.RouteStatus = payloadString(event.Payload, "routeKind")
		if !containsString(input.RunIDs, runID) {
			input.RunIDs = append(input.RunIDs, runID)
		}
		if taskID := payloadString(event.Payload, "taskId"); taskID != "" {
			input.TargetTaskID = taskID
		}
		if runID != "" {
			input.TargetRunID = runID
		}
		for index := range input.WorkItems {
			if input.WorkItems[index].RunID == runID {
				input.WorkItems[index].Status = "linked"
				input.WorkItems[index].EvidenceRef = runID + "#" + payloadString(event.Payload, "taskId")
			}
		}
		input.WaitingReason = ""
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventInputWaitingInput:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if input == nil || input.QueueStatus == InputRouted || input.QueueStatus == InputCancelled {
			return
		}
		reason := payloadString(event.Payload, "reason")
		if input.QueueStatus == InputWaitingInput && input.WaitingReason == reason {
			return
		}
		input.QueueStatus = InputWaitingInput
		input.WaitingReason = reason
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventWorkItemsProposed:
		l.applyWorkItemsProposed(event)
	case EventWorkItemLinked:
		inputID := payloadString(event.Payload, "inputId")
		input := l.inputs[inputID]
		if input == nil {
			return
		}
		workItemID := payloadString(event.Payload, "workItemId")
		for i := range input.WorkItems {
			if input.WorkItems[i].ID == workItemID {
				return // 已链接
			}
		}
		input.WorkItems = append(input.WorkItems, WorkItem{
			ID: workItemID, SourceInputID: inputID,
			SourceRevision:  payloadInt(event.Payload, "sourceRevision"),
			RequirementText: payloadString(event.Payload, "requirementHead"),
			RunID:           payloadString(event.Payload, "runId"),
			TaskIDs:         payloadStrings(event.Payload, "taskIds"),
			Status:          payloadString(event.Payload, "status"),
		})
		if !containsString(input.WorkItemIDs, workItemID) {
			input.WorkItemIDs = append(input.WorkItemIDs, workItemID)
		}
		input.Version++
		input.UpdatedAt = event.CreatedAt
	case EventIssueProposalApplied:
		l.applyIssueProposalEvent(event)
	case EventIssueAssociationFailed:
		l.applyIssueAssociationFailedEvent(event)
	case EventGoalReviewed, EventIssueClosed:
		l.applyIssueGoalEvent(event)
	case EventIssueDeleted:
		l.applyIssueDeletedEvent(event)
	case EventInputDeleted:
		id, teamID := payloadString(event.Payload, "inputId"), payloadString(event.Payload, "teamId")
		if id != "" && teamID != "" {
			l.deleted[id] = teamID
			if input := l.inputs[id]; input != nil && input.TeamID == teamID {
				delete(l.inputs, id)
			}
			l.order = removeMissing(l.order, l.inputs)
		}
	case EventInputTeamPurged:
		teamID := payloadString(event.Payload, "teamId")
		kept := l.order[:0]
		for _, key := range l.order {
			if input := l.inputs[key]; input != nil && input.TeamID == teamID {
				delete(l.inputs, key)
				continue
			}
			kept = append(kept, key)
		}
		l.order = kept
		keptIssues := l.issuesOrder[:0]
		for _, key := range l.issuesOrder {
			if issue := l.issues[key]; issue != nil && issue.TeamID == teamID {
				delete(l.issues, key)
				delete(l.issueGoals, key)
				continue
			}
			keptIssues = append(keptIssues, key)
		}
		l.issuesOrder = keptIssues
	default:
		// 阶段 2 的路由事件（input_route_intent/input_routed/
		// input_waiting_input/work_item_linked）与未知事件：只增不删的
		// 前向兼容——先记日志不崩溃，投影语义随阶段 2 落地。
		if event.Type != "" {
			log.Printf("输入事件重放：暂不支持的事件类型 %s，已跳过", event.Type)
		}
	}
}
