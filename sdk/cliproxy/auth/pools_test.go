package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const poolTestProvider = "pooltest"
const poolTestModel = "pool-model"

type poolEchoExecutor struct{}

func (poolEchoExecutor) Identifier() string { return poolTestProvider }
func (poolEchoExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}
func (poolEchoExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not used")
}
func (poolEchoExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (poolEchoExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}
func (poolEchoExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// fakeGin satisfies the subset of *gin.Context the pool resolver reads.
type fakeGin map[string]any

func (g fakeGin) Get(key string) (any, bool) { v, ok := g[key]; return v, ok }

func testPools(t *testing.T, fallback string) *CredentialPools {
	t.Helper()
	pools, err := NewCredentialPools([]internalconfig.CredentialPool{
		{Name: "munder-difflin", Credentials: []string{"md@example.com"}, Reserved: true, Fallback: fallback},
		{Name: "hydron-engine", Credentials: []string{"shared-b.json"}},
	}, []internalconfig.PoolClient{
		{Name: "munder-difflin", APIKey: "key-md", Pool: "munder-difflin"},
		{Name: "studio-claude-code", APIKey: "key-dev", Pool: "shared"},
		{Name: "hydron-engine", APIKey: "key-hydron", Pool: "hydron-engine"},
	}, "")
	if err != nil {
		t.Fatalf("NewCredentialPools() error = %v", err)
	}
	return pools
}

func poolCtx(pools *CredentialPools, apiKey string) context.Context {
	client := pools.ClientForKey(apiKey)
	return withPoolScope(context.Background(), &poolScope{pools: pools, pool: client.Pool, client: client.Name})
}

func newPoolManager(t *testing.T, selector Selector, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(poolEchoExecutor{})
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		auth.Provider = poolTestProvider
		auth.Status = StatusActive
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
		reg.RegisterClient(auth.ID, poolTestProvider, []*registry.ModelInfo{{ID: poolTestModel}})
		id := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(id) })
		manager.RefreshSchedulerEntry(auth.ID)
	}
	return manager
}

func mdAuth() *Auth {
	return &Auth{ID: "md.json", Metadata: map[string]any{"email": "md@example.com", "type": "oauth"}, Attributes: map[string]string{}}
}

func execID(t *testing.T, m *Manager, ctx context.Context, opts cliproxyexecutor.Options) (string, error) {
	t.Helper()
	resp, err := m.Execute(ctx, []string{poolTestProvider}, cliproxyexecutor.Request{Model: poolTestModel}, opts)
	return string(resp.Payload), err
}

func TestPools_ValidationRejectsDoubleReservation(t *testing.T) {
	_, err := NewCredentialPools([]internalconfig.CredentialPool{
		{Name: "a", Credentials: []string{"x@example.com"}, Reserved: true},
		{Name: "b", Credentials: []string{"X@example.com"}, Reserved: true},
	}, nil, "")
	if err == nil {
		t.Fatal("want error for a credential reserved by two pools")
	}
}

func TestPools_SharedExcludesReservedAccount(t *testing.T) {
	pools := testPools(t, "")
	md := mdAuth()
	if pools.Allows(SharedPoolName, md) {
		t.Fatal("shared pool must not include a reserved account")
	}
	if !pools.Allows("munder-difflin", md) {
		t.Fatal("reserved account must be in its own pool")
	}
	if got := pools.PoolsFor(md); len(got) != 1 || got[0] != "munder-difflin" {
		t.Fatalf("PoolsFor(md) = %v", got)
	}
}

func TestPools_ReservedAccountNeverServesAnotherKey(t *testing.T) {
	pools := testPools(t, "")
	m := newPoolManager(t, &FillFirstSelector{}, mdAuth(), &Auth{ID: "shared-a.json"}, &Auth{ID: "shared-b.json"})
	for _, key := range []string{"key-dev", "key-hydron", "key-unknown"} {
		for i := 0; i < 5; i++ {
			got, err := execID(t, m, poolCtx(pools, key), cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("%s: Execute() error = %v", key, err)
			}
			if got == "md.json" {
				t.Fatalf("%s was served by the reserved munder-difflin account", key)
			}
		}
	}
	got, err := execID(t, m, poolCtx(pools, "key-md"), cliproxyexecutor.Options{})
	if err != nil || got != "md.json" {
		t.Fatalf("munder-difflin key got %q, %v; want md.json", got, err)
	}
	got, _ = execID(t, m, poolCtx(pools, "key-hydron"), cliproxyexecutor.Options{})
	if got != "shared-b.json" {
		t.Fatalf("hydron key got %q, want its pool member shared-b.json", got)
	}
}

func coolingAuth(id string) *Auth {
	next := time.Now().Add(time.Hour)
	auth := mdAuth()
	auth.ID = id
	auth.ModelStates = map[string]*ModelState{poolTestModel: {
		Status: StatusError, Unavailable: true, NextRetryAfter: next,
		Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
	}}
	return auth
}

func TestPools_ExhaustionIsAnErrorNotSpillOver(t *testing.T) {
	pools := testPools(t, "")
	m := newPoolManager(t, &FillFirstSelector{}, coolingAuth("md.json"), &Auth{ID: "shared-a.json"})
	got, err := execID(t, m, poolCtx(pools, "key-md"), cliproxyexecutor.Options{})
	if err == nil {
		t.Fatalf("Execute() served %q, want pool exhausted error", got)
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "pool_exhausted" || !strings.Contains(err.Error(), `"munder-difflin"`) {
		t.Fatalf("error = %v, want pool_exhausted naming munder-difflin", err)
	}
}

func TestPools_ExplicitFallbackIsUsedOnlyWhenConfigured(t *testing.T) {
	pools := testPools(t, SharedPoolName)
	m := newPoolManager(t, &FillFirstSelector{}, coolingAuth("md.json"), &Auth{ID: "shared-a.json"})
	got, err := execID(t, m, poolCtx(pools, "key-md"), cliproxyexecutor.Options{})
	if err != nil || got != "shared-a.json" {
		t.Fatalf("Execute() = %q, %v; want fallback to shared-a.json", got, err)
	}
}

func TestPools_SessionAffinityStaysWithinPool(t *testing.T) {
	pools := testPools(t, "")
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	m := newPoolManager(t, selector, &Auth{ID: "a-shared.json"}, mdAuth())
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_ac980658-63bd-4fb3-97ba-8da64cb1e344"}}`)}

	for i := 0; i < 3; i++ {
		got, err := execID(t, m, poolCtx(pools, "key-dev"), opts)
		if err != nil || got != "a-shared.json" {
			t.Fatalf("dev key got %q, %v", got, err)
		}
		got, err = execID(t, m, poolCtx(pools, "key-md"), opts)
		if err != nil || got != "md.json" {
			t.Fatalf("munder-difflin key with the same session got %q, %v; want md.json", got, err)
		}
	}
}

func TestPools_ResolvesScopeFromClientKey(t *testing.T) {
	pools := testPools(t, "")
	activeCredentialPools.Store(pools)
	defer activeCredentialPools.Store(nil)
	ctx := context.WithValue(context.Background(), "gin", fakeGin{"userApiKey": "key-md"})
	scope := poolScopeForRequest(ctx)
	if scope == nil || scope.pool != "munder-difflin" || scope.client != "munder-difflin" {
		t.Fatalf("scope = %+v", scope)
	}
	if poolScopeForRequest(context.Background()) != nil {
		t.Fatal("a request without a client key must not be scoped")
	}
}
