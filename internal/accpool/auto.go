package accpool

import (
	"context"
	"errors"
	"log"
	"time"
)

// AutoSwitcher drives quota-triggered failover. It is fed by two paths: the
// turn-settle hook (a turn died on a quota-ish error) and the poller (the
// active account's window crossed the threshold). Both funnel into
// HandleQuotaExhaustion, which confirms the exhaustion with a fresh quota
// read before touching anything.
type AutoSwitcher struct {
	Pool     *Manager
	Sessions func(kind string) SessionManager
	// Notify publishes a user-facing notification (title, body); nil disables.
	Notify func(title, body string)

	// Threshold is the used/limit ratio that counts as exhausted (0.95).
	Threshold float64
	// Cooldown bounds consecutive switches of one agent (10 minutes).
	Cooldown time.Duration
	// MaxPerHour caps automatic switches across all agents (6/hour).
	MaxPerHour int
	// MaxSwitchesAwayFrom bounds how often one account may be auto-abandoned
	// in 24h before the switcher stops using it as a source (3).
	MaxSwitchesAwayFrom int
	// ResetGrace: when the exhausted window resets within this span, waiting
	// beats switching (5 minutes).
	ResetGrace time.Duration
	// ProactiveResetUsedRatio: on the poller path a zcode five-hour card is
	// spent as soon as the Token window's used ratio crosses this line (0.98 —
	// remaining ≤2%), before a live turn dies on the window; the failure-hook
	// path keeps the plain Threshold trigger so a dead turn recovers ASAP.
	ProactiveResetUsedRatio float64

	now func() time.Time
}

// NewAutoSwitcher builds a switcher with the documented default gates.
func NewAutoSwitcher(pool *Manager) *AutoSwitcher {
	switcher := &AutoSwitcher{Pool: pool}
	switcher.defaults()
	return switcher
}

func (a *AutoSwitcher) defaults() {
	if a.Threshold <= 0 || a.Threshold > 1 {
		a.Threshold = 0.95
	}
	if a.Cooldown <= 0 {
		a.Cooldown = 10 * time.Minute
	}
	if a.MaxPerHour <= 0 {
		a.MaxPerHour = 6
	}
	if a.MaxSwitchesAwayFrom <= 0 {
		a.MaxSwitchesAwayFrom = 3
	}
	if a.ResetGrace <= 0 {
		a.ResetGrace = 5 * time.Minute
	}
	if a.ProactiveResetUsedRatio <= 0 || a.ProactiveResetUsedRatio > 1 {
		a.ProactiveResetUsedRatio = 0.98
	}
	if a.now == nil {
		a.now = time.Now
	}
}

func (a *AutoSwitcher) notify(title, body string) {
	if a.Notify != nil {
		a.Notify(title, body)
	}
}

// HandleQuotaExhaustion confirms the active account is over the threshold and
// switches to the best available account. It reports whether a switch
// happened; resuming interrupted sessions is the caller's job (it owns the
// ACP managers and the resume budget). failedSessionID exempts the session
// whose turn just died from the busy gate (empty on the poller path).
func (a *AutoSwitcher) HandleQuotaExhaustion(ctx context.Context, kind, failedSessionID string) (bool, error) {
	if a == nil || a.Pool == nil {
		return false, nil
	}
	a.defaults()
	if !a.Pool.AutoSwitchEnabled(kind) {
		return false, nil
	}
	active := a.Pool.activeID(kind)
	if active == "" {
		return false, nil
	}

	// Confirm with a fresh read: transient 429s look exactly like window
	// exhaustion in failure text, and only the API tells them apart.
	account, err := a.Pool.RefreshQuota(ctx, kind, active)
	if err != nil && account.Quota == nil {
		return false, err
	}
	exhausted := ExhaustedWindows(account.Quota, a.Threshold)
	if len(exhausted) == 0 {
		log.Printf("账号池: %s 故障疑似限流但额度未耗尽，不切换（账号 %s）", kind, active)
		return false, nil
	}
	// zcode reset cards: a held 重置卡 restores the window without touching
	// sessions, which always beats switching accounts when available. The
	// poller path burns proactively once ≤2% remains; a turn that already died
	// burns at the plain exhaustion threshold.
	proactive := failedSessionID == ""
	if kind == "zcode" && a.tryZcodeResetCard(ctx, account, exhausted, proactive) {
		return false, nil
	}
	// Between the exhaustion and proactive lines a held card means the
	// drain-aware poller will burn it within seconds — switching now would
	// disturb every live session for nothing.
	if kind == "zcode" && proactive && a.zcodeCardHeld(ctx, account, exhausted) {
		log.Printf("账号池: zcode 账号 %s 额度未到主动核销线且持有重置卡，等待而非切换", account.ID)
		return false, nil
	}
	if gate := a.gate(kind, active, exhausted); gate != "" {
		log.Printf("账号池: %s 自动切换被门控拦下：%s", kind, gate)
		return false, nil
	}

	target := a.Pool.ChooseTarget(ctx, kind, active, a.Threshold)
	if target == nil {
		a.notify("账号池：无可用账号", kind+" 的 "+active+" 额度已耗尽，池内没有其他可用账号")
		return false, nil
	}
	sessions := a.Sessions
	var sm SessionManager
	if sessions != nil {
		sm = sessions(kind)
	}
	ignoreBusy := map[string]bool{}
	if failedSessionID != "" {
		ignoreBusy[failedSessionID] = true
	}
	if err := a.Pool.Switch(ctx, kind, target.ID, "auto-quota", sm, ignoreBusy); err != nil {
		var busy *BusyError
		if errors.As(err, &busy) {
			// A turn in flight owns the account right now; the failure hook
			// fires again when it settles.
			return false, nil
		}
		a.notify("账号池：自动切换失败", kind+"："+err.Error())
		return false, err
	}
	a.notify("账号池：已自动切换", kind+" 账号 "+active+" 额度用尽，已切换到 "+target.Label)
	log.Printf("账号池: %s 自动切换 %s -> %s（额度用尽）", kind, active, target.ID)
	return true, nil
}

