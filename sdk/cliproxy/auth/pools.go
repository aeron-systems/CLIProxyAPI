package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	log "github.com/sirupsen/logrus"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SharedPoolName is the implicit pool holding every credential that is not
// reserved by another pool.
const SharedPoolName = "shared"

// CredentialPools binds client API keys to named pools of upstream credentials.
// A request authenticated with a key may only be routed to credentials in that
// key's pool, on the first pick, on retries and on failover. Pools are enforced
// in the request eligibility filter that every selection path applies.
type CredentialPools struct {
	defaultPool string
	pools       map[string]internalconfig.CredentialPool
	clients     map[string]internalconfig.PoolClient // keyed by API key
	members     map[string]map[string]struct{}       // pool -> normalized identifiers
	reserved    map[string]string                    // normalized identifier -> owning pool
	invalid     error
}

var activeCredentialPools atomic.Pointer[CredentialPools]

// CurrentCredentialPools returns the active pool configuration, or nil when
// pools are not configured.
func CurrentCredentialPools() *CredentialPools { return activeCredentialPools.Load() }

func setCredentialPoolsFromConfig(cfg *internalconfig.Config) {
	if cfg == nil || (len(cfg.Routing.Pools) == 0 && len(cfg.Routing.PoolClients) == 0) {
		activeCredentialPools.Store(nil)
		return
	}
	pools, err := NewCredentialPools(cfg.Routing.Pools, cfg.Routing.PoolClients, cfg.Routing.DefaultPool)
	if err != nil {
		// Fail closed: keyed requests are refused until the config is fixed.
		log.Errorf("credential pools: invalid configuration, keyed requests will be refused: %v", err)
		pools = &CredentialPools{invalid: err}
	}
	activeCredentialPools.Store(pools)
}

// NewCredentialPools validates and indexes a pool configuration.
func NewCredentialPools(pools []internalconfig.CredentialPool, clients []internalconfig.PoolClient, defaultPool string) (*CredentialPools, error) {
	p := &CredentialPools{
		defaultPool: strings.TrimSpace(defaultPool),
		pools:       map[string]internalconfig.CredentialPool{SharedPoolName: {Name: SharedPoolName}},
		clients:     make(map[string]internalconfig.PoolClient),
		members:     make(map[string]map[string]struct{}),
		reserved:    make(map[string]string),
	}
	if p.defaultPool == "" {
		p.defaultPool = SharedPoolName
	}
	for _, pool := range pools {
		name := strings.TrimSpace(pool.Name)
		if name == "" {
			return nil, errors.New("pool without a name")
		}
		if name == SharedPoolName {
			if len(pool.Credentials) > 0 || pool.Reserved {
				return nil, errors.New(`pool "shared" is implicit: it may only set a fallback`)
			}
		} else if _, dup := p.members[name]; dup {
			return nil, fmt.Errorf("pool %q defined twice", name)
		}
		pool.Name = name
		p.pools[name] = pool
		set := make(map[string]struct{}, len(pool.Credentials))
		for _, raw := range pool.Credentials {
			id := normalizePoolIdentifier(raw)
			if id == "" {
				continue
			}
			set[id] = struct{}{}
			if pool.Reserved {
				if owner, taken := p.reserved[id]; taken && owner != name {
					return nil, fmt.Errorf("credential %q is reserved by both %q and %q", raw, owner, name)
				}
				p.reserved[id] = name
			}
		}
		p.members[name] = set
	}
	for name, pool := range p.pools {
		if fb := strings.TrimSpace(pool.Fallback); fb != "" {
			if _, ok := p.pools[fb]; !ok {
				return nil, fmt.Errorf("pool %q falls back to unknown pool %q", name, fb)
			}
		}
	}
	if _, ok := p.pools[p.defaultPool]; !ok {
		return nil, fmt.Errorf("default-pool %q is not defined", p.defaultPool)
	}
	for _, client := range clients {
		key := strings.TrimSpace(client.APIKey)
		if key == "" {
			return nil, fmt.Errorf("pool client %q has no api-key", client.Name)
		}
		if _, ok := p.pools[strings.TrimSpace(client.Pool)]; !ok {
			return nil, fmt.Errorf("pool client %q uses unknown pool %q", client.Name, client.Pool)
		}
		if _, dup := p.clients[key]; dup {
			return nil, fmt.Errorf("pool client %q reuses an api-key", client.Name)
		}
		client.Pool = strings.TrimSpace(client.Pool)
		p.clients[key] = client
	}
	return p, nil
}

