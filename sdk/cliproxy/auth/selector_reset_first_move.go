package auth

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Defaults for moving a pinned session under reset-first routing.
const (
	DefaultAffinityMoveThreshold = 90.0
	DefaultAffinityMoveMargin    = 24 * time.Hour
)

// AffinityMovePolicy lets a session bound by session affinity leave its credential
// so reset-first keeps spending the quota that resets soonest. A pinned session
// is re-evaluated on every request and moves only when:
//
//   - its credential's weekly window (or the requested model's 7-day window) is
//     at or above Threshold percent used, or exhausted; or
//   - another available credential's weekly window resets at least Margin sooner,
//     is below Threshold, and is not exhausted (including a full 5-hour window).
//
// Otherwise the pin is kept, so the prompt cache survives.
type AffinityMovePolicy struct {
	// Threshold is the percent used (0-100) at which a credential counts as nearly used up.
	Threshold float64
	// Margin is how much sooner another credential must reset to justify a move.
	Margin time.Duration
}

// quotaStanding is what reset-first knows about one credential for one model.
type quotaStanding struct {
	reset       time.Time // soonest known future reset of the weekly or model window
	utilization float64   // highest percent used across those windows; -1 when unknown
	exhausted   bool      // a window (including the 5-hour one) is used up
}

func credentialStanding(auth *Auth, model string, now time.Time) quotaStanding {
	reset, exhausted := quotaWindowReset(auth, model, now)
	return quotaStanding{reset: reset, utilization: weeklyUtilization(auth, model, now), exhausted: exhausted}
}

// weeklyUtilization returns the highest percent used of the weekly window and any
// matching per-model 7-day window, from the poller or the response headers,
// whichever is newer. It returns -1 when neither reports one.
func weeklyUtilization(auth *Auth, model string, now time.Time) float64 {
	if auth == nil {
		return -1
	}
	model = strings.ToLower(model)
	if q := loadPolledQuota(auth.ID); q != nil && (auth.Quota.ObservedAt.IsZero() || !q.observedAt.Before(auth.Quota.ObservedAt)) {
		used := -1.0
		consider := func(w polledWindow) {
			if !w.reset.IsZero() && w.reset.After(now) && w.utilization > used {
				used = w.utilization
			}
		}
		consider(q.weekly)
		for name, w := range q.models {
			if name != "" && strings.Contains(model, name) {
				consider(w)
			}
		}
		if used >= 0 {
			return used
		}
	}
	return headerWeeklyUtilization(auth.Quota.Signals, model, now)
}

// headerWeeklyUtilization reads Anthropic's 7-day utilization headers (a fraction, 0-1).
func headerWeeklyUtilization(raw map[string]string, model string, now time.Time) float64 {
	const prefix = "anthropic-ratelimit-unified-"
	signals := make(map[string]string, len(raw))
	for key, value := range raw {
		signals[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	used := -1.0
	for key, value := range signals {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "-utilization") {
			continue
		}
		window := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "-utilization")
		if window != "7d" {
			name, ok := strings.CutPrefix(window, "7d_")
			if !ok || name == "" || !strings.Contains(model, name) {
				continue
			}
		}
		if reset, ok := parseResetTime(signals[prefix+window+"-reset"]); ok && !reset.After(now) {
			continue
		}
		if u, err := strconv.ParseFloat(value, 64); err == nil && u*100 > used {
			used = u * 100
		}
	}
	return used
}

// affinityMoveTarget decides whether a session bound to bound should move to one
// of candidates. It returns the target and a reason for the log, or nil to keep
// the pin. candidates may include bound; it is skipped.
func affinityMoveTarget(policy *AffinityMovePolicy, bound *Auth, candidates []*Auth, model string, now time.Time) (*Auth, string) {
	if policy == nil || bound == nil || len(candidates) < 2 {
		return nil, ""
	}
	cur := credentialStanding(bound, model, now)
	hasRoom := func(s quotaStanding) bool {
		return !s.exhausted && s.utilization >= 0 && s.utilization < policy.Threshold
	}
	nearlyUsedUp := cur.exhausted || cur.utilization >= policy.Threshold
	for _, cand := range orderResetFirst(candidates, model, now) {
		if cand == nil || cand.ID == bound.ID {
			continue
		}
		next := credentialStanding(cand, model, now)
		if !hasRoom(next) || next.reset.IsZero() {
			continue
		}
		if nearlyUsedUp {
			return cand, fmt.Sprintf("bound credential at %.0f%% of its weekly window (threshold %.0f%%); %s at %.0f%%", cur.utilization, policy.Threshold, cand.ID, next.utilization)
		}
		if !cur.reset.IsZero() && !next.reset.After(cur.reset.Add(-policy.Margin)) {
			return cand, fmt.Sprintf("%s resets %s sooner (%s vs %s) and is at %.0f%%", cand.ID, cur.reset.Sub(next.reset).Round(time.Minute), next.reset.UTC().Format(time.RFC3339), cur.reset.UTC().Format(time.RFC3339), next.utilization)
		}
		// Candidates are ordered soonest reset first; later ones reset even later.
		return nil, ""
	}
	return nil, ""
}