// zcodeTokenWindow picks the Token 用量 (five-hour) row out of an exhausted
// window list — the only window reset cards restore.
func zcodeTokenWindow(exhausted []*WindowQuota) *WindowQuota {
	for _, candidate := range exhausted {
		if candidate.Name == "Token 用量" {
			return candidate
		}
	}
	return nil
}

// zcodeCardHeld reports whether the Token window sits between the exhaustion
// and proactive-burn lines while the account still holds a five-hour card: the
// drain-aware poller will burn it within seconds, so the switcher waits
// instead of disturbing live sessions. A failed status check falls through to
// the ordinary switch logic.
func (a *AutoSwitcher) zcodeCardHeld(ctx context.Context, account *Account, exhausted []*WindowQuota) bool {
	window := zcodeTokenWindow(exhausted)
	if window == nil || window.Limit <= 0 {
		return false
	}
	if float64(window.Used)/float64(window.Limit) >= a.ProactiveResetUsedRatio {
		return false
	}
	status, err := zcodeResetStatus(ctx, account)
	if err != nil {
		return false
	}
	return status.FiveHourCards > 0
}

// tryZcodeResetCard spends one held five-hour reset card on the exhausted
// account. proactive selects the trigger line: the poller path burns once the
// window is ProactiveResetUsedRatio used (≤2% remaining), the failure-hook
// path at the plain exhaustion threshold. It returns true only when a card was
// consumed and a fresh quota read confirms the window recovered — the caller
// then skips the switch (the account keeps serving; sessions stay attached
// untouched).
func (a *AutoSwitcher) tryZcodeResetCard(ctx context.Context, account *Account, exhausted []*WindowQuota, proactive bool) bool {
	window := zcodeTokenWindow(exhausted)
	if window == nil {
		return false
	}
	if window.Limit > 0 {
		minUsed := a.Threshold
		if proactive {
			minUsed = a.ProactiveResetUsedRatio
		}
		if float64(window.Used)/float64(window.Limit) < minUsed {
			return false
		}
	}
	// A window about to reset naturally must not burn a card (ResetGrace).
	if resetAt, err := time.Parse(time.RFC3339, window.ResetAt); err == nil {
		if until := resetAt.Sub(a.now()); until > 0 && until <= a.ResetGrace {
			return false
		}
	}
	status, err := zcodeResetStatus(ctx, account)
	if err != nil {
		log.Printf("账号池: zcode 重置卡状态查询失败（账号 %s）：%v", account.ID, err)
		return false
	}
	if status.FiveHourCards < 1 {
		return false
	}
	used, err := useZcodeResetCard(ctx, account, "FIVE_HOUR")
	if err != nil || !used {
		log.Printf("账号池: zcode 重置卡核销失败（账号 %s）：used=%v err=%v", account.ID, used, err)
		return false
	}
	refreshed, rErr := a.Pool.RefreshQuota(ctx, "zcode", account.ID)
	if rErr == nil && refreshed != nil && refreshed.Quota != nil &&
		len(ExhaustedWindows(refreshed.Quota, a.Threshold)) == 0 {
		if proactive {
			a.notify("账号池：已提前用重置卡恢复额度",
				"zcode 账号 "+account.Label+" 的 5 小时额度剩余不足 2%，提前消耗一张重置卡恢复，无需切换账号")
		} else {
			a.notify("账号池：已用重置卡恢复额度",
				"zcode 账号 "+account.Label+" 的 5 小时额度已用尽，消耗一张重置卡恢复，无需切换账号")
		}
		log.Printf("账号池: zcode 账号 %s 消耗重置卡恢复 5 小时额度", account.ID)
		return true
	}
	log.Printf("账号池: zcode 账号 %s 重置卡已核销但额度未恢复，继续走切换", account.ID)
	return false
}

// gate returns the reason a switch must not happen, or "" when clear. All
// gates read history.jsonl so restarts do not reset the protections.
func (a *AutoSwitcher) gate(kind, active string, exhausted []*WindowQuota) string {
	now := a.now()
	history := a.Pool.History(kind, 200)
	autoSwitches := 0
	awayFromSource := 0
	for _, record := range history {
		if !record.OK {
			continue
		}
		at := time.UnixMilli(record.At)
		if record.Trigger == "auto-quota" {
			if now.Sub(at) < a.Cooldown {
				return "切换冷却中"
			}
			if now.Sub(at) < time.Hour {
				autoSwitches++
			}
			if record.From == active && now.Sub(at) < 24*time.Hour {
				awayFromSource++
			}
		}
	}
	if autoSwitches >= a.MaxPerHour {
		return "本小时自动切换次数已达上限"
	}
	if awayFromSource >= a.MaxSwitchesAwayFrom {
		return "当前账号 24h 内已被自动切走多次，停止继续切走"
	}
	for _, window := range exhausted {
		if window.ResetAt == "" {
			continue
		}
		if resetAt, err := time.Parse(time.RFC3339, window.ResetAt); err == nil {
			if until := resetAt.Sub(now); until > 0 && until <= a.ResetGrace {
				return "额度窗口即将重置（" + window.ResetAt + "），等待重置"
			}
		}
	}
	return ""
}
