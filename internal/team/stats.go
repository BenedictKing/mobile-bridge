package team

// stats.go 是 §12 阶段四工作项 5 的成本与耗时统计（docs/team-orchestration-
// plan.md §9/§11）：按 journal 事件 createdAt 推导 Run/Task 耗时与派发计量，
// 全部纯函数、零 I/O。时间戳来源是 newEvent 的统一盖章（runner.go）——旧
// journal 的事件 createdAt 恒为零值且无法回填，历史 Run 以 Coverage=none
// 明示「无数据」，绝不编造。
//
// token 成本明示不支持：Tokens 恒为 nil。理由（勘察定案，不猜不编）：
//   - zcode 的 turn_usage 表虽存于 ~/.zcode/cli/db/db.sqlite，但其原生 turn
//     id（turn_<uuid>）与 acpview 活动轮的序数 id（turn-N）是两个 id 空间，
//     按 turnId join 会错配；且桥不拥有该外部库的 schema 稳定性。
//   - kimi/minimax/workbuddy 的额度是账号级、轮询刷新，无 per-run/per-task
//     归因；codex 只有账号级 rateLimits；claude/dsh/opencode 无任何用量
//     数据源。
//   - 跨 Run 成本分摊因此没有可审计的事实基础，不做、不估。
//
// 统计必须走 journal.Replay 全量事件：内存投影每 Run 只保留最近 500 条
// （projection.go），长 Run 的早期事件只在磁盘。

import (
	"strconv"
	"time"
)

// 统计覆盖度（历史 journal 无时间戳时的诚实降级）。
const (
	// StatsCoverageFull 表示全部计时锚点（run_created、各次派发及其终态）
	// 都带有效 createdAt。
	StatsCoverageFull = "full"
	// StatsCoveragePartial 表示部分锚点缺时间戳（升级窗口：Run 跨新旧
	// 版本运行，旧事件零值、新事件已盖章）——缺锚点的耗时为 0。
	StatsCoveragePartial = "partial"
	// StatsCoverageNone 表示没有任何有效时间戳（纯旧 journal 的历史 Run）
	// ——全部耗时为 0，UI 明示「无数据」。
	StatsCoverageNone = "none"
)

// TaskStat 是单个任务的耗时统计：Attempt 取最高尝试号（照 recovery.go 的
// attempt 单调先例——受限重试/停靠重飞各 +1，回退的派发事实不认），
// ElapsedMs=该 attempt 的 task_dispatching.createdAt 到同 attempt 终态事件
// createdAt 的间隔；attempt 在途（已派发未终局）时到 now。从未派发的任务
// 不产生条目（amend 缓存命中任务零派发即零耗时，没有可测的墙钟）。
type TaskStat struct {
	TaskID    string `json:"taskId"`
	Attempt   int    `json:"attempt"`
	ElapsedMs int64  `json:"elapsedMs"`
}

// BudgetStat 是 §9 的预算计量：Units=策略 BudgetUnits（0=只统计不暂停）；
// Used=task_dispatching 计数（按 taskId+attempt 去重，commitRun 版本冲突
// 重试可能重复落同内容事件，重放按幂等键折算）；Ceiling=Run.BudgetCeiling
// （预算追加后的有效上限，0=从未追加）；Exhausted 与 runner 侧的停靠判定
// 同一谓词（budgetExhausted）。
type BudgetStat struct {
	Units     int  `json:"units"`
	Used      int  `json:"used"`
	Ceiling   int  `json:"ceiling"`
	Exhausted bool `json:"exhausted"`
}

// RunStats 是一次 Run 的统计快照。Tokens 恒 nil（见文件头：token 无可审计
// 数据源，明示不支持，绝不填估值）；RunElapsedMs=run_created.createdAt 到
// 终局事件（无终局则到 now）的间隔。
type RunStats struct {
	RunElapsedMs  int64      `json:"runElapsedMs"`
	DispatchCount int        `json:"dispatchCount"`
	Tokens        *struct{}  `json:"tokens"`
	Tasks         []TaskStat `json:"tasks"`
	Coverage      string     `json:"coverage"`
	Budget        BudgetStat `json:"budget"`
}

