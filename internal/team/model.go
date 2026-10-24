// Package team 提供多 Agent 编排的核心对象与持久化（docs/team-orchestration-plan.md
// §4、§8）：Team/Run/Task 模型、teams.json / team-runs.json 原子账本（乐观版本，
// §8.4）、追加式 team-journal.jsonl 运行事实（§8.1），以及每 Run 的事件投影
// （§10.3）。崩溃一致性优先于功能广度：账本写入为原子替换且落盘成功后才翻内存
// 值；journal 以单调序号追加，重放拒绝重复/回退序号；Team/Run/Task/TaskResult
// 保留未知字段，未来版本的账本可以无损读回。
package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// 任务状态（§4.3 状态图）。outcome_unknown 表示外部副作用是否发生尚未确认，
// 禁止自动重发；cancelled/stopped 可通过对账或恢复继续。
const (
	TaskPlanned        = "planned"
	TaskReady          = "ready"
	TaskDispatching    = "dispatching"
	TaskRunning        = "running"
	TaskCompleted      = "completed"
	TaskFailed         = "failed"
	TaskCancelled      = "cancelled"
	TaskOutcomeUnknown = "outcome_unknown"
	TaskStopped        = "stopped"
)

// Run 状态（§4.3 状态图）。planning 由阶段三 Lead 自动规划使用，阶段一显式
// Plan 不经过该状态。
const (
	RunCreated        = "created"
	RunPlanning       = "planning"
	RunDispatching    = "dispatching"
	RunCollecting     = "collecting"
	RunReducing       = "reducing"
	RunCompleted      = "completed"
	RunWaitingInput   = "waiting_input"
	RunFailed         = "failed"
	RunCancelled      = "cancelled"
	RunStopped        = "stopped"
	RunOutcomeUnknown = "outcome_unknown"
)

// 事件类型（§10.3 的阶段一子集）。task_cache_hit / action_* / question_* 等
// 留给后续阶段。
const (
	EventRunCreated            = "run_created"
	EventTaskPlanned           = "task_planned"
	EventTaskDispatching       = "task_dispatching"
	EventTaskRunning           = "task_running"
	EventTaskCompleted         = "task_completed"
	EventTaskFailed            = "task_failed"
	EventTaskOutcomeUnknown    = "task_outcome_unknown"
	EventTaskStopped           = "task_stopped"
	EventTaskInputHashMismatch = "task_input_hash_mismatch"
	EventRunWaitingInput       = "run_waiting_input"
	EventRunCompleted          = "run_completed"
	EventRunFailed             = "run_failed"
	EventRunCancelled          = "run_cancelled"
)

// 策略取值（§4.2）。isolated_worktree 属阶段二，仅作为可持久化的取值存在。
const (
	WorkspaceShared           = "shared_workspace"
	WorkspaceIsolatedWorktree = "isolated_worktree"
	WritePolicyExclusive      = "exclusive"
	WritePolicyParallel       = "parallel"
)

// 成员角色与不可用标记（§4.1：成员引用失效时保留记录并标记 unavailable）。
const (
	RoleLead     = "lead"
	RoleMember   = "member"
	RoleReviewer = "reviewer"

	MemberUnavailable = "unavailable"
)

// memberIDPattern 约束成员 id：字母/数字/下划线/连字符，1-64 字符。
var memberIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidMemberID 报告 id 是否可作为 TeamMember.ID。
func ValidMemberID(id string) bool { return memberIDPattern.MatchString(id) }

// instructionsHeadRunes 是任务卡摘要的截断长度（§11.4：Prompt 前 240 字符）。
const instructionsHeadRunes = 240

// InstructionsHead 截取 Prompt 前 240 个 rune 作为任务卡摘要。
func InstructionsHead(prompt string) string {
	count := 0
	for index := range prompt {
		if count == instructionsHeadRunes {
			return prompt[:index]
		}
		count++
	}
	return prompt
}

