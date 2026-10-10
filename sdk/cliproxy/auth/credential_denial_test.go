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

// denialTestExecutor answers 403 for the denied credential, or for the denied model
// with modelMsg, and succeeds otherwise.
type denialTestExecutor struct {
	mu          sync.Mutex
	denied      string
	deniedModel string
	modelMsg    string
	calls       map[string]int
}

func (e *denialTestExecutor) Identifier() string { return "denial-test-provider" }

func (e *denialTestExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[auth.ID]++
	if e.deniedModel != "" && req.Model == e.deniedModel {
		return cliproxyexecutor.Response{}, compactTestStatusError{code: http.StatusForbidden, msg: e.modelMsg}
	}
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

// A 403 that refuses one model must not take the account out for its other models.
func TestManager_ModelScopedForbiddenStaysPerModel(t *testing.T) {
	cases := map[string]string{
		"plan":   `{"error":{"message":"The model denial-scope-b is not available for your plan."}}`,
		"vertex": `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Permission denied on resource projects/p/locations/l/publishers/google/models/denial-scope-b"}}`,
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			id := "denial-scope-" + name
			executor := &denialTestExecutor{deniedModel: "denial-scope-b", modelMsg: msg, calls: map[string]int{}}
			m := NewManager(nil, nil, nil)
			m.RegisterExecutor(executor)
			if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: executor.Identifier(), Status: StatusActive}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			registry.GetGlobalRegistry().RegisterClient(id, executor.Identifier(), []*registry.ModelInfo{{ID: "denial-scope-a"}, {ID: "denial-scope-b"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })

			reqB := cliproxyexecutor.Request{Model: "denial-scope-b", Payload: []byte(`{"input":"hello"}`)}
			if _, err := m.Execute(context.Background(), []string{executor.Identifier()}, reqB, cliproxyexecutor.Options{}); err == nil {
				t.Fatal("Execute model B: want the 403")
			}
			reqA := cliproxyexecutor.Request{Model: "denial-scope-a", Payload: []byte(`{"input":"hello"}`)}
			if _, err := m.Execute(context.Background(), []string{executor.Identifier()}, reqA, cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("Execute model A: %v, want the same credential to serve it", err)
			}
			if got := executor.callsFor(id); got != 2 {
				t.Fatalf("credential reached upstream %d times, want 2 (B refused, A served)", got)
			}
			auth, _ := m.GetByID(id)
			if IsCredentialWideCooldown(auth, time.Now()) {
				t.Fatalf("quota = %+v, want no credential-wide cooldown", auth.Quota)
			}
			if state := auth.ModelStates["denial-scope-b"]; state == nil || !state.NextRetryAfter.After(time.Now()) {
				t.Fatalf("model B state = %+v, want it cooling", state)
			}
		})
	}
}

func TestIsModelSupportResultError_ForbiddenScope(t *testing.T) {
	cases := []struct {
		msg   string
		model bool
	}{
		{"This model is not available for your plan", true},
		{`{"error":{"status":"PERMISSION_DENIED","message":"Permission denied on resource .../models/gemini-x"}}`, true},
		{`{"error":{"type":"permission_error","message":"oauth_not_allowed_for_organization"}}`, false},
		{"OAuth authentication is currently not allowed for this organization.", false},
		{`{"error":{"status":"PERMISSION_DENIED","message":"The caller does not have permission"}}`, false},
	}
	for _, c := range cases {
		err := &Error{HTTPStatus: http.StatusForbidden, Message: c.msg}
		if got := isModelSupportResultError(err); got != c.model {
			t.Errorf("isModelSupportResultError(403 %q) = %v, want %v", c.msg, got, c.model)
		}
	}
}

// A whole-account 403 still cools the credential for every model.
func TestManager_OrganizationForbiddenIsCredentialWide(t *testing.T) {
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(context.Background(), &Auth{ID: "denial-org", Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	m.MarkResult(context.Background(), Result{
		AuthID: "denial-org", Provider: "claude", Model: "denial-org-a",
		Error: &Error{HTTPStatus: http.StatusForbidden, Message: `{"error":{"type":"permission_error","message":"oauth_not_allowed_for_organization"}}`},
	})
	auth, _ := m.GetByID("denial-org")
	if !IsCredentialWideCooldown(auth, time.Now()) || auth.Quota.Reason != "forbidden" {
		t.Fatalf("quota = %+v, want a forbidden credential-wide cooldown", auth.Quota)
	}
}

// A denial landing inside a longer credential quota window must not shorten it.
func TestCredentialDenialCooldown_KeepsLongerQuotaWindow(t *testing.T) {
	now := time.Now()
	quotaUntil := now.Add(3 * time.Hour)
	auth := &Auth{ID: "denial-quota", Provider: "claude", Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: quotaUntil}}
	if next := applyCredentialDenialCooldown(auth, http.StatusForbidden, now); !next.Equal(quotaUntil) {
		t.Fatalf("deadline = %v, want the quota window %v", next, quotaUntil)
	}
	if !auth.NextRetryAfter.Equal(quotaUntil) || auth.Quota.Reason != "credential_quota" {
		t.Fatalf("auth = retry %v reason %q, want the quota window kept", auth.NextRetryAfter, auth.Quota.Reason)
	}

	// A shorter quota window gives way to the 30m denial.
	auth = &Auth{ID: "denial-quota-short", Provider: "claude", Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(5 * time.Minute)}}
	if next := applyCredentialDenialCooldown(auth, http.StatusForbidden, now); next.Sub(now) != 30*time.Minute || auth.Quota.Reason != "forbidden" {
		t.Fatalf("deadline %v reason %q, want 30m forbidden", next.Sub(now), auth.Quota.Reason)
	}
}
