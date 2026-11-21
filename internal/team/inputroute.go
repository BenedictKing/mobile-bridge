package team

// inputroute.go 是输入路由器（docs/team-demand-queue-plan.md §4，阶段 2）：
// 每次处理一个 Team 的一条输入，按「优先输入在前、各自按接受序」取队；
// 路由决策是机械且可复核的（无歧义自动派发）：
//
//	显式 targetRunId/targetTaskId → follow_up（直达原任务的成员与执行线程）
//	→ 唯一活跃 autoPlan Run 续作 → follow_up
//	→ 无活跃 Run 且有历史 → amend（parentRunId 血缘修订）
//	→ 全新 → new_task（runId=inputID 的初始 autoPlan Run）
//	→ 多个活跃 Run / Run 收集中 → waiting_input（原因记账，重泵复核）。
//
// 顺序契约（§3.1）：先落 input_route_intent（确定性目标），幂等创建
// Run/Task，再确认 input_routed + work_item_linked。崩溃后重泵把停在
// routing 的输入按 intent 对账补齐（创建路径全部幂等：runID/taskID 确定）。
// 语义路由（answer/clarify、多要求拆分、与在途写入冲突的分类）依赖 Lead
// 提案协议，属后续增量；当前对范围不明的输入停靠 waiting_input，不用猜测
// 代替用户决定。

import (
	"errors"
	"fmt"
	"log"
	"time"
)

// ConversationTaskID 是后续团队消息的确定性任务 ID（httpd 的
// conversationTaskID 同一约定）。
func ConversationTaskID(inputID string) string { return "input-" + inputID }

// routingTicker 是路由循环的兜底周期：Run 终态释放「唯一活跃 Run」槽位后，
// 排队输入由下一拍补泵（accept 侧有即时唤醒，这里只兜恢复/竞态窗口）。
const routingTicker = 15 * time.Second

// Inputs 返回装配的输入账本（未装配为 nil）。
func (s *Service) Inputs() *InputLedger { return s.inputs }

// SetInputs 装配输入账本（必须在任何请求前完成；main.go/测试环境直赋）。
func (s *Service) SetInputs(ledger *InputLedger) { s.inputs = ledger }

// PumpRouting 非阻塞唤醒路由循环（accept 之后即时泵；循环惰性启动）。
func (s *Service) PumpRouting() {
	if s.inputs == nil {
		return
	}
	s.routingOnce.Do(func() {
		s.routingWake = make(chan struct{}, 1)
		go s.routingLoop()
	})
	select {
	case s.routingWake <- struct{}{}:
	default:
	}
}

// routingLoop 是常驻路由循环：唤醒即全 Team 重泵，ticker 兜底。
func (s *Service) routingLoop() {
	ticker := time.NewTicker(routingTicker)
	defer ticker.Stop()
	for {
		select {
		case <-s.routingWake:
		case <-ticker.C:
		}
		for _, team := range s.store.Teams() {
			s.RoutePendingInputs(team.ID)
		}
	}
}

// RoutePendingInputs 同步重泵一个 Team 的待路由输入：逐条推进，遇到
// waiting_input 停靠记录原因后继续（后续输入仍按序尝试——不同输入的停靠
// 原因独立）。返回本轮确认 routed 的条数。
func (s *Service) RoutePendingInputs(teamID string) int {
	if s.inputs == nil {
		return 0
	}
	team := s.store.Team(teamID)
	if team == nil {
		return 0
	}
	routed := 0
	// 队列预算计量一次、逐条复用（journal 全量重放，泵多条输入不重复读）。
	budgetExceeded, budgetReason := s.queueBudgetState(team)
	for _, input := range s.inputs.RouteOrderAt(teamID, s.nowFn()) {
		if s.routeOneInput(team, input, budgetExceeded, budgetReason) {
			routed++
		}
	}
	return routed
}

// queueBudgetState 预计算团队的队列预算停靠态（阶段 3，§5）：计量=journal
// 的 task_dispatching 事实跨 Run 汇总（防拆 Run 绕过成本上限）；≤0 不限。
// journal 读取失败退化为不限——绝不因读账失败卡死路由。
func (s *Service) queueBudgetState(team *Team) (bool, string) {
	budget := team.Policy.QueueDispatchBudget
	if budget <= 0 {
		return false, ""
	}
	count, oldest := s.teamDispatchCount(team.ID)
	if count < budget {
		return false, ""
	}
	reset := "滚动窗口即将重置"
	if !oldest.IsZero() {
		reset = "额度重置约 " + oldest.Add(queueDispatchWindow).Format("15:04")
	}
	return true, fmt.Sprintf("团队队列预算已用完（24 小时滚动上限 %d 次派发，当前 %d 次）：%s，或调整 queueDispatchBudget", budget, count, reset)
}