// Team 是一个多 Agent 编排单元（§4.1）。extra 保留账本中的未知字段。
type Team struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Members   []TeamMember `json:"members"`
	LeadID    string       `json:"leadId,omitempty"`
	Policy    TeamPolicy   `json:"policy"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Version   uint64       `json:"version"`

	extra *jsoncompat.Object
}

// TeamMember 引用一个既有会话（§4.1）。
type TeamMember struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	AgentKind string `json:"agentKind"`
	Name      string `json:"name"`
	Role      string `json:"role"` // lead | member | reviewer
	Enabled   bool   `json:"enabled"`
}

// TeamPolicy 是编排策略（§4.2）。零值不可直接使用，请用 DefaultPolicy。
type TeamPolicy struct {
	MaxParallelTasks       int    `json:"maxParallelTasks"`
	MinParallelTasks       int    `json:"minParallelTasks"`
	MaxProviderConcurrency int    `json:"maxProviderConcurrency"`
	MaxRounds              int    `json:"maxRounds"`
	MaxTasksPerRun         int    `json:"maxTasksPerRun"`
	MaxContextChars        int    `json:"maxContextChars"`
	MaxResultChars         int    `json:"maxResultChars"`
	TaskTimeoutSecs        int    `json:"taskTimeoutSecs"`
	RunTimeoutSecs         int    `json:"runTimeoutSecs"`
	BudgetUnits            int    `json:"budgetUnits"`
	WorkspaceMode          string `json:"workspaceMode"` // shared_workspace | isolated_worktree
	WritePolicy            string `json:"writePolicy"`   // exclusive | parallel
	AutoDispatch           bool   `json:"autoDispatch"`
	AutoApplyChanges       bool   `json:"autoApplyChanges"`
}

// DefaultPolicy 返回 §4.2 的默认策略。
func DefaultPolicy() TeamPolicy {
	return TeamPolicy{
		MaxParallelTasks:       3,
		MinParallelTasks:       1,
		MaxProviderConcurrency: 5,
		MaxRounds:              3,
		MaxTasksPerRun:         12,
		MaxContextChars:        12000,
		MaxResultChars:         12000,
		TaskTimeoutSecs:        1800,
		RunTimeoutSecs:         7200,
		BudgetUnits:            0, // 0 表示只统计，不主动暂停
		WorkspaceMode:          WorkspaceShared,
		WritePolicy:            WritePolicyExclusive,
		AutoDispatch:           false,
		AutoApplyChanges:       false,
	}
}

// Run 是一次编排执行（§4.3）。team-runs.json 是可重建的查询快照，
// 运行事实以 team-journal.jsonl 为准。
type Run struct {
	ID             string    `json:"id"`
	TeamID         string    `json:"teamId"`
	Goal           string    `json:"goal"`
	Status         string    `json:"status"`
	StopReason     string    `json:"stopReason,omitempty"`
	Round          int       `json:"round"`
	PlanHash       string    `json:"planHash"`
	Tasks          []Task    `json:"tasks"`
	PendingActions []Action  `json:"pendingActions,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Version        uint64    `json:"version"`

	extra *jsoncompat.Object
}

// Task 是 Run 内的一个执行单元（§4.3）。Result 是结果文本本体；
// ResultDetail 携带 §6.5 要求的引用字段（sessionId/turnId/…），随 Run
// 持久化，供 salvage 快照与重启对账使用。
type Task struct {
	ID               string      `json:"id"`
	MemberID         string      `json:"memberId"`
	Prompt           string      `json:"prompt"`
	InputHash        string      `json:"inputHash"`
	Attempt          int         `json:"attempt"`
	SiteID           string      `json:"siteId,omitempty"`
	Ordinal          int         `json:"ordinal,omitempty"`
	DependsOn        []string    `json:"dependsOn,omitempty"`
	WorkspaceMode    string      `json:"workspaceMode,omitempty"`
	WorkspaceID      string      `json:"workspaceId,omitempty"`
	WriteLock        string      `json:"writeLock,omitempty"`
	Status           string      `json:"status"`
	SubmissionID     string      `json:"submissionId,omitempty"`
	Result           string      `json:"result,omitempty"`
	ResultDetail     *TaskResult `json:"resultDetail,omitempty"`
	Error            string      `json:"error,omitempty"`
	Outcome          string      `json:"outcome,omitempty"`
	StopReason       string      `json:"stopReason,omitempty"`
	InstructionsHead string      `json:"instructionsHead,omitempty"`

	extra *jsoncompat.Object
}

