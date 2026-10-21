package accpool

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"
)

// Poller refreshes quotas in the background. To bound requests against the
// official API (design doc open question 4), the fast loop polls only the
// account occupying the official slot on Interval; the slow loop sweeps every
// pooled account whose snapshot is older than AllInterval, so backup accounts
// stay fresh enough to trust (and display) without a manual refresh.
type Poller struct {
	Manager  *Manager
	Interval time.Duration
	// AllInterval is the slow full-pool sweep cadence; a negative value
	// disables the sweep (only the active account is polled). Snapshots
	// younger than AllInterval are skipped, so restarts don't re-hit fresh
	// accounts.
	AllInterval time.Duration
	// Threshold is the used/limit ratio that counts as exhausted for the
	// preventive-switch check (0 disables the check).
	Threshold float64
	// OnExhausted fires (on the poll goroutine) when the active account
	// crosses Threshold; the auto-switcher owns the reaction.
	OnExhausted func(kind string)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

const (
	defaultPollInterval    = 5 * time.Minute
	defaultPollAllInterval = time.Hour
)

// NewPoller reads GATEWAY_POOL_POLL_SECONDS (0 or absent = 5 minutes; a
// negative value disables polling entirely) and GATEWAY_POOL_POLL_ALL_SECONDS
// (0 or absent = 1 hour; a negative value disables the full-pool sweep).
func NewPoller(m *Manager) *Poller {
	interval := defaultPollInterval
	if raw := os.Getenv("GATEWAY_POOL_POLL_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil {
			if seconds < 0 {
				interval = -1
			} else if seconds > 0 {
				interval = time.Duration(seconds) * time.Second
			}
		}
	}
	allInterval := defaultPollAllInterval
	if raw := os.Getenv("GATEWAY_POOL_POLL_ALL_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil {
			if seconds < 0 {
				allInterval = -1
			} else if seconds > 0 {
				allInterval = time.Duration(seconds) * time.Second
			}
		}
	}
	return &Poller{Manager: m, Interval: interval, AllInterval: allInterval}
}

// Start launches the refresh loop; it is a no-op when polling is disabled.
func (p *Poller) Start() {
	if p == nil || p.Interval <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.loop(ctx, p.done)
}

func (p *Poller) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (p *Poller) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	p.tick(ctx)
	p.tickAll(ctx)
	fast := time.NewTimer(p.Interval)
	defer fast.Stop()
	var all *time.Timer
	var allC <-chan time.Time
	if p.AllInterval > 0 {
		all = time.NewTimer(p.AllInterval)
		allC = all.C
		defer all.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			p.tick(ctx)
			fast.Reset(p.Interval)
		case <-allC:
			p.tickAll(ctx)
			all.Reset(p.AllInterval)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	for _, kind := range p.Manager.Kinds() {
		active := p.Manager.activeID(kind)
		if active == "" {
			continue
		}
		// A failed refresh (network, auth) is recorded on the account by
		// RefreshQuota itself; the loop keeps going either way.
		account, _ := p.Manager.RefreshQuota(ctx, kind, active)
		if account != nil && p.OnExhausted != nil && p.Threshold > 0 &&
			len(ExhaustedWindows(account.Quota, p.Threshold)) > 0 {
			p.OnExhausted(kind)
		}
	}
}

// tickAll sweeps every pooled account whose quota snapshot is older than
// AllInterval (snapshots fresh from the fast loop are skipped). Failures land
// on the account as quotaError, same as tick; the sweep never fires
// OnExhausted — the preventive switcher only watches the active account.
func (p *Poller) tickAll(ctx context.Context) {
	if p.AllInterval <= 0 {
		return
	}
	cutoff := p.Manager.now().Add(-p.AllInterval)
	for _, kind := range p.Manager.Kinds() {
		for _, acc := range p.Manager.list(kind) {
			if acc.QuotaUpdatedAt > 0 && time.UnixMilli(acc.QuotaUpdatedAt).After(cutoff) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			_, _ = p.Manager.RefreshQuota(ctx, kind, acc.ID)
		}
	}
}
