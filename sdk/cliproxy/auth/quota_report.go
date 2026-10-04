package auth

import (
	"context"
	"sort"
	"strings"
	"time"
)

// QuotaWindow is one usage window as last polled from the provider.
type QuotaWindow struct {
	Name        string    `json:"name"`
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
	Exhausted   bool      `json:"exhausted"`
}

// QuotaCredential is one signed-in account, its windows and whether it can take work now.
type QuotaCredential struct {
	ID           string        `json:"id"`
	Account      string        `json:"account"`
	Provider     string        `json:"provider"`
	Pools        []string      `json:"pools"`
	Disabled     bool          `json:"disabled"`
	Unavailable  bool          `json:"unavailable"`
	CoolingUntil *time.Time    `json:"cooling_until,omitempty"`
	Status       string        `json:"status"`
	StatusText   string        `json:"status_message,omitempty"`
	ObservedAt   *time.Time    `json:"observed_at,omitempty"`
	FiveHour     *QuotaWindow  `json:"five_hour,omitempty"`
	SevenDay     *QuotaWindow  `json:"seven_day,omitempty"`
	Models       []QuotaWindow `json:"models"`
	// Usable: the selector would consider this account for a new session right now.
	Usable bool `json:"usable"`
}

// QuotaView answers "what does each account have left, and where would new work go".
// Read-only: it never polls or changes routing state.
type QuotaView struct {
	Model       string            `json:"model"`
	Pool        string            `json:"pool"`
	Strategy    string            `json:"strategy"`
	Next        string            `json:"next,omitempty"`
	Order       []string          `json:"order"`
	Credentials []QuotaCredential `json:"credentials"`
}

func windowOf(name string, w polledWindow, now time.Time) *QuotaWindow {
	if w.reset.IsZero() {
		return nil
	}
	return &QuotaWindow{Name: name, Utilization: w.utilization, ResetsAt: w.reset, Exhausted: w.exhausted && w.reset.After(now)}
}

// BuildQuotaView reports every credential's polled windows and the order the
// reset-first selector would try them in for a new session of model in pool.
// An empty pool means every credential.
func BuildQuotaView(auths []*Auth, pool, model, strategy string, now time.Time) QuotaView {
	pools := CurrentCredentialPools()
	view := QuotaView{Model: model, Pool: pool, Strategy: strategy, Order: []string{}, Credentials: []QuotaCredential{}}
	candidates := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		_, account := a.AccountInfo()
		qc := QuotaCredential{
			ID: a.ID, Account: account, Provider: a.Provider, Pools: []string{},
			Disabled: a.Disabled || a.Status == StatusDisabled, Unavailable: a.Unavailable,
			Status: string(a.Status), StatusText: a.StatusMessage, Models: []QuotaWindow{},
		}
		if pools != nil {
			for _, def := range pools.Definitions() {
				if pools.Allows(def.Name, a) {
					qc.Pools = append(qc.Pools, def.Name)
				}
			}
			if pools.Allows("shared", a) && !containsString(qc.Pools, "shared") {
				qc.Pools = append(qc.Pools, "shared")
			}
		}
		if !a.NextRetryAfter.IsZero() && a.NextRetryAfter.After(now) {
			t := a.NextRetryAfter
			qc.CoolingUntil = &t
		}
		if q := loadPolledQuota(a.ID); q != nil {
			t := q.observedAt
			qc.ObservedAt = &t
			qc.FiveHour = windowOf("five_hour", q.fiveHour, now)
			qc.SevenDay = windowOf("seven_day", q.weekly, now)
			names := make([]string, 0, len(q.models))
			for name := range q.models {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if w := windowOf(name, q.models[name], now); w != nil {
					qc.Models = append(qc.Models, *w)
				}
			}
		}
		view.Credentials = append(view.Credentials, qc)
		if pool == "" || pools == nil || pools.Allows(pool, a) {
			candidates = append(candidates, a)
		}
	}
	available, err := getAvailableAuthsAcrossPriorities(candidates, "claude", model, now)
	if err == nil {
		claude := make([]*Auth, 0, len(available))
		for _, a := range available {
			if strings.EqualFold(a.Provider, "claude") {
				claude = append(claude, a)
			}
		}
		sort.SliceStable(claude, func(i, j int) bool { return claude[i].ID < claude[j].ID })
		for _, a := range orderResetFirst(claude, model, now) {
			view.Order = append(view.Order, a.ID)
		}
	}
	usable := map[string]bool{}
	for i, id := range view.Order {
		usable[id] = true
		if i == 0 {
			view.Next = id
		}
	}
	for i := range view.Credentials {
		view.Credentials[i].Usable = usable[view.Credentials[i].ID]
	}
	// Exhausted accounts are ordered last but are not usable for new work.
	for _, a := range candidates {
		if _, exhausted := quotaWindowReset(a, model, now); exhausted {
			for i := range view.Credentials {
				if view.Credentials[i].ID == a.ID {
					view.Credentials[i].Usable = false
				}
			}
			if view.Next == a.ID {
				view.Next = ""
			}
		}
	}
	return view
}

// QuotaView is BuildQuotaView over the manager's current credentials.
func (m *Manager) QuotaView(_ context.Context, pool, model, strategy string) QuotaView {
	if m == nil {
		return BuildQuotaView(nil, pool, model, strategy, time.Now())
	}
	return BuildQuotaView(m.List(), pool, model, strategy, time.Now())
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