// budgetExhausted 是 §9 预算停靠的统一判定（passDispatch 的闸门与
// ComputeRunStats 的 Exhausted 字段共用，绝不两处漂移）：units≤0 只统计
// 不停靠（§4.2 默认）；否则已派发数 ≥ max(ceiling, units) 即耗尽——
// 追加过的上限（ceiling）不会因策略调低而回落，也不低于当前策略值。
func budgetExhausted(units, ceiling, used int) bool {
	if units <= 0 {
		return false
	}
	threshold := ceiling
	if units > threshold {
		threshold = units
	}
	return used >= threshold
}

// budgetThreshold 返回当前有效上限（max(ceiling, units)；units≤0 时无闸门，
// 返回 0 仅供展示）。
func budgetThreshold(units, ceiling int) int {
	threshold := ceiling
	if units > threshold {
		threshold = units
	}
	return threshold
}

// budgetStopReason 组装预算停靠的中文说明（机器前缀照 verify_failed 先例，
// Resume 按前缀路由；stopReason 同时落 run_waiting_input payload，journal
// 重建据此还原）。
func budgetStopReason(threshold, used int) string {
	return StopReasonBudgetExhausted + "（任务派发数已达预算上限：已派发 " +
		strconv.Itoa(used) + " / 上限 " + strconv.Itoa(threshold) +
		"；可继续追加预算运行，或取消本次运行）"
}

// countRunDispatches 数 journal 里该 Run 的 task_dispatching 事实数（按
// taskId+attempt 去重）：预算计量（§9）与 DispatchCount 共用——派发是
// journal 可精确重放的事实，运行期增量、重启恢复、Resume 重算都从同一
// 函数取数，不漂移。
func countRunDispatches(events []*Event, runID string) int {
	seen := map[[2]any]bool{}
	for _, event := range events {
		if event == nil || event.RunID != runID || event.Type != EventTaskDispatching {
			continue
		}
		seen[[2]any{payloadString(event.Payload, "taskId"), payloadInt(event.Payload, "attempt")}] = true
	}
	return len(seen)
}

