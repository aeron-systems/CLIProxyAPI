package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var defaultMovePolicy = &AffinityMovePolicy{Threshold: DefaultAffinityMoveThreshold, Margin: DefaultAffinityMoveMargin}

// pollQuotaForTest stores a polled weekly (and optional 5-hour) window and removes it after the test.
func pollQuotaForTest(t *testing.T, id string, now time.Time, weeklyIn time.Duration, used float64, fiveHourFull bool) {
	t.Helper()
	q := &polledQuota{observedAt: now, models: map[string]polledWindow{},
		weekly: polledWindow{reset: now.Add(weeklyIn), utilization: used, exhausted: used >= 100}}
	if fiveHourFull {
		q.fiveHour = polledWindow{reset: now.Add(2 * time.Hour), utilization: 100, exhausted: true}
	}
	storePolledQuota(id, q)
	t.Cleanup(func() { polledQuotas.Delete(id) })
}

func moveTargetID(policy *AffinityMovePolicy, bound *Auth, candidates []*Auth, now time.Time) (string, string) {
	target, reason := affinityMoveTarget(policy, bound, candidates, "claude-opus-5-5", now)
	if target == nil {
		return "", reason
	}
	return target.ID, reason
}

func TestAffinityMove_NearlyUsedUpMovesToSoonestWithRoom(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pollQuotaForTest(t, "mv1-bound", now, 2*24*time.Hour, 92, false)
	pollQuotaForTest(t, "mv1-later", now, 6*24*time.Hour, 30, false)
	pollQuotaForTest(t, "mv1-full", now, 1*24*time.Hour, 95, false)
	bound, later, full := &Auth{ID: "mv1-bound"}, &Auth{ID: "mv1-later"}, &Auth{ID: "mv1-full"}

	// The bound credential resets soonest of the usable ones, but it is past 90%.
	got, reason := moveTargetID(defaultMovePolicy, bound, []*Auth{bound, later, full}, now)
	if got != "mv1-later" {
		t.Fatalf("target = %q, want mv1-later", got)
	}
	if !strings.Contains(reason, "92%") {
		t.Fatalf("reason %q should name the bound credential's usage", reason)
	}
}

func TestAffinityMove_ExhaustedBoundMoves(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pollQuotaForTest(t, "mv2-bound", now, 3*24*time.Hour, 100, false)
	pollQuotaForTest(t, "mv2-other", now, 5*24*time.Hour, 10, false)
	bound, other := &Auth{ID: "mv2-bound"}, &Auth{ID: "mv2-other"}
	if got, _ := moveTargetID(defaultMovePolicy, bound, []*Auth{bound, other}, now); got != "mv2-other" {
		t.Fatalf("target = %q, want mv2-other", got)
	}
}

func TestAffinityMove_SoonerResetBeyondMarginMoves(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// The Studio on 2026-10-10: a session pinned to the account resetting in 6.5 days
	// while another resets in 2.6 days with 75% left.
	pollQuotaForTest(t, "mv3-max", now, 156*time.Hour, 61, false)
	pollQuotaForTest(t, "mv3-team", now, 63*time.Hour, 25, false)
	bound, team := &Auth{ID: "mv3-max"}, &Auth{ID: "mv3-team"}
	got, reason := moveTargetID(defaultMovePolicy, bound, []*Auth{bound, team}, now)
	if got != "mv3-team" {
		t.Fatalf("target = %q, want mv3-team", got)
	}
	if !strings.Contains(reason, "sooner") {
		t.Fatalf("reason %q should say the target resets sooner", reason)
	}
}

func TestAffinityMove_KeepsPin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pollQuotaForTest(t, "mv4-bound", now, 3*24*time.Hour, 50, false)
	pollQuotaForTest(t, "mv4-within-margin", now, 3*24*time.Hour-20*time.Hour, 10, false)
	pollQuotaForTest(t, "mv4-sooner-but-full", now, 1*24*time.Hour, 90, false)
	pollQuotaForTest(t, "mv4-sooner-5h-full", now, 1*24*time.Hour, 20, true)
	pollQuotaForTest(t, "mv4-later", now, 6*24*time.Hour, 0, false)
	bound := &Auth{ID: "mv4-bound"}
	unknown := &Auth{ID: "mv4-unknown"}

	cases := map[string][]*Auth{
		"reset within the margin":      {bound, {ID: "mv4-within-margin"}},
		"sooner one at the threshold":  {bound, {ID: "mv4-sooner-but-full"}},
		"sooner one at its 5-hour cap": {bound, {ID: "mv4-sooner-5h-full"}},
		"only later resets":            {bound, {ID: "mv4-later"}},
		"other has no quota data":      {bound, unknown},
		"bound alone":                  {bound},
		"all of the above at once":     {bound, {ID: "mv4-within-margin"}, {ID: "mv4-sooner-but-full"}, {ID: "mv4-sooner-5h-full"}, {ID: "mv4-later"}, unknown},
	}
	for name, candidates := range cases {
		if got, reason := moveTargetID(defaultMovePolicy, bound, candidates, now); got != "" {
			t.Errorf("%s: moved to %q (%s), want the pin kept", name, got, reason)
		}
	}
}

