package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// usageExecutor answers the Claude usage endpoint with a per-credential payload.
type usageExecutor struct{ bodies map[string]string }

func (usageExecutor) Identifier() string { return "claude" }
func (usageExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}
func (usageExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not used")
}
func (usageExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (usageExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}
func (e usageExecutor) HttpRequest(_ context.Context, a *Auth, req *http.Request) (*http.Response, error) {
	if req.URL.String() != claudeUsageURL {
		return nil, fmt.Errorf("unexpected url %s", req.URL)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(e.bodies[a.ID]))}, nil
}

func usageBody(fiveHourUtil float64, weeklyReset time.Time) string {
	return fmt.Sprintf(`{"five_hour":{"utilization":%g,"resets_at":%q},"seven_day":{"utilization":10.0,"resets_at":%q},"seven_day_opus":null,
	"limits":[{"kind":"weekly_scoped","percent":0,"resets_at":%q,"scope":{"model":{"id":null,"display_name":"Fable"}}}]}`,
		fiveHourUtil, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339), weeklyReset.UTC().Format(time.RFC3339), weeklyReset.UTC().Format(time.RFC3339))
}

// No dashboard open and no request served yet: only the poller knows the resets.
func TestQuotaPoller_ResetFirstWithoutDashboardOrTraffic(t *testing.T) {
	now := time.Now()
	ids := []string{"poll-navigation", "poll-aiml", "poll-eric"}
	resets := map[string]time.Time{
		"poll-navigation": now.Add(6 * 24 * time.Hour),
		"poll-aiml":       now.Add(4 * 24 * time.Hour),
		"poll-eric":       now.Add(7 * 24 * time.Hour),
	}
	exec := usageExecutor{bodies: map[string]string{}}
	m := NewManager(nil, &ResetFirstSelector{}, nil)
	m.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "reset-first"}})
	m.RegisterExecutor(exec)
	var auths []*Auth
	for _, id := range ids {
		exec.bodies[id] = usageBody(5, resets[id])
		a := &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"type": "claude", "email": id}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		auths = append(auths, a)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			polledQuotas.Delete(id)
		}
	})

	m.pollQuotas(context.Background(), "")

	got, err := (&ResetFirstSelector{}).Pick(context.Background(), "claude", "claude-haiku-4-5", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "poll-aiml" {
		t.Fatalf("Pick() = %v, %v; want poll-aiml (soonest weekly reset)", got, err)
	}

	// A used-up 5-hour window sends the account to the back for new sessions.
	exec.bodies["poll-aiml"] = usageBody(100, resets["poll-aiml"])
	m.pollQuotas(context.Background(), "poll-aiml")
	got, _ = (&ResetFirstSelector{}).Pick(context.Background(), "claude", "claude-haiku-4-5", cliproxyexecutor.Options{}, auths)
	if got.ID != "poll-navigation" {
		t.Fatalf("Pick() = %s, want poll-navigation while poll-aiml's 5h window is used up", got.ID)
	}
}

func TestQuotaPoller_ExistingSessionKeepsItsAccount(t *testing.T) {
	now := time.Now()
	t.Cleanup(func() { polledQuotas.Delete("sess-a"); polledQuotas.Delete("sess-b") })
	storePolledQuota("sess-a", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(5 * 24 * time.Hour)}})
	storePolledQuota("sess-b", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(6 * 24 * time.Hour)}})
	a := &Auth{ID: "sess-a", Provider: "claude"}
	b := &Auth{ID: "sess-b", Provider: "claude"}
	selector := NewSessionAffinitySelector(&ResetFirstSelector{})
	defer selector.Stop()
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_bc980658-63bd-4fb3-97ba-8da64cb1e344"}}`)}
	first, err := selector.Pick(context.Background(), "claude", "claude-haiku-4-5", opts, []*Auth{a, b})
	if err != nil || first.ID != "sess-a" {
		t.Fatalf("first = %v, %v", first, err)
	}
	// sess-a's 5-hour window fills up and sess-b now resets sooner; the session stays.
	storePolledQuota("sess-a", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(5 * 24 * time.Hour)}, fiveHour: polledWindow{reset: now.Add(time.Hour), exhausted: true}})
	storePolledQuota("sess-b", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(24 * time.Hour)}})
	for i := 0; i < 3; i++ {
		got, errPick := selector.Pick(context.Background(), "claude", "claude-haiku-4-5", opts, []*Auth{a, b})
		if errPick != nil || got.ID != "sess-a" {
			t.Fatalf("call %d moved the session to %v (%v)", i, got, errPick)
		}
	}
}

func TestParseClaudeUsage_ModelWindowFromLimits(t *testing.T) {
	reset := time.Now().Add(3 * 24 * time.Hour)
	q, ok := parseClaudeUsage([]byte(usageBody(0, reset)), time.Now())
	if !ok || q.weekly.reset.Unix() != reset.Unix() {
		t.Fatalf("weekly = %+v ok=%v", q, ok)
	}
	if w, found := q.models["fable"]; !found || w.reset.Unix() != reset.Unix() {
		t.Fatalf("model windows = %+v", q.models)
	}
}