// routeOneInput 推进单条输入：意图 → 创建/追加 → 确认。返回是否在本轮
// 确认 routed（停靠/幂等跳过返回 false）。
func (s *Service) routeOneInput(team *Team, input *TeamInput, budgetExceeded bool, budgetReason string) bool {
	kind, runID, taskID, parentRunID := input.RouteStatus, input.TargetRunID, input.TargetTaskID, input.ParentRunID
	sourceTaskID := input.SourceTaskID
	hasIntent := (input.QueueStatus == InputRouting || input.QueueStatus == InputWaitingInput) && kind != ""
	waitReason := ""
	if !hasIntent {
		allRuns := s.store.RunsByTeam(team.ID)
		runs := make([]*Run, 0, len(allRuns))
		for _, run := range allRuns {
			if !run.IsIssueClassificationOnly() {
				runs = append(runs, run)
			}
		}
		active := make([]*Run, 0, 1)
		for _, run := range runs {
			if !terminalRun(run.Status) && !goalLoopRequiresAmend(run, team.Policy) {
				active = append(active, run)
			}
		}
		switch {
		case input.ExplicitTarget && input.TargetTaskID != "" && input.TargetRunID != "":
			targetRun := s.store.Run(input.TargetRunID)
			if targetRun == nil || findTaskIn(targetRun, input.TargetTaskID) == nil {
				waitReason = "追问目标不存在，请刷新 Run 后重新选择任务"
				break
			}
			if goalLoopRequiresAmend(targetRun, team.Policy) {
				// 原 Run 已达到自动续做上限，具体任务入口也必须遵守
				// 同一修订语义，不能再次把输入塞回已满的 Run。
				kind, runID, taskID, parentRunID = "amend", input.ID, "lead", targetRun.ID
				sourceTaskID = ""
			} else {
				kind, runID, taskID = "follow_up", input.TargetRunID, ConversationTaskID(input.ID)
				sourceTaskID = input.TargetTaskID
			}
		case len(active) > 1:
			waitReason = "团队有多个未结束的 Run，请先在 Run 详情页收敛目标（完成或取消）后重试"
		case len(active) == 1:
			if !active[0].AutoPlan || active[0].Mode != ModeSingle {
				waitReason = "当前活跃 Run 是显式计划，请在 Run 详情页处理"
				break
			}
			kind, runID, taskID = "follow_up", active[0].ID, ConversationTaskID(input.ID)
		case len(runs) > 0:
			kind, runID, taskID, parentRunID = "amend", input.ID, "lead", runs[len(runs)-1].ID
		default:
			kind, runID, taskID = "new_task", input.ID, "lead"
		}
	}
	if waitReason != "" {
		if err := s.inputs.MarkWaitingInput(team.ID, input.ID, waitReason); err != nil {
			log.Printf("team %s 输入 %s 停靠记账失败: %v", team.ID, input.ID, err)
		}
		return false
	}
	// ⓪ Team 队列滚动派发预算（阶段 3，§5）：超出后新路由停靠并提示重置
	// 时间（计量由 RoutePendingInputs 预计算复用）。旧路径（budgetUnits）
	// 语义不变。
	if budgetExceeded {
		if err := s.inputs.MarkWaitingInput(team.ID, input.ID, budgetReason); err != nil {
			log.Printf("team %s 输入 %s 停靠记账失败: %v", team.ID, input.ID, err)
		}
		return false
	}
	// ① 意图先持久化（同意图幂等；queued/waiting → routing）。
	if err := s.inputs.MarkRoutingIntent(team.ID, input.ID, kind, runID, taskID, parentRunID, sourceTaskID); err != nil {
		log.Printf("team %s 输入 %s 路由意图落账失败: %v", team.ID, input.ID, err)
		return false
	}
	// ② 幂等创建 Run/Task（确定性 ID：重放命中既有 Run/Task）。
	input.SourceTaskID = sourceTaskID
	createErr := s.materializeRoute(team, input, kind, runID, taskID, parentRunID)
	if createErr != nil {
		waitReason = createErr.Error()
		if err := s.inputs.MarkWaitingInput(team.ID, input.ID, waitReason); err != nil {
			log.Printf("team %s 输入 %s 停靠记账失败: %v", team.ID, input.ID, err)
		}
		return false
	}
	targets := []struct{ runID, taskID string }{{runID, taskID}}
	if kind == "planned" {
		targets = plannedRouteTargets(input)
	}
	for _, target := range targets {
		if err := s.validateRouteTarget(team.ID, kind, target.runID, target.taskID, parentRunID); err != nil {
			if waitErr := s.inputs.MarkWaitingInput(team.ID, input.ID, err.Error()); waitErr != nil {
				log.Printf("team %s 输入 %s 停靠记账失败: %v", team.ID, input.ID, waitErr)
			}
			return false
		}
		// ③ 确认 routed + 工作项链接。
		if err := s.inputs.MarkRouted(team.ID, input.ID, kind, target.runID, target.taskID); err != nil {
			log.Printf("team %s 输入 %s 路由确认失败（重泵补齐）: %v", team.ID, input.ID, err)
			return false
		}
	}
	return len(targets) > 0
}

