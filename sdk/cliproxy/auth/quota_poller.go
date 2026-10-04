package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The reset-first selector needs each account's usage windows before the
// account has served a request (response headers only exist afterwards). The
// quota poller fetches them from the provider's usage endpoint on a timer and
// right after a 429, so routing never depends on a dashboard being open.

const (
	claudeUsageURL         = "https://api.anthropic.com/api/oauth/usage"
	defaultQuotaPollPeriod = 5 * time.Minute
)

// polledQuota is the last usage snapshot fetched for one credential.
type polledQuota struct {
	observedAt time.Time
	weekly     polledWindow
	fiveHour   polledWindow
	// models holds per-model 7-day windows keyed by lower-case model family
	// (for example "opus", "sonnet").
	models map[string]polledWindow
}

type polledWindow struct {
	reset     time.Time
	exhausted bool
}

var polledQuotas sync.Map // auth ID -> *polledQuota

func storePolledQuota(authID string, q *polledQuota) { polledQuotas.Store(authID, q) }

func loadPolledQuota(authID string) *polledQuota {
	if v, ok := polledQuotas.Load(authID); ok {
		return v.(*polledQuota)
	}
	return nil
}

// polledWindowReset reports the soonest future reset of the binding weekly
// window (and a matching per-model window) and whether any window is used up.
func polledWindowReset(authID, model string, now time.Time) (time.Time, bool, time.Time) {
	q := loadPolledQuota(authID)
	if q == nil {
		return time.Time{}, false, time.Time{}
	}
	var soonest time.Time
	exhausted := false
	consider := func(w polledWindow) {
		if w.reset.IsZero() || !w.reset.After(now) {
			return
		}
		if w.exhausted {
			exhausted = true
		}
		if soonest.IsZero() || w.reset.Before(soonest) {
			soonest = w.reset
		}
	}
	consider(q.weekly)
	model = strings.ToLower(model)
	for name, w := range q.models {
		if name != "" && strings.Contains(model, name) {
			consider(w)
		}
	}
	if q.fiveHour.exhausted && q.fiveHour.reset.After(now) {
		exhausted = true
	}
	return soonest, exhausted, q.observedAt
}

// parseClaudeUsage reads the /api/oauth/usage payload. Utilization is a
// percentage (0-100).
func parseClaudeUsage(body []byte, observedAt time.Time) (*polledQuota, bool) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	read := func(key string) (polledWindow, bool) {
		var w window
		if msg, ok := raw[key]; !ok || json.Unmarshal(msg, &w) != nil || w.ResetsAt == nil {
			return polledWindow{}, false
		}
		reset, ok := parseResetTime(*w.ResetsAt)
		if !ok {
			return polledWindow{}, false
		}
		return polledWindow{reset: reset, exhausted: w.Utilization != nil && *w.Utilization >= 100}, true
	}
	q := &polledQuota{observedAt: observedAt, models: map[string]polledWindow{}}
	weekly, okWeekly := read("seven_day")
	q.weekly = weekly
	q.fiveHour, _ = read("five_hour")
	for key := range raw {
		if name, ok := strings.CutPrefix(key, "seven_day_"); ok && name != "oauth_apps" && name != "cowork" {
			if w, okW := read(key); okW {
				q.models[name] = w
			}
		}
	}
	// limits[] carries per-model weekly windows by display name.
	var limits struct {
		Limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt *string  `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if json.Unmarshal(body, &limits) == nil {
		for _, l := range limits.Limits {
			if l.Kind != "weekly_scoped" || l.ResetsAt == nil || l.Scope == nil || l.Scope.Model == nil {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(l.Scope.Model.DisplayName))
			reset, ok := parseResetTime(*l.ResetsAt)
			if name == "" || !ok {
				continue
			}
			q.models[name] = polledWindow{reset: reset, exhausted: l.Percent != nil && *l.Percent >= 100}
		}
	}
	return q, okWeekly || !q.fiveHour.reset.IsZero() || len(q.models) > 0
}

// StartQuotaPoller refreshes usage windows for Claude OAuth credentials every
// period, and for one credential right after it returns a 429. It only does
// work while routing.strategy is reset-first.
func (m *Manager) StartQuotaPoller(ctx context.Context, period time.Duration) {
	if m == nil {
		return
	}
	if period <= 0 {
		period = defaultQuotaPollPeriod
	}
	m.quotaKickOnce.Do(func() { m.quotaKick = make(chan string, 64) })
	go func() {
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		m.pollQuotas(ctx, "")
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.pollQuotas(ctx, "")
			case authID := <-m.quotaKick:
				m.pollQuotas(ctx, authID)
			}
		}
	}()
}

// kickQuotaRefresh asks the poller to refresh one credential soon.
func (m *Manager) kickQuotaRefresh(authID string) {
	if m == nil || m.quotaKick == nil || authID == "" {
		return
	}
	select {
	case m.quotaKick <- authID:
	default:
	}
}

func (m *Manager) quotaPollingWanted() bool {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy)) {
	case "reset-first", "resetfirst", "rf":
		return true
	}
	return false
}

func (m *Manager) pollQuotas(ctx context.Context, onlyAuthID string) {
	if !m.quotaPollingWanted() || ctx.Err() != nil {
		return
	}
	for _, auth := range m.List() {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if onlyAuthID != "" && auth.ID != onlyAuthID {
			continue
		}
		if !strings.EqualFold(auth.Provider, "claude") || auth.AuthKind() != AuthKindOAuth {
			continue
		}
		m.pollClaudeQuota(ctx, auth)
	}
}

func (m *Manager) pollClaudeQuota(ctx context.Context, auth *Auth) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	resp, err := m.HttpRequest(reqCtx, auth, req)
	if err != nil {
		log.WithField("credential", auth.ID).Debugf("quota poll failed: %v", err)
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("quota poll: close body: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		log.WithFields(log.Fields{"credential": auth.ID, "status": resp.StatusCode}).Debug("quota poll: no usage data")
		return
	}
	if q, ok := parseClaudeUsage(body, time.Now()); ok {
		storePolledQuota(auth.ID, q)
		log.WithFields(log.Fields{"credential": auth.ID, "weekly_reset": q.weekly.reset.Format(time.RFC3339)}).Debug("quota poll: updated")
	}
}
