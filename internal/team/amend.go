package team

// amend.go 是修订 Run（amend）的创建与多轮缓存导入（§10.2 amend 段、§6.4.1
// 确定性验证闸门与多轮缓存，§12 阶段三工作项 7）：amend 创建新 Run、绝不
// 改写父 Run 的 journal；命中前缀以本 Run 的新节点实例入账（§6.4.1 规则 4：
// 不跳过事件写入，任务在创建批次内直接落 completed + task_cache_hit 事实），
// inputHash 不一致的任务显式拒绝复用、按全新任务执行（§8.2「已有不同
// inputHash：拒绝复用并要求创建修订后的新 Run」的创建期形态）。不搬运
// transcript——下游任务的上下文走既有 BuildTaskContext 的有界摘要注入
//（§6.4.1 规则 5，零新机械：导入任务的 ResultDetail 就在本 Run 快照里）。

import (
	"errors"
	"fmt"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// ErrRunContentMismatch 是创建幂等重放的内容比对失败（同 id 不同内容）：
// HTTP 侧映射 400。handleRunCreate 重放比对的同款语义，amend 的身份比对
// 增 parentRunID 一项（修订链的父 Run 属于请求身份）。
var ErrRunContentMismatch = errors.New("同一创建请求不能用于不同内容")

// TaskImport 是 ImportPrefix 的一条命中：父 Run 任务的确定性结果导入到
// 新 Run 的同名任务槽。ResultDetail 是父任务结果引用的深拷贝（杜绝与
// 父 Run 快照共享内层指针；Result 正文随 TaskResult 一体携带）。
type TaskImport struct {
	TaskID       string      // 新 Run 任务 ID（= 父任务 ID：同 ID 才可导入）
	ParentTaskID string      // 父 Run 任务 ID（与新任务同 ID，事件载荷显式携带）
	InputHash    string      // 命中所依据的创建期规范化 inputHash
	ResultDetail *TaskResult // 父任务结果引用（sessionId/turnId/…，§6.5）
}

// ImportPrefix 计算新计划可从父 Run 零 token 复用的连续前缀（§6.4.1 规则
// 1–3，纯函数：只读父 Run 与新计划，不碰 journal/账本/会话）。按新计划
// 拓扑序逐任务判定，可导入当且仅当：
//
//	(a) 同 TaskID 同 MemberID，且创建期规范化 inputHash 一致——匹配键是
//	    planner 的创建期哈希（InputHashFor(task, PolicyDigest, 空上下文)），
//	    不是快照 InputHash：父任务派发后快照哈希已被「含真实注入上下文」
//	    的派发期哈希覆写（两级哈希分离，dispatcher），父侧必须从创建后
//	    不可变的确定性字段（memberId/prompt/dependsOn/workspaceMode/
//	    writeLock）按当前策略摘要重算。假设声明（v1）：父 Run 创建期的
//	    策略摘要不可从快照恢复，团队策略在此期间的变更不会被检出——两侧
//	    按当前策略同步重算，字段一致即命中；
//	(b) 父任务 completed 且带 ResultDetail（§6.5 结果引用，导入的最小
//	    确定性凭据）；
//	(c) 依赖（映射后）均已导入——拓扑序下即连续前缀：遇首个不可导入的
//	    任务整体停止（v1 简化，不计算依赖闭包）；
//	(d) 排他闸门（§6.4.1 规则 3 创建期形态）：前缀在首个「新计划里未被
//	    导入的 exclusive 任务」处整体停止——该任务将在修订版里写入共享
//	    工作区（或对 isolated worktree 产生文件副作用），其后任务看到的
//	    磁盘状态必然异于父 Run。连续前缀规则 (c) 在创建期已蕴含本闸门
//	    （首个不可导入任务无论写锁形态都断链，闸门是其保守上界）；运行
//	    中的动态二级失效（本 Run 执行写入后关闭后续 amend 对下游结果的
//	    缓存）留阶段四。
//
// inputHash 不一致（同 ID 同成员、内容已修订）是显式拒绝：不导入、不记
// cache_hit，该任务按全新任务执行——绝不静默套用旧结果（§8.2 拒绝复用）。
// 纯摘要任务（writeLock=none）同样只经 (a) 的输入复验后才能复用
// （§6.4.1 规则 3 后半句）。
func ImportPrefix(parent *Run, plan *Plan, policy TeamPolicy) []TaskImport {
	if parent == nil || plan == nil {
		return nil
	}
	parentByID := map[string]*Task{}
	for i := range parent.Tasks {
		parentByID[parent.Tasks[i].ID] = &parent.Tasks[i]
	}
	policyDigest := PolicyDigest(policy)
	imported := map[string]bool{}
	var imports []TaskImport
	for i := range plan.Tasks {
		task := &plan.Tasks[i]
		parentTask := parentByID[task.ID]
		if parentTask == nil || parentTask.MemberID != task.MemberID {
			break // (a) 无同名任务或成员变更：前缀断链。
		}
		if InputHashFor(parentTask, policyDigest, "") != task.InputHash {
			break // (a) inputHash 不一致：显式拒绝复用，按全新任务执行。
		}
		if parentTask.Status != TaskCompleted || parentTask.ResultDetail == nil {
			break // (b) 父任务无确定性结果可导入。
		}
		ready := true
		for _, dep := range task.DependsOn {
			if !imported[dep] {
				ready = false
				break
			}
		}
		if !ready {
			break // (c) 依赖（映射后）未全部导入：下游输入必然异于父 Run。
		}
		imports = append(imports, TaskImport{
			TaskID:       task.ID,
			ParentTaskID: parentTask.ID,
			InputHash:    task.InputHash,
			ResultDetail: parentTask.ResultDetail.Clone(),
		})
		imported[task.ID] = true
	}
	return imports
}

// CreateAmendRun 创建修订 Run（§10.2 amend 段、§6.4.1 多轮缓存；HTTP 侧
// POST /api/teams/:t/runs/:r/amend 的处理入口，httpd 阶段三流接线）。硬
// 边界：创建新 Run、绝不写父 Run 的 journal——父 Run 的事件、快照与版本
// 都不因 amend 改变（测试按「父 Run 的 journal 行数零增」断言）。
//
// autoPlan（§5.1/§10.2，与 CreateRun 同一语义）：plan 为空（single 模式合成
// Lead 任务）即 Lead 自动规划——修订 Run 的 Lead 回复照常解析 team-actions
// （「让 Lead 再规划一轮」的典型修订场景）；携带显式计划则 false，显式 Plan
// 优先。autoPlan 与显式计划互斥（同 CreateRun），随 run_created payload 落账
// 供 journal 重建。
//
// 流程：ParsePlan 全套复用（循环/成员/上限/双口径长度）→ 父 Run 校验
// （存在且属本团队；不要求终态——导入只取 completed 任务，任务终态单调，
// 在途父 Run 与修订 Run 的 exclusive 写锁由同一 LockRegistry 跨 Run 互斥）
// → runID 创建幂等（同 id 同内容 duplicate=true、异内容
// ErrRunContentMismatch；比对=handleRunCreate 全字段 + parentRunID）→
// ImportPrefix 计算可复用前缀 → 单个创建批次落账：run_created（payload
// 增 parentRunId 供 team-runs.json 损坏后的 journal 重建）+ 每任务
// task_planned + 命中任务 task_cache_hit → 一次 SaveRun（命中任务在创建
// 批次内直接快照 completed：Result/ResultDetail 深拷贝、CachedFrom=父
// 任务 ID、SubmissionID/SentText 留空——缓存任务绝不重派发）→ startLoop。
//
// 崩溃窗口（journal 已落、快照未存）与 CreateRun 同语义：Run 不存在、
// 客户端按同 id 重放重建，追加的创建事件内容一致、按 run_created 锚
// 幂等重放，绝不产生双份快照。
func (s *Service) CreateAmendRun(team *Team, parentRunID, runID, goal, mode, collect string, verifyCommand []string, raw []PlanTaskInput, autoPlan bool) (*Run, bool, error) {
	if team == nil {
		return nil, false, errors.New("team 不能为 nil")
	}
	if parentRunID == "" {
		return nil, false, errors.New("修订 Run 必须指定父 Run")
	}
	if runID != "" && runID == parentRunID {
		return nil, false, errors.New("修订 Run 不能复用父 Run 的标识")
	}
	if autoPlan && len(raw) > 0 {
		return nil, false, errors.New("自动规划的 Run 不能携带显式计划（显式 Plan 优先，Lead 自动动作只在空计划时解析）")
	}
	if len(verifyCommand) > 0 {
		if err := ValidateVerifyCommand(verifyCommand); err != nil {
			return nil, false, err
		}
	}
	plan, err := ParsePlan(team, goal, mode, collect, raw)
	if err != nil {
		return nil, false, err
	}
	parent := s.store.Run(parentRunID)
	if parent == nil {
		return nil, false, fmt.Errorf("%w: run=%s", ErrNotFound, parentRunID)
	}
	if parent.TeamID != team.ID {
		return nil, false, fmt.Errorf("父 Run 不属于该团队：%s", parentRunID)
	}
	if runID == "" {
		runID = uuid.NewString()
	}
	// 创建幂等重放（handleRunCreate 同款，增 parentRunID 一项）：同 id 已
	// 存在 → 比对规范化内容（goal/planHash 覆盖计划本体，verifyCommand
	// 独立比对）；相等返回已建 Run + duplicate=true，零副作用。
	if existing := s.store.Run(runID); existing != nil {
		same := existing.TeamID == team.ID && existing.ParentRunID == parentRunID &&
			existing.Goal == plan.Goal && existing.Mode == plan.Mode &&
			existing.CollectPolicy == plan.CollectPolicy &&
			existing.PlanHash == plan.PlanHash &&
			existing.AutoPlan == autoPlan &&
			sameStringSlices(existing.VerifyCommand, verifyCommand)
		if !same {
			return nil, false, ErrRunContentMismatch
		}
		return existing, true, nil
	}
	imports := ImportPrefix(parent, plan, team.Policy)
	importByID := map[string]*TaskImport{}
	for i := range imports {
		importByID[imports[i].TaskID] = &imports[i]
	}
	now := time.Now()
	run := &Run{
		ID:            runID,
		TeamID:        team.ID,
		ParentRunID:   parentRunID,
		Goal:          plan.Goal,
		Mode:          plan.Mode,
		CollectPolicy: plan.CollectPolicy,
		AutoPlan:      autoPlan,
		Status:        RunCreated,
		Round:         1,
		PlanHash:      plan.PlanHash,
		Tasks:         plan.Tasks,
		VerifyCommand: append([]string(nil), verifyCommand...),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	// 创建批次事件（§10.3 有界 payload）：run_created（parentRunId 供重建）
	// → 每任务 task_planned → 命中任务紧跟一条 task_cache_hit。命中即本
	// Run 的新节点实例入账（§6.4.1 规则 4：事件一个不少，权限/闸门/版本
	// 全套照走——verifyCommand 声明的闸门对全缓存 Run 同样生效）。
	createdPayload := map[string]any{
		"teamId":        team.ID,
		"goal":          plan.Goal,
		"mode":          plan.Mode,
		"collectPolicy": plan.CollectPolicy,
		"planHash":      plan.PlanHash,
		"taskCount":     jsoncompat.Num(len(plan.Tasks)),
		"parentRunId":   parentRunID,
	}
	if autoPlan {
		createdPayload["autoPlan"] = true
	}
	if len(verifyCommand) > 0 {
		createdPayload["verifyCommand"] = append([]string(nil), verifyCommand...)
	}
	events := []*Event{newEvent(run.ID, EventRunCreated, "user", createdPayload)}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		events = append(events, newEvent(run.ID, EventTaskPlanned, "user", map[string]any{
			"taskId":           task.ID,
			"memberId":         task.MemberID,
			"instructionsHead": task.InstructionsHead,
		}))
		entry, hit := importByID[task.ID]
		if !hit {
			continue
		}
		// 快照形态与 mutateTaskTerminal 的 completed 落账一致（Result 与
		// ResultDetail 同源）；CachedFrom 标记来源，SubmissionID/SentText
		// 留空——缓存任务绝不重派发（调度只认 planned/ready）。
		detail := entry.ResultDetail
		task.Status = TaskCompleted
		task.Result = detail.Result
		task.ResultDetail = detail
		task.CachedFrom = entry.ParentTaskID
		events = append(events, newEvent(run.ID, EventTaskCacheHit, "user", map[string]any{
			"taskId":       task.ID,
			"parentRunId":  parentRunID,
			"parentTaskId": entry.ParentTaskID,
			"inputHash":    entry.InputHash,
			"sessionId":    detail.SessionID,
			"turnId":       detail.TurnID,
			"result":       detail.Result,
			"resultChars":  jsoncompat.Num(detail.ResultChars),
			"truncated":    detail.Truncated,
		}))
	}
	if err := s.accountCreatedRun(run, events); err != nil {
		return nil, false, err
	}
	return run, false, nil
}

// sameStringSlices 逐元素比对两个字符串切片（nil 与空切片视为相等）——
// httpd teams.go 同名 helper 的包内副本，供创建幂等的 verifyCommand 比对。
func sameStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