func normalizePoolIdentifier(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// authPoolIdentifiers lists the names a pool may use for a credential: its ID,
// its auth file name, its label and its account email.
func authPoolIdentifiers(auth *Auth) []string {
	ids := []string{auth.ID, auth.Label}
	if auth.FileName != "" {
		ids = append(ids, auth.FileName, filepath.Base(auth.FileName))
	}
	if kind, account := auth.AccountInfo(); kind == "oauth" {
		ids = append(ids, account)
	}
	out := ids[:0]
	for _, id := range ids {
		if n := normalizePoolIdentifier(id); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ReservedBy returns the pool a credential is reserved for, if any.
func (p *CredentialPools) ReservedBy(auth *Auth) string {
	if p == nil || auth == nil {
		return ""
	}
	for _, id := range authPoolIdentifiers(auth) {
		if owner, ok := p.reserved[id]; ok {
			return owner
		}
	}
	return ""
}

// Allows reports whether pool may route to auth.
func (p *CredentialPools) Allows(pool string, auth *Auth) bool {
	if p == nil {
		return true
	}
	if p.invalid != nil || auth == nil {
		return false
	}
	owner := p.ReservedBy(auth)
	if pool == SharedPoolName {
		return owner == ""
	}
	if owner != "" && owner != pool {
		return false
	}
	members := p.members[pool]
	for _, id := range authPoolIdentifiers(auth) {
		if _, ok := members[id]; ok {
			return true
		}
	}
	return false
}

// PoolsFor lists every pool that may route to auth, sorted.
func (p *CredentialPools) PoolsFor(auth *Auth) []string {
	if p == nil || p.invalid != nil {
		return nil
	}
	var out []string
	for name := range p.pools {
		if p.Allows(name, auth) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ClientForKey resolves the pool client for a client API key. Keys without an
// explicit entry use the default pool and an empty client name.
func (p *CredentialPools) ClientForKey(apiKey string) internalconfig.PoolClient {
	if p == nil {
		return internalconfig.PoolClient{}
	}
	if client, ok := p.clients[strings.TrimSpace(apiKey)]; ok {
		return client
	}
	return internalconfig.PoolClient{Pool: p.defaultPool}
}

// Invalid returns the configuration error, if the pool config failed to load.
func (p *CredentialPools) Invalid() error {
	if p == nil {
		return nil
	}
	return p.invalid
}

// DefaultPool returns the pool used by keys without an explicit entry.
func (p *CredentialPools) DefaultPool() string {
	if p == nil {
		return ""
	}
	return p.defaultPool
}

// Definitions returns the configured pools, including the implicit "shared".
func (p *CredentialPools) Definitions() []internalconfig.CredentialPool {
	if p == nil {
		return nil
	}
	out := make([]internalconfig.CredentialPool, 0, len(p.pools))
	for _, pool := range p.pools {
		out = append(out, pool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Clients returns the configured key-to-pool bindings.
func (p *CredentialPools) Clients() []internalconfig.PoolClient {
	if p == nil {
		return nil
	}
	out := make([]internalconfig.PoolClient, 0, len(p.clients))
	for _, client := range p.clients {
		out = append(out, client)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// poolScope is the pool a request is confined to.
type poolScope struct {
	pools  *CredentialPools
	pool   string
	client string
}

type poolScopeContextKey struct{}

func withPoolScope(ctx context.Context, scope *poolScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, poolScopeContextKey{}, scope)
}

// poolScopeForRequest returns the pool the request is confined to, or nil when
// pools are off or the request carries no client API key (internal calls).
func poolScopeForRequest(ctx context.Context) *poolScope {
	if ctx == nil {
		return nil
	}
	if scope, ok := ctx.Value(poolScopeContextKey{}).(*poolScope); ok {
		return scope
	}
	pools := CurrentCredentialPools()
	if pools == nil {
		return nil
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return nil
	}
	raw, ok := ginCtx.Get("userApiKey")
	if !ok || raw == nil {
		return nil
	}
	apiKey := strings.TrimSpace(fmt.Sprint(raw))
	if apiKey == "" {
		return nil
	}
	client := pools.ClientForKey(apiKey)
	return &poolScope{pools: pools, pool: client.Pool, client: client.Name}
}

func (s *poolScope) allows(auth *Auth) bool {
	return s == nil || s.pools.Allows(s.pool, auth)
}

// isPoolExhaustedCandidate reports whether err means no eligible credential
// was available, as opposed to an upstream or request failure.
func isPoolExhaustedCandidate(err error) bool {
	if err == nil {
		return false
	}
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) {
		return true
	}
	var unavailable *authUnavailableError
	if errors.As(err, &unavailable) {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
	}
	return false
}

func poolExhaustedError(scope *poolScope, cause error) error {
	status := statusCodeFromError(cause)
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	msg := fmt.Sprintf("credential pool %q has no eligible credential (exhausted, cooling down, or none signed in); requests with this key never spill over to other pools", scope.pool)
	if errInvalid := scope.pools.Invalid(); errInvalid != nil {
		msg = fmt.Sprintf("credential pool configuration is invalid, keyed requests are refused: %v", errInvalid)
	}
	return WithCause(&Error{Code: "pool_exhausted", Message: msg, Retryable: true, HTTPStatus: status}, cause)
}

// runInPool confines run to the request's pool. When the pool has no eligible
// credential it moves to the pool's configured fallback, if any, and otherwise
// returns an error naming the pool. It also logs which key used which credential.
func runInPool[T any](ctx context.Context, opts cliproxyexecutor.Options, run func(context.Context, cliproxyexecutor.Options) (T, error)) (T, error) {
	scope := poolScopeForRequest(ctx)
	if scope == nil {
		return run(ctx, opts)
	}
	visited := map[string]struct{}{}
	for {
		visited[scope.pool] = struct{}{}
		current := scope
		poolOpts := opts
		poolOpts.Metadata = cloneRequestMetadata(opts.Metadata)
		prev, _ := poolOpts.Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string))
		poolOpts.Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(authID string) {
			log.WithFields(log.Fields{"pool": current.pool, "client": current.client, "credential": authID}).Info("credential pool: routed request")
			if prev != nil {
				prev(authID)
			}
		}
		result, err := run(withPoolScope(ctx, current), poolOpts)
		if err == nil || !isPoolExhaustedCandidate(err) {
			return result, err
		}
		fallback := strings.TrimSpace(current.pools.pools[current.pool].Fallback)
		if _, seen := visited[fallback]; fallback == "" || seen || current.pools.Invalid() != nil {
			return result, poolExhaustedError(current, err)
		}
		log.WithFields(log.Fields{"pool": current.pool, "fallback": fallback, "client": current.client}).Warn("credential pool exhausted, using configured fallback pool")
		scope = &poolScope{pools: current.pools, pool: fallback, client: current.client}
	}
}