// ComputeRunStats 从 journal 事件推导 Run 统计（纯函数，零 I/O）：events
// 传 journal.Replay 的全量（本函数自行按 RunID 过滤）；run/policy 提供
// BudgetCeiling 与 BudgetUnits 快照；now 供在途任务与未终局 Run 取时。
// 终局事件取 run_completed/failed/cancelled/stopped 的最后一条；任务终态
// 配对规则：task_failed/task_stopped 的 payload 携带 attempt（mutateTaskTerminal
// 约定），task_completed/task_outcome_unknown/task_input_hash_mismatch 按
// 该任务最近一次 task_dispatching 的 attempt 归属（与 recovery.go 的
// 「record.attempt + 更高 attempt 取代」同构）。
func ComputeRunStats(events []*Event, run *Run, policy TeamPolicy, now time.Time) RunStats {
	stats := RunStats{Coverage: StatsCoverageNone, Tokens: nil}
	if run == nil {
		return stats
	}
	created := time.Time{}
	createdSeen := false
	finished := time.Time{}
	type timingPair struct {
		dispatch  time.Time
		terminal  time.Time
		termEvent bool
	}
	pairs := map[[2]any]*timingPair{}
	order := []string{} // 任务首次派发序（快照缺任务时的兜底序）
	currentAttempt := map[string]int{}
	for _, event := range events {
		if event == nil || event.RunID != run.ID {
			continue
		}
		switch event.Type {
		case EventRunCreated:
			created = event.CreatedAt
			createdSeen = true
		case EventRunCompleted, EventRunFailed, EventRunCancelled, EventRunStopped:
			finished = event.CreatedAt
		case EventTaskDispatching:
			taskID := payloadString(event.Payload, "taskId")
			attempt := payloadInt(event.Payload, "attempt")
			if taskID == "" {
				continue
			}
			// 最高 attempt 生效（单调语义，照 recovery.go 先例）：回退的
			// 派发号不回退 currentAttempt。
			if attempt >= currentAttempt[taskID] {
				currentAttempt[taskID] = attempt
			}
			key := [2]any{taskID, attempt}
			entry, exists := pairs[key]
			if !exists {
				entry = &timingPair{}
				pairs[key] = entry
				order = append(order, taskID)
			}
			entry.dispatch = event.CreatedAt // 同键重复事件（commitRun 重试）last-wins
		case EventTaskCompleted, EventTaskFailed, EventTaskOutcomeUnknown,
			EventTaskStopped, EventTaskInputHashMismatch:
			taskID := payloadString(event.Payload, "taskId")
			if taskID == "" {
				continue
			}
			attempt := payloadInt(event.Payload, "attempt")
			if attempt == 0 {
				attempt = currentAttempt[taskID] // completed/unknown 不携带 attempt
			}
			entry, exists := pairs[[2]any{taskID, attempt}]
			if !exists {
				continue // 终态先于可见派发（journal 截断/重建）：无从计时。
			}
			if !entry.termEvent {
				entry.termEvent = true
				entry.terminal = event.CreatedAt
			}
			// 同 attempt 的后续终态事件（重放 last-wins 的重复落账）不另计。
		}
	}
	// 覆盖度锚点：run_created + 每条任务条目的派发时间（及其已终局时的
	// 终态时间）。按条目计数而非按事件计数——同 (task,attempt) 的重复
	// 事件（commitRun 版本冲突重试）只算一个锚点，不把 full 误降 partial。
	anchors, stamped := 0, 0
	if createdSeen {
		anchors++
		if !created.IsZero() {
			stamped++
		}
	}
	for _, taskID := range order {
		entry := pairs[[2]any{taskID, currentAttempt[taskID]}]
		if entry == nil || entry.dispatch.IsZero() {
			continue
		}
		anchors++
		stamped++
		if entry.termEvent {
			anchors++
			if !entry.terminal.IsZero() {
				stamped++
			}
		}
	}
	stats.DispatchCount = countRunDispatches(events, run.ID)
	// Run 耗时：created 有效才有意义；终局时间缺失（在途/停靠）按 now。
	if !created.IsZero() {
		end := now
		if !finished.IsZero() {
			end = finished
		}
		if elapsed := end.Sub(created); elapsed > 0 {
			stats.RunElapsedMs = elapsed.Milliseconds()
		}
	}
	// 任务条目：优先按快照任务序（与 UI 卡片一致），快照缺的按首次派发序补。
	taskIDs := make([]string, 0, len(order))
	inSnapshot := map[string]bool{}
	for i := range run.Tasks {
		if _, dispatched := currentAttempt[run.Tasks[i].ID]; dispatched {
			taskIDs = append(taskIDs, run.Tasks[i].ID)
			inSnapshot[run.Tasks[i].ID] = true
		}
	}
	for _, taskID := range order {
		if !inSnapshot[taskID] {
			taskIDs = append(taskIDs, taskID)
		}
	}
	for _, taskID := range taskIDs {
		attempt := currentAttempt[taskID]
		entry := pairs[[2]any{taskID, attempt}]
		if entry == nil {
			continue
		}
		stat := TaskStat{TaskID: taskID, Attempt: attempt}
		if !entry.dispatch.IsZero() {
			switch {
			case entry.termEvent && entry.terminal.IsZero():
				// 终态事件缺时间戳（旧 journal）：该条目不计耗时。
			case entry.termEvent:
				if elapsed := entry.terminal.Sub(entry.dispatch); elapsed > 0 {
					stat.ElapsedMs = elapsed.Milliseconds()
				}
			default:
				if elapsed := now.Sub(entry.dispatch); elapsed > 0 {
					stat.ElapsedMs = elapsed.Milliseconds()
				}
			}
		}
		stats.Tasks = append(stats.Tasks, stat)
	}
	// 覆盖度：全部计时锚点有效=full，部分=partial，全无=none。锚点口径=
	// run_created + 每条有效任务条目的派发时间（及其已终局时的终态时间）。
	switch {
	case stamped == 0:
		stats.Coverage = StatsCoverageNone
	case stamped == anchors:
		stats.Coverage = StatsCoverageFull
	default:
		stats.Coverage = StatsCoveragePartial
	}
	stats.Budget = BudgetStat{
		Units:     policy.BudgetUnits,
		Used:      stats.DispatchCount,
		Ceiling:   run.BudgetCeiling,
		Exhausted: budgetExhausted(policy.BudgetUnits, run.BudgetCeiling, stats.DispatchCount),
	}
	return stats
}
