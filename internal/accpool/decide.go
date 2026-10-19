package accpool

import (
	"context"
)

// primaryWindows lists the windows that gate AI work: the regular limits plus
// the weekly window, minus auxiliary side quotas (see WindowQuota.Auxiliary).
func primaryWindows(quota *Quota) []*WindowQuota {
	if quota == nil {
		return nil
	}
	var out []*WindowQuota
	for _, window := range quota.Limits {
		if window != nil && !window.Auxiliary {
			out = append(out, window)
		}
	}
	if quota.Weekly != nil && !quota.Weekly.Auxiliary {
		out = append(out, quota.Weekly)
	}
	return out
}

// ExhaustedWindows returns the quota windows at or over the threshold
// (used/limit >= threshold). Windows without a usable limit carry no signal.
func ExhaustedWindows(quota *Quota, threshold float64) []*WindowQuota {
	var out []*WindowQuota
	for _, window := range primaryWindows(quota) {
		if window.Limit <= 0 {
			continue
		}
		if float64(window.Used)/float64(window.Limit) >= threshold {
			out = append(out, window)
		}
	}
	return out
}

// availableForSwitch reports whether the account can absorb work right now:
// no window at or over the switch threshold and the quota snapshot fresh
// enough to trust. kimi accounts need a refresh token to be renewable; zcode
// accounts need their snapshot pair.
func availableForSwitch(kind string, acc *Account, threshold float64) bool {
	if acc.Status != StatusActive {
		return false
	}
	if kind == "kimi" && acc.RefreshToken == "" {
		return false
	}
	if kind == "zcode" && (acc.Credentials == "" || acc.Config == "") {
		return false
	}
	if acc.Quota == nil || acc.QuotaUpdatedAt == 0 {
		// No data means "unknown", not "exhausted": an account that was never
		// polled stays eligible, the switch probe is the real check.
		return true
	}
	return len(ExhaustedWindows(acc.Quota, threshold)) == 0
}

// ChooseTarget picks the switch target: every candidate is re-polled (backup
// accounts are otherwise never refreshed), then the winner is the account
// whose tightest window has the most remaining headroom; ties go to the
// account unused for the longest time.
func (m *Manager) ChooseTarget(ctx context.Context, kind, currentID string, threshold float64) *Account {
	var best *Account
	bestHeadroom := -1.0
	for _, candidate := range m.list(kind) {
		if candidate.ID == currentID {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		// Refresh failures (dead token, network) simply drop the candidate;
		// RefreshQuota has already marked reauth_required where applicable.
		fresh, err := m.RefreshQuota(ctx, kind, candidate.ID)
		if err != nil && fresh.Quota == nil {
			continue
		}
		if !availableForSwitch(kind, fresh, threshold) {
			continue
		}
		headroom := 1.0
		if fresh.Quota != nil {
			for _, window := range primaryWindows(fresh.Quota) {
				if window.Limit <= 0 {
					continue
				}
				remaining := 1.0 - float64(window.Used)/float64(window.Limit)
				if remaining < headroom {
					headroom = remaining
				}
			}
		}
		if best == nil || headroom > bestHeadroom ||
			(headroom == bestHeadroom && candidate.LastUsedAt < best.LastUsedAt) {
			best, bestHeadroom = fresh, headroom
		}
	}
	return best
}
