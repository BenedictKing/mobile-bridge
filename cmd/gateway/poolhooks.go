package main

// Account-pool quota failover hook: runs first in the turn-settle chain. When
// a turn dies on a quota-ish error and the pool confirms exhaustion, the
// auto-switcher swaps credentials and the session resumes on its fresh
// handle. Returning true tells the chain the settle was handled (the regular
// auto-continue would skip the quota stop anyway, and queued sends dispatch
// on the resumed turn's next settle).

import (
	"context"
	"log"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func poolQuotaHook(pool *accpool.Manager, auto *accpool.AutoSwitcher, kind string, manager *acpruntime.Manager, resumeAfterSwitch func(*acpruntime.Session)) func(*acpruntime.Session) bool {
	return func(session *acpruntime.Session) bool {
		// kind is the ACP agent kind; the pool speaks its own kind names
		// (minimax pool vs mcode agent).
		poolKind := accpool.PoolKind(kind)
		if pool == nil || auto == nil || !pool.AutoSwitchEnabled(poolKind) {
			return false
		}
		snapshot := session.Snapshot()
		turnContext, ok := jev.ContinueContext(kind, snapshot)
		if !ok {
			return false
		}
		stopReason := jsoncompat.PyStr(turnContext.MustGet("stopReason"))
		errorText := jsoncompat.PyStr(turnContext.MustGet("error"))
		if !jev.QuotaStop(stopReason, errorText) {
			return false
		}
		log.Printf("账号池: %s 会话 %s 回合因疑似额度问题结束，确认额度状态", poolKind, session.ID)
		switched, err := auto.HandleQuotaExhaustion(context.Background(), poolKind, session.ID)
		if err != nil {
			log.Printf("账号池: %s 自动切换失败: %v", poolKind, err)
		}
		if !switched {
			return false
		}
		// Switch re-attached every live session; judge the fresh handle once.
		fresh := manager.Get(session.ID)
		if fresh == nil {
			log.Printf("账号池: %s 会话 %s 切换后无活跃句柄，跳过续跑", poolKind, session.ID)
			return true
		}
		if resumeAfterSwitch != nil {
			resumeAfterSwitch(fresh)
			return true
		}
		if resumeErr := fresh.ResumeAfterSwitchPrompt("继续", time.Hour); resumeErr != nil {
			log.Printf("账号池: %s 会话 %s 切换后续跑失败: %v", poolKind, session.ID, resumeErr)
			return true
		}
		log.Printf("账号池: %s 会话 %s 已在新账号上自动继续", poolKind, session.ID)
		return true
	}
}

func resumePoolSessionsAfterSwitch(manager *acpruntime.Manager, resumeAfterSwitch func(*acpruntime.Session)) {
	if manager == nil || resumeAfterSwitch == nil {
		return
	}
	for id := range manager.LiveSessions() {
		if fresh := manager.Peek(id); fresh != nil {
			resumeAfterSwitch(fresh)
		}
	}
}