// TaskResult 是任务结果的结构化引用（§6.5）：每条结果必须带 sessionId、
// turnId、resultChars 与 truncated 标记，跨网关重启存活。
type TaskResult struct {
	SessionID   string `json:"sessionId"`
	TurnID      string `json:"turnId"`
	Result      string `json:"result"`
	ResultChars int    `json:"resultChars"`
	Truncated   bool   `json:"truncated"`
	StopReason  string `json:"stopReason,omitempty"`

	extra *jsoncompat.Object
}

// Action 是 Lead 提出的待确认动作（§5.2）。阶段一只持久化与回显，
// 解析与执行在后续阶段实现。
type Action struct {
	Type         string `json:"type"`
	SourceTaskID string `json:"sourceTaskId"`
	Round        int    `json:"round"`
	Member       string `json:"member,omitempty"`
	Task         string `json:"task,omitempty"`
}

// Event 是写入 journal 的运行事实（§4.3）。Sequence 由 Journal.Append
// 统一编号，调用方无需自行赋值。
type Event struct {
	ID             string         `json:"id"`
	RunID          string         `json:"runId"`
	Type           string         `json:"type"`
	Source         string         `json:"source"`
	Payload        map[string]any `json:"payload"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	Sequence       uint64         `json:"sequence"`
	CreatedAt      time.Time      `json:"createdAt"`
}

// ---- 未知字段保留（§8.1）----
//
// Team/Run/Task/TaskResult 自定义 UnmarshalJSON：已知字段逐个解码，
// 多余键存入 extra（保持文档顺序与数字字面量），Marshal 时原样回写。

var teamKnownFields = map[string]bool{
	"id": true, "name": true, "members": true, "leadId": true,
	"policy": true, "createdAt": true, "updatedAt": true, "version": true,
}

var runKnownFields = map[string]bool{
	"id": true, "teamId": true, "goal": true, "status": true,
	"stopReason": true, "round": true, "planHash": true, "tasks": true,
	"pendingActions": true, "createdAt": true, "updatedAt": true, "version": true,
}

var taskKnownFields = map[string]bool{
	"id": true, "memberId": true, "prompt": true, "inputHash": true,
	"attempt": true, "siteId": true, "ordinal": true, "dependsOn": true,
	"workspaceMode": true, "workspaceId": true, "writeLock": true,
	"status": true, "submissionId": true, "result": true, "resultDetail": true,
	"error": true, "outcome": true, "stopReason": true, "instructionsHead": true,
}

var taskResultKnownFields = map[string]bool{
	"sessionId": true, "turnId": true, "result": true,
	"resultChars": true, "truncated": true, "stopReason": true,
}

// UnmarshalJSON 解码 Team 并保留未知字段。
func (t *Team) UnmarshalJSON(data []byte) error {
	type teamAlias Team
	var decoded teamAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, teamKnownFields)
	if err != nil {
		return err
	}
	*t = Team(decoded)
	t.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (t *Team) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(t.jsonObject()), nil
}

func (t *Team) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", t.ID)
	object.Set("name", t.Name)
	members := &jsoncompat.Array{}
	for i := range t.Members {
		members.Items = append(members.Items, t.Members[i].jsonObject())
	}
	object.Set("members", members)
	if t.LeadID != "" {
		object.Set("leadId", t.LeadID)
	}
	object.Set("policy", t.Policy.jsonObject())
	object.Set("createdAt", t.CreatedAt.Format(time.RFC3339Nano))
	object.Set("updatedAt", t.UpdatedAt.Format(time.RFC3339Nano))
	object.Set("version", json.Number(strconv.FormatUint(t.Version, 10)))
	mergeExtras(object, t.extra)
	return object
}

func (m *TeamMember) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", m.ID)
	object.Set("sessionId", m.SessionID)
	object.Set("agentKind", m.AgentKind)
	object.Set("name", m.Name)
	object.Set("role", m.Role)
	object.Set("enabled", m.Enabled)
	return object
}

func (p *TeamPolicy) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("maxParallelTasks", jsoncompat.Num(p.MaxParallelTasks))
	object.Set("minParallelTasks", jsoncompat.Num(p.MinParallelTasks))
	object.Set("maxProviderConcurrency", jsoncompat.Num(p.MaxProviderConcurrency))
	object.Set("maxRounds", jsoncompat.Num(p.MaxRounds))
	object.Set("maxTasksPerRun", jsoncompat.Num(p.MaxTasksPerRun))
	object.Set("maxContextChars", jsoncompat.Num(p.MaxContextChars))
	object.Set("maxResultChars", jsoncompat.Num(p.MaxResultChars))
	object.Set("taskTimeoutSecs", jsoncompat.Num(p.TaskTimeoutSecs))
	object.Set("runTimeoutSecs", jsoncompat.Num(p.RunTimeoutSecs))
	object.Set("budgetUnits", jsoncompat.Num(p.BudgetUnits))
	object.Set("workspaceMode", p.WorkspaceMode)
	object.Set("writePolicy", p.WritePolicy)
	object.Set("autoDispatch", p.AutoDispatch)
	object.Set("autoApplyChanges", p.AutoApplyChanges)
	return object
}

// Clone 深拷贝 Team（含未知字段），账本入库与出库都返回副本。
func (t *Team) Clone() *Team {
	if t == nil {
		return nil
	}
	copied := *t
	copied.Members = append([]TeamMember(nil), t.Members...)
	copied.extra = cloneExtra(t.extra)
	return &copied
}

// UnmarshalJSON 解码 Run 并保留未知字段。
func (r *Run) UnmarshalJSON(data []byte) error {
	type runAlias Run
	var decoded runAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, runKnownFields)
	if err != nil {
		return err
	}
	*r = Run(decoded)
	r.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (r *Run) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(r.jsonObject()), nil
}

func (r *Run) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", r.ID)
	object.Set("teamId", r.TeamID)
	object.Set("goal", r.Goal)
	object.Set("status", r.Status)
	if r.StopReason != "" {
		object.Set("stopReason", r.StopReason)
	}
	object.Set("round", jsoncompat.Num(r.Round))
	object.Set("planHash", r.PlanHash)
	tasks := &jsoncompat.Array{}
	for i := range r.Tasks {
		tasks.Items = append(tasks.Items, r.Tasks[i].jsonObject())
	}
	object.Set("tasks", tasks)
	if len(r.PendingActions) > 0 {
		actions := &jsoncompat.Array{}
		for i := range r.PendingActions {
			actions.Items = append(actions.Items, r.PendingActions[i].jsonObject())
		}
		object.Set("pendingActions", actions)
	}
	object.Set("createdAt", r.CreatedAt.Format(time.RFC3339Nano))
	object.Set("updatedAt", r.UpdatedAt.Format(time.RFC3339Nano))
	object.Set("version", json.Number(strconv.FormatUint(r.Version, 10)))
	mergeExtras(object, r.extra)
	return object
}

func (a *Action) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("type", a.Type)
	object.Set("sourceTaskId", a.SourceTaskID)
	object.Set("round", jsoncompat.Num(a.Round))
	if a.Member != "" {
		object.Set("member", a.Member)
	}
	if a.Task != "" {
		object.Set("task", a.Task)
	}
	return object
}

// Clone 深拷贝 Run（含任务、动作与未知字段）。
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	copied := *r
	copied.Tasks = nil
	if len(r.Tasks) > 0 {
		copied.Tasks = make([]Task, len(r.Tasks))
		for i := range r.Tasks {
			copied.Tasks[i] = *r.Tasks[i].Clone()
		}
	}
	copied.PendingActions = append([]Action(nil), r.PendingActions...)
	copied.extra = cloneExtra(r.extra)
	return &copied
}

// UnmarshalJSON 解码 Task 并保留未知字段。
func (t *Task) UnmarshalJSON(data []byte) error {
	type taskAlias Task
	var decoded taskAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, taskKnownFields)
	if err != nil {
		return err
	}
	*t = Task(decoded)
	t.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (t *Task) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(t.jsonObject()), nil
}

func (t *Task) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", t.ID)
	object.Set("memberId", t.MemberID)
	object.Set("prompt", t.Prompt)
	object.Set("inputHash", t.InputHash)
	object.Set("attempt", jsoncompat.Num(t.Attempt))
	if t.SiteID != "" {
		object.Set("siteId", t.SiteID)
	}
	if t.Ordinal != 0 {
		object.Set("ordinal", jsoncompat.Num(t.Ordinal))
	}
	if len(t.DependsOn) > 0 {
		depends := &jsoncompat.Array{}
		for _, id := range t.DependsOn {
			depends.Items = append(depends.Items, id)
		}
		object.Set("dependsOn", depends)
	}
	if t.WorkspaceMode != "" {
		object.Set("workspaceMode", t.WorkspaceMode)
	}
	if t.WorkspaceID != "" {
		object.Set("workspaceId", t.WorkspaceID)
	}
	if t.WriteLock != "" {
		object.Set("writeLock", t.WriteLock)
	}
	object.Set("status", t.Status)
	if t.SubmissionID != "" {
		object.Set("submissionId", t.SubmissionID)
	}
	if t.Result != "" {
		object.Set("result", t.Result)
	}
	if t.ResultDetail != nil {
		object.Set("resultDetail", t.ResultDetail.jsonObject())
	}
	if t.Error != "" {
		object.Set("error", t.Error)
	}
	if t.Outcome != "" {
		object.Set("outcome", t.Outcome)
	}
	if t.StopReason != "" {
		object.Set("stopReason", t.StopReason)
	}
	if t.InstructionsHead != "" {
		object.Set("instructionsHead", t.InstructionsHead)
	}
	mergeExtras(object, t.extra)
	return object
}

// Clone 深拷贝 Task（含依赖、结果引用与未知字段）。
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	copied := *t
	copied.DependsOn = append([]string(nil), t.DependsOn...)
	copied.ResultDetail = t.ResultDetail.Clone()
	copied.extra = cloneExtra(t.extra)
	return &copied
}

// UnmarshalJSON 解码 TaskResult 并保留未知字段。
func (tr *TaskResult) UnmarshalJSON(data []byte) error {
	type taskResultAlias TaskResult
	var decoded taskResultAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, taskResultKnownFields)
	if err != nil {
		return err
	}
	*tr = TaskResult(decoded)
	tr.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (tr *TaskResult) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(tr.jsonObject()), nil
}

func (tr *TaskResult) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("sessionId", tr.SessionID)
	object.Set("turnId", tr.TurnID)
	object.Set("result", tr.Result)
	object.Set("resultChars", jsoncompat.Num(tr.ResultChars))
	object.Set("truncated", tr.Truncated)
	if tr.StopReason != "" {
		object.Set("stopReason", tr.StopReason)
	}
	mergeExtras(object, tr.extra)
	return object
}

// Clone 深拷贝 TaskResult（含未知字段）。
func (tr *TaskResult) Clone() *TaskResult {
	if tr == nil {
		return nil
	}
	copied := *tr
	copied.extra = cloneExtra(tr.extra)
	return &copied
}

// UnmarshalJSON 按有序树解码 Event：数字保持字面量，payload 的键序与
// 数值精度无损进入 map。
func (e *Event) UnmarshalJSON(data []byte) error {
	value, err := jsoncompat.Unmarshal(data)
	if err != nil {
		return err
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		return errors.New("event 记录不是 JSON 对象")
	}
	for _, key := range object.Keys() {
		raw, _ := object.Get(key)
		switch key {
		case "id":
			e.ID, _ = raw.(string)
		case "runId":
			e.RunID, _ = raw.(string)
		case "type":
			e.Type, _ = raw.(string)
		case "source":
			e.Source, _ = raw.(string)
		case "payload":
			if raw == nil {
				e.Payload = nil
				continue
			}
			payload, ok := raw.(*jsoncompat.Object)
			if !ok {
				return errors.New("event 的 payload 不是 JSON 对象")
			}
			e.Payload = objectToMap(payload)
		case "idempotencyKey":
			e.IdempotencyKey, _ = raw.(string)
		case "sequence":
			number, ok := raw.(json.Number)
			if !ok {
				return errors.New("event 的 sequence 不是数字")
			}
			sequence, err := strconv.ParseUint(number.String(), 10, 64)
			if err != nil {
				return fmt.Errorf("event 的 sequence 非法: %w", err)
			}
			e.Sequence = sequence
		case "createdAt":
			text, ok := raw.(string)
			if !ok {
				return errors.New("event 的 createdAt 不是字符串")
			}
			createdAt, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return fmt.Errorf("event 的 createdAt 非法: %w", err)
			}
			e.CreatedAt = createdAt
		}
	}
	return nil
}

// MarshalJSON 输出 §4.3 声明顺序的字段；payload 键排序保证字节级确定性。
func (e *Event) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(eventToObject(e)), nil
}

// eventToObject 把 Event 组装为有序 JSON 树（journal 行与事件流行共用）。
func eventToObject(event *Event) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", event.ID)
	object.Set("runId", event.RunID)
	object.Set("type", event.Type)
	object.Set("source", event.Source)
	object.Set("payload", payloadValue(event.Payload))
	if event.IdempotencyKey != "" {
		object.Set("idempotencyKey", event.IdempotencyKey)
	}
	object.Set("sequence", json.Number(strconv.FormatUint(event.Sequence, 10)))
	object.Set("createdAt", event.CreatedAt.Format(time.RFC3339Nano))
	return object
}

// payloadValue 把 map 载荷转为键排序的有序对象（nil 保持 null）。
func payloadValue(payload map[string]any) any {
	if payload == nil {
		return nil
	}
	object := jsoncompat.NewObject()
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object.Set(key, goValue(payload[key]))
	}
	return object
}

// goValue 把任意 Go 值转为 jsoncompat 树：标准标量直通，数值统一成
// json.Number，容器递归；其余类型经 encoding/json 兜底，绝不 panic。
func goValue(value any) any {
	switch v := value.(type) {
	case nil, bool, string, json.Number, *jsoncompat.Object, *jsoncompat.Array:
		return v
	case int:
		return jsoncompat.Num(v)
	case int32:
		return json.Number(strconv.FormatInt(int64(v), 10))
	case int64:
		return json.Number(strconv.FormatInt(v, 10))
	case uint64:
		return json.Number(strconv.FormatUint(v, 10))
	case float64:
		return json.Number(strconv.FormatFloat(v, 'g', -1, 64))
	case map[string]any:
		object := jsoncompat.NewObject()
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			object.Set(key, goValue(v[key]))
		}
		return object
	case []any:
		array := &jsoncompat.Array{}
		for _, item := range v {
			array.Items = append(array.Items, goValue(item))
		}
		return array
	case []string:
		array := &jsoncompat.Array{}
		for _, item := range v {
			array.Items = append(array.Items, item)
		}
		return array
	default:
		if raw, err := json.Marshal(v); err == nil {
			if parsed, parseErr := jsoncompat.Unmarshal(raw); parseErr == nil {
				return parsed
			}
		}
		return nil
	}
}

// objectToMap 把有序树转回 map（数字保持 json.Number 字面量）。
func objectToMap(object *jsoncompat.Object) map[string]any {
	out := make(map[string]any, object.Len())
	for _, key := range object.Keys() {
		value, _ := object.Get(key)
		out[key] = treeToGo(value)
	}
	return out
}

func treeToGo(value any) any {
	switch v := value.(type) {
	case *jsoncompat.Object:
		return objectToMap(v)
	case *jsoncompat.Array:
		items := make([]any, len(v.Items))
		for i, item := range v.Items {
			items[i] = treeToGo(item)
		}
		return items
	default:
		return v
	}
}

// collectExtras 提取文档中不属于 known 的键，保持出现顺序与原始值。
func collectExtras(data []byte, known map[string]bool) (*jsoncompat.Object, error) {
	value, err := jsoncompat.Unmarshal(data)
	if err != nil {
		return nil, err
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		return nil, errors.New("记录不是 JSON 对象")
	}
	var extras *jsoncompat.Object
	for _, key := range object.Keys() {
		if known[key] {
			continue
		}
		if extras == nil {
			extras = jsoncompat.NewObject()
		}
		raw, _ := object.Get(key)
		extras.Set(key, raw)
	}
	return extras, nil
}

func mergeExtras(object *jsoncompat.Object, extra *jsoncompat.Object) {
	if extra == nil {
		return
	}
	for _, key := range extra.Keys() {
		if value, ok := extra.Get(key); ok {
			object.Set(key, value)
		}
	}
}

func cloneExtra(extra *jsoncompat.Object) *jsoncompat.Object {
	if extra == nil {
		return nil
	}
	cloned, ok := jsoncompat.Clone(extra).(*jsoncompat.Object)
	if !ok {
		return nil
	}
	return cloned
}
