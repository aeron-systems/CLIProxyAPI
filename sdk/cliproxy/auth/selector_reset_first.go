package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector prefers the available credential whose longest observed
// usage window (the weekly window, and for Claude a matching per-model 7-day
// window) resets soonest, so quota that would otherwise expire unused is spent
// first.
//
// The reset times come from the passive quota snapshot (QuotaState.Signals)
// recorded from upstream response headers. Credentials whose snapshot reports
// an exhausted window are tried last. Credentials without a known future reset
// keep fill-first order after every credential that has one, so with no quota
// data at all the selector behaves exactly like FillFirstSelector.
type ResetFirstSelector struct {
	// nowFunc overrides the clock in tests.
	nowFunc func() time.Time
}

// Pick selects the available credential with the soonest known window reset.
func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return orderResetFirst(available, model, now)[0], nil
}

// orderResetFirst returns a stable reordering of auths: usable before
// exhausted, known reset before unknown, then soonest reset first.
func orderResetFirst(auths []*Auth, model string, now time.Time) []*Auth {
	type ranked struct {
		auth      *Auth
		reset     time.Time
		exhausted bool
	}
	items := make([]ranked, len(auths))
	for i, auth := range auths {
		reset, exhausted := quotaWindowReset(auth, model, now)
		items[i] = ranked{auth: auth, reset: reset, exhausted: exhausted}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.exhausted != b.exhausted {
			return !a.exhausted
		}
		if a.reset.IsZero() != b.reset.IsZero() {
			return !a.reset.IsZero()
		}
		return a.reset.Before(b.reset)
	})
	out := make([]*Auth, len(items))
	for i := range items {
		out[i] = items[i].auth
	}
	return out
}

// quotaWindowReset reports the next reset of the credential's longest usage
// window and whether the snapshot says a window is currently exhausted. A zero
// time means the reset is unknown or already in the past.
func quotaWindowReset(auth *Auth, model string, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	reset, exhausted := observedWindowReset(auth, model, now)
	polledReset, polledExhausted, polledAt := polledWindowReset(auth.ID, model, now)
	// Prefer the newer of the passive response snapshot and the polled usage.
	if !polledReset.IsZero() && (reset.IsZero() || polledAt.After(auth.Quota.ObservedAt)) {
		reset = polledReset
	}
	return reset, exhausted || polledExhausted
}

// observedWindowReset reads the passive snapshot taken from response headers.
func observedWindowReset(auth *Auth, model string, now time.Time) (time.Time, bool) {
	if len(auth.Quota.Signals) == 0 {
		return time.Time{}, false
	}
	signals := make(map[string]string, len(auth.Quota.Signals))
	for key, value := range auth.Quota.Signals {
		signals[strings.ToLower(strings.TrimSpace(key))] = strings.ToLower(strings.TrimSpace(value))
	}
	if hasSignal(signals, "x-codex-primary-window-minutes") || hasSignal(signals, "x-codex-secondary-window-minutes") {
		return codexWindowReset(signals, auth.Quota.ObservedAt, now)
	}
	return claudeWindowReset(signals, model, now)
}

func hasSignal(signals map[string]string, key string) bool {
	_, ok := signals[key]
	return ok
}

// claudeWindowReset reads the Anthropic unified 7-day window and any 7-day
// per-model window whose name appears in the requested model.
func claudeWindowReset(signals map[string]string, model string, now time.Time) (time.Time, bool) {
	const prefix = "anthropic-ratelimit-unified-"
	model = strings.ToLower(model)
	var soonest time.Time
	exhausted := false
	for key, value := range signals {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "-reset") {
			continue
		}
		window := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "-reset")
		if window != "7d" {
			name, ok := strings.CutPrefix(window, "7d_")
			if !ok || name == "" || !strings.Contains(model, name) {
				continue
			}
		}
		reset, ok := parseResetTime(value)
		if !ok || !reset.After(now) {
			continue
		}
		if signals[prefix+window+"-status"] == "rejected" {
			exhausted = true
		}
		if soonest.IsZero() || reset.Before(soonest) {
			soonest = reset
		}
	}
	fiveHourFull := signals[prefix+"5h-status"] == "rejected"
	if u, errU := strconv.ParseFloat(signals[prefix+"5h-utilization"], 64); errU == nil && u >= 1 {
		fiveHourFull = true
	}
	if reset, ok := parseResetTime(signals[prefix+"5h-reset"]); ok && reset.After(now) && fiveHourFull {
		exhausted = true
	}
	return soonest, exhausted
}

// codexWindowReset reads the Codex primary/secondary window with the longest
// duration (the weekly window).
func codexWindowReset(signals map[string]string, observedAt, now time.Time) (time.Time, bool) {
	var best time.Time
	var bestMinutes int64 = -1
	exhausted := false
	for _, window := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + window + "-"
		minutes, errMinutes := strconv.ParseInt(signals[prefix+"window-minutes"], 10, 64)
		if errMinutes != nil {
			continue
		}
		reset, ok := parseResetTime(signals[prefix+"reset-at"])
		if !ok && !observedAt.IsZero() {
			if after, errAfter := strconv.ParseInt(signals[prefix+"reset-after-seconds"], 10, 64); errAfter == nil && after >= 0 {
				reset, ok = observedAt.Add(time.Duration(after)*time.Second), true
			}
		}
		if !ok || !reset.After(now) {
			continue
		}
		if used, errUsed := strconv.ParseFloat(signals[prefix+"used-percent"], 64); errUsed == nil && used >= 100 {
			exhausted = true
		}
		if minutes > bestMinutes {
			best, bestMinutes = reset, minutes
		}
	}
	if !best.IsZero() && (signals["x-codex-limit-reached"] == "true" || signals["x-codex-allowed"] == "false") {
		exhausted = true
	}
	return best, exhausted
}

// parseResetTime accepts Unix seconds, Unix milliseconds, or RFC 3339.
func parseResetTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(raw)); err == nil {
		return t, true
	}
	return time.Time{}, false
}
