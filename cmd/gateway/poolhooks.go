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

func poolQuotaHook(pool *accpool.Manager, auto *accpool.AutoSwitcher, kind string, manager *acpruntime.Manager) func(*acpruntime.Session) bool {
	return func(session *acpruntime.Session) bool {
		if pool == nil || auto == nil || !pool.AutoSwitchEnabled(kind) {
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
		log.Printf("账号池: %s 会话 %s 回合因疑似额度问题结束，确认额度状态", kind, session.ID)
		switched, err := auto.HandleQuotaExhaustion(context.Background(), kind, session.ID)
		if err != nil {
			log.Printf("账号池: %s 自动切换失败: %v", kind, err)
		}
		if !switched {
			return false
		}
		// Switch re-attached every live session; resume on the fresh handle.
		fresh := manager.Get(session.ID)
		if fresh == nil {
			log.Printf("账号池: %s 会话 %s 切换后无活跃句柄，跳过续跑", kind, session.ID)
			return true
		}
		if resumeErr := fresh.ResumeAfterSwitchPrompt("继续", time.Hour); resumeErr != nil {
			log.Printf("账号池: %s 会话 %s 切换后续跑失败: %v", kind, session.ID, resumeErr)
			return true
		}
		log.Printf("账号池: %s 会话 %s 已在新账号上自动继续", kind, session.ID)
		return true
	}
}