func TestAffinityMove_NearlyUsedUpButNowhereBetterKeepsPin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pollQuotaForTest(t, "mv5-bound", now, 2*24*time.Hour, 95, false)
	pollQuotaForTest(t, "mv5-other", now, 4*24*time.Hour, 97, false)
	bound := &Auth{ID: "mv5-bound"}
	if got, _ := moveTargetID(defaultMovePolicy, bound, []*Auth{bound, {ID: "mv5-other"}}, now); got != "" {
		t.Fatalf("moved to %q, want the pin kept when no credential has room", got)
	}
}

func TestAffinityMove_ConfigurableThresholdAndMargin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pollQuotaForTest(t, "mv6-bound", now, 3*24*time.Hour, 80, false)
	pollQuotaForTest(t, "mv6-other", now, 3*24*time.Hour-2*time.Hour, 10, false)
	bound, other := &Auth{ID: "mv6-bound"}, &Auth{ID: "mv6-other"}
	if got, _ := moveTargetID(defaultMovePolicy, bound, []*Auth{bound, other}, now); got != "" {
		t.Fatalf("defaults moved to %q, want the pin kept", got)
	}
	if got, _ := moveTargetID(&AffinityMovePolicy{Threshold: 75, Margin: 24 * time.Hour}, bound, []*Auth{bound, other}, now); got != "mv6-other" {
		t.Fatalf("threshold 75: target = %q, want mv6-other", got)
	}
	if got, _ := moveTargetID(&AffinityMovePolicy{Threshold: 90, Margin: time.Hour}, bound, []*Auth{bound, other}, now); got != "mv6-other" {
		t.Fatalf("margin 1h: target = %q, want mv6-other", got)
	}
}

func TestAffinityMove_HeaderUtilizationUsedWithoutPoll(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	bound := claudeAuth("mv7-bound", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       unixIn(now, 2*24*time.Hour),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.93",
	})
	other := claudeAuth("mv7-other", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       unixIn(now, 5*24*time.Hour),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.2",
	})
	if got, _ := moveTargetID(defaultMovePolicy, bound, []*Auth{bound, other}, now); got != "mv7-other" {
		t.Fatalf("target = %q, want mv7-other", got)
	}
}

func TestSessionAffinitySelector_ResetFirstMovesPinnedSession(t *testing.T) {
	now := time.Now()
	pollQuotaForTest(t, "mv8-later", now, 6*24*time.Hour, 10, false)
	pollQuotaForTest(t, "mv8-sooner", now, 2*24*time.Hour, 20, false)
	later := &Auth{ID: "mv8-later", Provider: "claude", Status: StatusActive}
	sooner := &Auth{ID: "mv8-sooner", Provider: "claude", Status: StatusActive}

	pick := func(selector *SessionAffinitySelector, auths []*Auth) string {
		t.Helper()
		headers := http.Header{}
		headers.Set("X-Claude-Code-Session-Id", "11111111-2222-3333-4444-555555555555")
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{Headers: headers}, auths)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		return got.ID
	}

	for _, tc := range []struct {
		name   string
		policy *AffinityMovePolicy
		want   string
	}{
		{name: "move on", policy: defaultMovePolicy, want: "mv8-sooner"},
		{name: "move off", policy: nil, want: "mv8-later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback: &ResetFirstSelector{}, TTL: time.Hour, MovePolicy: tc.policy,
			})
			defer selector.Stop()
			// Bind while only the later-reset account is signed in (like a session
			// started before a new account was added), then add the sooner one.
			if got := pick(selector, []*Auth{later}); got != "mv8-later" {
				t.Fatalf("first pick = %q, want mv8-later", got)
			}
			if got := pick(selector, []*Auth{later, sooner}); got != tc.want {
				t.Fatalf("second pick = %q, want %q", got, tc.want)
			}
			// Once moved (or kept), the session stays where it is.
			if got := pick(selector, []*Auth{later, sooner}); got != tc.want {
				t.Fatalf("third pick = %q, want %q", got, tc.want)
			}
		})
	}
}
