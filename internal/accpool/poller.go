package accpool

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"
)

// Poller refreshes quotas in the background. To bound requests against the
// official API (design doc open question 4), only the account occupying the
// official slot is polled on the interval; backup accounts refresh on demand
// (explicit refresh or a future switch).
type Poller struct {
	Manager  *Manager
	Interval time.Duration
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

const defaultPollInterval = 5 * time.Minute

// NewPoller reads GATEWAY_POOL_POLL_SECONDS (0 or absent = 5 minutes, a
// negative value disables polling entirely).
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
	return &Poller{Manager: m, Interval: interval}
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
	timer := time.NewTimer(p.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			p.tick(ctx)
			timer.Reset(p.Interval)
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