// materializeRoute 按路由种类幂等落成 Run/Task（复用既有会话化路径——
// 首条输入创建初始 Run、活跃 Run 追加 Lead 任务、终态后血缘修订）。
func (s *Service) materializeRoute(team *Team, input *TeamInput, kind, runID, taskID, parentRunID string) error {
	if kind == "new_task" || kind == "amend" {
		if existing := s.store.Run(runID); existing != nil {
			task := findTaskIn(existing, taskID)
			if existing.TeamID != team.ID || !existing.AutoPlan || existing.Mode != ModeSingle ||
				existing.Goal != input.Text || task == nil || task.MemberID != team.LeadID ||
				task.Prompt != input.Text || !sameStringSlices(task.Attachments, input.AttachmentIDs) ||
				(kind == "amend" && existing.ParentRunID != parentRunID) ||
				(kind == "new_task" && existing.ParentRunID != "") {
				return errors.New("路由目标已存在但内容或血缘不一致")
			}
			return nil
		}
	}
	switch kind {
	case "new_task":
		_, err := s.CreateConversationRun(team, runID, input.Text, input.AttachmentIDs)
		return err
	case "amend":
		if parentRunID == "" {
			return errors.New("修订目标缺失：路由意图没有固定父 Run")
		}
		_, err := s.CreateConversationAmendRun(team, parentRunID, runID, input.Text, input.AttachmentIDs)
		return err
	case "follow_up":
		_, _, err := s.FollowUpConversationRun(team, runID, taskID, input.Text, input.AttachmentIDs, input.SourceTaskID)
		return err
	case "planned":
		return s.materializePlannedRoute(team, input)
	default:
		return errors.New("未知路由种类：" + kind)
	}
}

func (s *Service) validateRouteTarget(teamID string, kind, runID, taskID, parentRunID string) error {
	run := s.store.Run(runID)
	if run == nil || run.TeamID != teamID {
		return fmt.Errorf("路由目标缺失：Run 不存在或不属于当前团队")
	}
	if kind == "amend" && run.ParentRunID != parentRunID {
		return fmt.Errorf("路由目标异常：amend 父 Run 与持久意图不一致")
	}
	if findTaskIn(run, taskID) == nil {
		return fmt.Errorf("路由目标缺失：目标任务不存在")
	}
	return nil
}

// teamDispatchCount 统计团队在滚动 24 小时窗口内的派发次数（journal 的
// task_dispatching 事实，runId→teamId 映射来自 run_created 载荷与账本快照
// ——已删 Run 的事件仍可映射）。返回 (次数, 窗口内最早一次的时间)；journal
// 读取失败返回 (0, 零值)——预算闸门退化为不限，绝不因读账失败卡死路由。
func (s *Service) teamDispatchCount(teamID string) (int, time.Time) {
	if s.journal == nil {
		return 0, time.Time{}
	}
	events, err := s.journal.Replay()
	if err != nil {
		log.Printf("team %s 队列预算计量失败（本轮不限）：journal 重放失败 %v", teamID, err)
		return 0, time.Time{}
	}
	teamOfRun := map[string]string{}
	for _, run := range s.store.Runs() {
		teamOfRun[run.ID] = run.TeamID
	}
	cutoff := s.nowFn().Add(-queueDispatchWindow)
	count := 0
	var oldest time.Time
	for _, event := range events {
		if event == nil {
			continue
		}
		if event.Type == "run_created" {
			if id := payloadString(event.Payload, "teamId"); id != "" {
				teamOfRun[event.RunID] = id
			}
			continue
		}
		if event.Type != EventTaskDispatching || teamOfRun[event.RunID] != teamID {
			continue
		}
		if event.CreatedAt.Before(cutoff) {
			continue
		}
		count++
		if oldest.IsZero() || event.CreatedAt.Before(oldest) {
			oldest = event.CreatedAt
		}
	}
	return count, oldest
}
