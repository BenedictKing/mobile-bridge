package team

// inputroute.go 是输入路由器（docs/team-demand-queue-plan.md §4，阶段 2）：
// 每次处理一个 Team 的一条输入，按「优先输入在前、各自按接受序」取队；
// 路由决策是机械且可复核的（无歧义自动派发）：
//
//	显式 targetRunId/targetTaskId → follow_up（追加 Lead 任务）
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
	for _, input := range s.inputs.RouteOrder(teamID) {
		if s.routeOneInput(team, input) {
			routed++
		}
	}
	return routed
}

// routeOneInput 推进单条输入：意图 → 创建/追加 → 确认。返回是否在本轮
// 确认 routed（停靠/幂等跳过返回 false）。
func (s *Service) routeOneInput(team *Team, input *TeamInput) bool {
	runs := s.store.RunsByTeam(team.ID)
	active := make([]*Run, 0, 1)
	for _, run := range runs {
		if !terminalRun(run.Status) {
			active = append(active, run)
		}
	}
	kind, runID, taskID, waitReason := "", "", "", ""
	switch {
	case input.ExplicitTarget && input.TargetTaskID != "" && input.TargetRunID != "":
		// 显式目标优先（§4 路由顺序第 1 条）：仅用户显式指定（API 透传），
		// 路由意图自记的确定性目标在重泵时按 Run 状态重推导，不走此分支。
		kind, runID, taskID = "follow_up", input.TargetRunID, input.TargetTaskID
	case len(active) > 1:
		waitReason = "团队有多个未结束的 Run，请先在 Run 详情页收敛目标（完成或取消）后重试"
	case len(active) == 1:
		if !active[0].AutoPlan || active[0].Mode != ModeSingle {
			waitReason = "当前活跃 Run 是显式计划，请在 Run 详情页处理"
			break
		}
		kind, runID, taskID = "follow_up", active[0].ID, ConversationTaskID(input.ID)
	case len(runs) > 0:
		kind, runID, taskID = "amend", input.ID, "lead"
	default:
		kind, runID, taskID = "new_task", input.ID, "lead"
	}
	if waitReason != "" {
		if err := s.inputs.MarkWaitingInput(team.ID, input.ID, waitReason); err != nil {
			log.Printf("team %s 输入 %s 停靠记账失败: %v", team.ID, input.ID, err)
		}
		return false
	}
	// ① 意图先持久化（同意图幂等；queued/waiting → routing）。
	if err := s.inputs.MarkRoutingIntent(team.ID, input.ID, kind, runID, taskID); err != nil {
		log.Printf("team %s 输入 %s 路由意图落账失败: %v", team.ID, input.ID, err)
		return false
	}
	// ② 幂等创建 Run/Task（确定性 ID：重放命中既有 Run/Task）。
	createErr := s.materializeRoute(team, input, kind, runID, taskID)
	if createErr != nil {
		waitReason = createErr.Error()
		if err := s.inputs.MarkWaitingInput(team.ID, input.ID, waitReason); err != nil {
			log.Printf("team %s 输入 %s 停靠记账失败: %v", team.ID, input.ID, err)
		}
		return false
	}
	// ③ 确认 routed + 工作项链接。
	if err := s.inputs.MarkRouted(team.ID, input.ID, kind, runID, taskID); err != nil {
		log.Printf("team %s 输入 %s 路由确认失败（重泵补齐）: %v", team.ID, input.ID, err)
		return false
	}
	return true
}

// materializeRoute 按路由种类幂等落成 Run/Task（复用既有会话化路径——
// 首条输入创建初始 Run、活跃 Run 追加 Lead 任务、终态后血缘修订）。
func (s *Service) materializeRoute(team *Team, input *TeamInput, kind, runID, taskID string) error {
	switch kind {
	case "new_task":
		_, err := s.CreateConversationRun(team, runID, input.Text, input.AttachmentIDs)
		return err
	case "amend":
		runs := s.store.RunsByTeam(team.ID)
		if len(runs) == 0 {
			return errors.New("修订目标缺失：团队没有可延续的 Run")
		}
		_, err := s.CreateConversationAmendRun(team, runs[len(runs)-1].ID, runID, input.Text, input.AttachmentIDs)
		return err
	case "follow_up":
		_, _, err := s.FollowUpConversationRun(team, runID, taskID, input.Text, input.AttachmentIDs)
		return err
	default:
		return errors.New("未知路由种类：" + kind)
	}
}
