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
