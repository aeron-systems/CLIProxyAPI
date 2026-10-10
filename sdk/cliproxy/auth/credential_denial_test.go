package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// denialTestExecutor answers 403 for the denied credential and succeeds for any other.
type denialTestExecutor struct {
	mu     sync.Mutex
	denied string
	calls  map[string]int
}

func (e *denialTestExecutor) Identifier() string { return "denial-test-provider" }

func (e *denialTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[auth.ID]++
	if auth.ID == e.denied {
		return cliproxyexecutor.Response{}, compactTestStatusError{code: http.StatusForbidden, msg: `{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization."}}`}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"status":"ok"}`)}, nil
}

func (e *denialTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("stream not supported")
}

func (e *denialTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *denialTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not supported")
}

func (e *denialTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not supported")
}

func (e *denialTestExecutor) callsFor(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[id]
}

// An account refused by its organization was retried again and again, once per
// model, because a 403 cooled only the model that failed.
func TestManager_ForbiddenCoolsWholeCredentialAcrossModels(t *testing.T) {
	executor := &denialTestExecutor{denied: "denial-refused", calls: map[string]int{}}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)

	models := []string{"denial-model-a", "denial-model-b", "denial-model-c"}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	for _, id := range []string{"denial-refused", "denial-ok"} {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: executor.Identifier(), Status: StatusActive}); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, executor.Identifier(), infos)
		id := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}

	for round := 0; round < 4; round++ {
		for _, model := range models {
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}
			if _, err := m.Execute(context.Background(), []string{executor.Identifier()}, req, cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("Execute %s: %v (the other credential should serve it)", model, err)
			}
		}
	}
	if got := executor.callsFor("denial-refused"); got != 1 {
		t.Fatalf("refused credential reached upstream %d times, want 1 inside its cooldown window", got)
	}

	refused, _ := m.GetByID("denial-refused")
	if refused.Quota.Reason != "forbidden" || !refused.Quota.Exceeded {
		t.Fatalf("quota = %+v, want a forbidden credential cooldown", refused.Quota)
	}
	if left := time.Until(refused.Quota.NextRecoverAt); left < 29*time.Minute || left > 31*time.Minute {
		t.Fatalf("cooldown = %v, want about 30m", left)
	}

	// A success (an in-flight request that completed) clears the cooldown and its backoff.
	m.MarkResult(context.Background(), Result{AuthID: "denial-refused", Provider: executor.Identifier(), Model: models[0], Success: true})
	refused, _ = m.GetByID("denial-refused")
	if IsCredentialWideCooldown(refused, time.Now()) || refused.Quota.BackoffLevel != 0 || refused.Quota.Reason != "" {
		t.Fatalf("after success quota = %+v, want cleared", refused.Quota)
	}
}

func TestCredentialDenialCooldown_BacksOffAndSuccessResets(t *testing.T) {
	auth := &Auth{ID: "denial-ladder", Provider: "claude"}
	now := time.Now()
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		next := applyCredentialDenialCooldown(auth, http.StatusForbidden, now)
		if got := next.Sub(now); got != w {
			t.Fatalf("denial %d: cooldown %v, want %v", i+1, got, w)
		}
		// A burst inside the window reuses it instead of climbing the ladder.
		if again := applyCredentialDenialCooldown(auth, http.StatusForbidden, now.Add(time.Minute)); !again.Equal(next) {
			t.Fatalf("denial %d: in-window denial moved the deadline to %v", i+1, again)
		}
		// The window expires; aggregation must keep the level for the next denial.
		now = next.Add(time.Second)
		updateAggregatedAvailability(auth, now)
	}

	if !clearCredentialDenial(auth) {
		t.Fatal("clearCredentialDenial reported nothing to clear")
	}
	if next := applyCredentialDenialCooldown(auth, http.StatusPaymentRequired, now); next.Sub(now) != 30*time.Minute || auth.Quota.Reason != "payment_required" {
		t.Fatalf("after success: cooldown %v reason %q, want 30m payment_required", next.Sub(now), auth.Quota.Reason)
	}
}

func TestSessionAffinitySelector_DoesNotBindToCoolingCredential(t *testing.T) {
	now := time.Now()
	pollQuotaForTest(t, "denial-aff-sooner", now, 24*time.Hour, 10, false)
	pollQuotaForTest(t, "denial-aff-later", now, 6*24*time.Hour, 10, false)
	refused := &Auth{ID: "denial-aff-sooner", Provider: "claude", Status: StatusActive}
	applyCredentialDenialCooldown(refused, http.StatusForbidden, now)
	other := &Auth{ID: "denial-aff-later", Provider: "claude", Status: StatusActive}

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &ResetFirstSelector{}, TTL: time.Hour, MovePolicy: defaultMovePolicy,
	})
	defer selector.Stop()
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", "99999999-2222-3333-4444-555555555555")
	for i := 0; i < 3; i++ {
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{Headers: headers}, []*Auth{refused, other})
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if got.ID != "denial-aff-later" {
			t.Fatalf("pick %d = %q, want the credential that is not cooling", i+1, got.ID)
		}
	}
}

func TestBuildQuotaView_CoolingCredentialIsNotUsable(t *testing.T) {
	now := time.Now()
	pollQuotaForTest(t, "denial-view-refused", now, 24*time.Hour, 10, false)
	pollQuotaForTest(t, "denial-view-ok", now, 48*time.Hour, 10, false)
	refused := &Auth{ID: "denial-view-refused", Provider: "claude", Status: StatusActive}
	applyCredentialDenialCooldown(refused, http.StatusForbidden, now)
	ok := &Auth{ID: "denial-view-ok", Provider: "claude", Status: StatusActive}

	view := BuildQuotaView([]*Auth{refused, ok}, "", "claude-opus-5-5", "reset-first", now)
	if view.Next != "denial-view-ok" {
		t.Fatalf("next = %q, want denial-view-ok", view.Next)
	}
	for _, cr := range view.Credentials {
		if cr.ID != "denial-view-refused" {
			continue
		}
		if cr.Usable || cr.CoolingReason != "forbidden" || cr.CoolingUntil == nil {
			t.Fatalf("refused credential: %+v, want usable=false, cooling_reason=forbidden, cooling_until set", cr)
		}
	}
}
