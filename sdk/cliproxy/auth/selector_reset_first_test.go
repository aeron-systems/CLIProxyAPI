package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeAuth(id string, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: signals}}
}

func unixIn(now time.Time, d time.Duration) string {
	return strconv.FormatInt(now.Add(d).Unix(), 10)
}

func pickIDs(t *testing.T, auths []*Auth, model string, now time.Time) []string {
	t.Helper()
	ordered := orderResetFirst(auths, model, now)
	ids := make([]string, len(ordered))
	for i, a := range ordered {
		ids[i] = a.ID
	}
	return ids
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestResetFirst_ClaudeSoonestWeeklyResetFirst(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		claudeAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 6*24*time.Hour)}),
		claudeAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 2*time.Hour)}),
		claudeAuth("c", nil),
		claudeAuth("d", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 3*24*time.Hour)}),
	}
	assertOrder(t, pickIDs(t, auths, "claude-sonnet-4-5", now), "b", "d", "a", "c")
}

func TestResetFirst_ClaudeModelWindowOnlyForMatchingModel(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		claudeAuth("a", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":        unixIn(now, 5*24*time.Hour),
			"Anthropic-Ratelimit-Unified-7d_sonnet-Reset": unixIn(now, time.Hour),
		}),
		claudeAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 2*24*time.Hour)}),
	}
	assertOrder(t, pickIDs(t, auths, "claude-sonnet-4-5", now), "a", "b")
	assertOrder(t, pickIDs(t, auths, "claude-opus-4-1", now), "b", "a")
}

func TestResetFirst_ExhaustedAndStaleResetsGoLast(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		claudeAuth("exhausted", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":  unixIn(now, time.Hour),
			"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
		}),
		claudeAuth("stale", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, -time.Hour)}),
		claudeAuth("known", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 4*24*time.Hour)}),
	}
	assertOrder(t, pickIDs(t, auths, "claude-sonnet-4-5", now), "known", "stale", "exhausted")
}

func TestResetFirst_CodexUsesLongestWindow(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	observed := now.Add(-time.Minute)
	codex := func(id string, primaryReset, secondaryReset time.Duration) *Auth {
		return &Auth{ID: id, Provider: "codex", Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":        "300",
			"X-Codex-Primary-Reset-At":              unixIn(now, primaryReset),
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(int((secondaryReset + time.Minute).Seconds())),
			"X-Codex-Secondary-Used-Percent":        "40",
		}}}
	}
	auths := []*Auth{
		codex("a", time.Minute, 5*24*time.Hour),
		codex("b", 4*time.Hour, 24*time.Hour),
	}
	assertOrder(t, pickIDs(t, auths, "gpt-5-codex", now), "b", "a")

	full := codex("full", time.Hour, time.Hour)
	full.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "100"
	assertOrder(t, pickIDs(t, append(auths, full), "gpt-5-codex", now), "b", "a", "full")
}

func TestResetFirst_NoQuotaDataMatchesFillFirst(t *testing.T) {
	t.Parallel()
	auths := []*Auth{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	got, err := (&ResetFirstSelector{}).Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	want, _ := (&FillFirstSelector{}).Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if got.ID != want.ID {
		t.Fatalf("Pick() = %q, want fill-first %q", got.ID, want.ID)
	}
}

func TestResetFirst_SkipsCoolingDownCredential(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	model := "claude-sonnet-4-5"
	cooling := claudeAuth("cooling", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, time.Hour)})
	cooling.ModelStates = map[string]*ModelState{model: {
		Status: StatusActive, Unavailable: true, NextRetryAfter: now.Add(10 * time.Minute),
		Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(10 * time.Minute)},
	}}
	ready := claudeAuth("ready", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 3*24*time.Hour)})
	selector := &ResetFirstSelector{nowFunc: func() time.Time { return now }}
	got, err := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, []*Auth{cooling, ready})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "ready" {
		t.Fatalf("Pick() = %q, want ready", got.ID)
	}
}

func TestResetFirst_SessionAffinityBindingWins(t *testing.T) {
	t.Parallel()
	now := time.Now()
	later := claudeAuth("later", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, 5*24*time.Hour)})
	selector := NewSessionAffinitySelector(&ResetFirstSelector{})
	defer selector.Stop()
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_ac980658-63bd-4fb3-97ba-8da64cb1e344"}}`)}

	first, err := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", opts, []*Auth{later})
	if err != nil || first.ID != "later" {
		t.Fatalf("first Pick() = %v, %v", first, err)
	}
	sooner := claudeAuth("sooner", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixIn(now, time.Hour)})
	got, err := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", opts, []*Auth{later, sooner})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "later" {
		t.Fatalf("bound session moved to %q, want later", got.ID)
	}
	fresh, _ := (&ResetFirstSelector{}).Pick(context.Background(), "claude", "claude-sonnet-4-5", cliproxyexecutor.Options{}, []*Auth{later, sooner})
	if fresh.ID != "sooner" {
		t.Fatalf("unbound Pick() = %q, want sooner", fresh.ID)
	}
}
