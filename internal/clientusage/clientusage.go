// Package clientusage counts model traffic per client app (per client API key),
// so the dashboard can show which app uses how much of the proxy.
//
// Every usage record is added to an hourly bucket under the name of the
// pool-client its API key belongs to. Buckets are kept for 35 days and saved to
// one small JSON file so a restart does not reset the picture. The API key
// itself is never stored, only the client's name.
package clientusage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// UnnamedClient labels requests made with a key that has no pool-client entry.
	UnnamedClient = "unnamed key"
	// InternalClient labels requests that carry no client key (the proxy's own calls).
	InternalClient = "internal"

	retention     = 35 * 24 * time.Hour
	flushInterval = 30 * time.Second
)

// Counts is the traffic of one client over some period.
type Counts struct {
	Requests            int64 `json:"requests"`
	Failed              int64 `json:"failed"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

func (c *Counts) add(o Counts) {
	c.Requests += o.Requests
	c.Failed += o.Failed
	c.InputTokens += o.InputTokens
	c.OutputTokens += o.OutputTokens
	c.CacheReadTokens += o.CacheReadTokens
	c.CacheCreationTokens += o.CacheCreationTokens
	c.TotalTokens += o.TotalTokens
}

// Client is one row of a summary.
type Client struct {
	Name string `json:"name"`
	Pool string `json:"pool,omitempty"`
	// Group is filled in by the caller from the current config, so a client
	// moved to another group takes its whole history with it.
	Group         string  `json:"group,omitempty"`
	ShareRequests float64 `json:"share_requests"`
	ShareTokens   float64 `json:"share_tokens"`
	Counts
}

// Summary is the answer to "who used how much over the last <window>".
type Summary struct {
	Window  string   `json:"window"`
	Since   string   `json:"since"`
	Total   Counts   `json:"total"`
	Clients []Client `json:"clients"`
}

type cell struct {
	Counts
	Pool string `json:"pool,omitempty"`
}

// Store holds the hourly buckets. hour (unix seconds, start of hour) -> client -> cell.
type Store struct {
	mu    sync.Mutex
	data  map[int64]map[string]*cell
	path  string
	dirty bool
	now   func() time.Time
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{data: make(map[int64]map[string]*cell), now: time.Now}
}

var defaultStore = NewStore()

// Default returns the process-wide store the usage plugin feeds.
func Default() *Store { return defaultStore }

// Add counts one request for client at time t.
func (s *Store) Add(t time.Time, client, pool string, c Counts) {
	if client == "" {
		client = UnnamedClient
	}
	hour := t.Truncate(time.Hour).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	byClient := s.data[hour]
	if byClient == nil {
		byClient = make(map[string]*cell)
		s.data[hour] = byClient
	}
	cl := byClient[client]
	if cl == nil {
		cl = &cell{}
		byClient[client] = cl
	}
	if pool != "" {
		cl.Pool = pool
	}
	cl.add(c)
	s.dirty = true
}

// Summarize totals every client over the last window. A zero window means all
// retained history.
func (s *Store) Summarize(label string, window time.Duration) Summary {
	now := s.now()
	var since time.Time
	if window > 0 {
		since = now.Add(-window).Truncate(time.Hour)
	}
	sum := Summary{Window: label, Clients: []Client{}}
	if !since.IsZero() {
		sum.Since = since.UTC().Format(time.RFC3339)
	}
	rows := make(map[string]*Client)
	s.mu.Lock()
	for hour, byClient := range s.data {
		if !since.IsZero() && hour < since.Unix() {
			continue
		}
		for name, cl := range byClient {
			row := rows[name]
			if row == nil {
				row = &Client{Name: name}
				rows[name] = row
			}
			if cl.Pool != "" {
				row.Pool = cl.Pool
			}
			row.add(cl.Counts)
			sum.Total.add(cl.Counts)
		}
	}
	s.mu.Unlock()
	for _, row := range rows {
		if sum.Total.TotalTokens > 0 {
			row.ShareTokens = float64(row.TotalTokens) / float64(sum.Total.TotalTokens)
		}
		if sum.Total.Requests > 0 {
			row.ShareRequests = float64(row.Requests) / float64(sum.Total.Requests)
		}
		sum.Clients = append(sum.Clients, *row)
	}
	sort.Slice(sum.Clients, func(i, j int) bool {
		a, b := sum.Clients[i], sum.Clients[j]
		if a.TotalTokens != b.TotalTokens {
			return a.TotalTokens > b.TotalTokens
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Name < b.Name
	})
	return sum
}

type file struct {
	Version int                        `json:"version"`
	Hours   map[int64]map[string]*cell `json:"hours"`
}

// SetPath sets the file the store saves to and loads any earlier history from it.
// Calling it again with the same path is a no-op.
func (s *Store) SetPath(path string) {
	s.mu.Lock()
	if path == s.path {
		s.mu.Unlock()
		return
	}
	s.path = path
	s.mu.Unlock()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("client usage: cannot read %s: %v", path, err)
		}
		return
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Warnf("client usage: ignoring unreadable %s: %v", path, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hour, byClient := range f.Hours {
		dst := s.data[hour]
		if dst == nil {
			dst = make(map[string]*cell)
			s.data[hour] = dst
		}
		for name, cl := range byClient {
			if cl == nil {
				continue
			}
			cur := dst[name]
			if cur == nil {
				cur = &cell{}
				dst[name] = cur
			}
			if cl.Pool != "" {
				cur.Pool = cl.Pool
			}
			cur.add(cl.Counts)
		}
	}
}

// Flush drops buckets past retention and writes the file if anything changed.
func (s *Store) Flush() error {
	cutoff := s.now().Add(-retention).Unix()
	s.mu.Lock()
	for hour := range s.data {
		if hour < cutoff {
			delete(s.data, hour)
			s.dirty = true
		}
	}
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(file{Version: 1, Hours: s.data})
	path := s.path
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunFlusher saves the store every few seconds until ctx ends, then once more.
func (s *Store) RunFlusher(ctx context.Context) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Flush(); err != nil {
				log.Warnf("client usage: final save failed: %v", err)
			}
			return
		case <-t.C:
			if err := s.Flush(); err != nil {
				log.Warnf("client usage: save failed: %v", err)
			}
		}
	}
}

var flusherOnce sync.Once

// StartFlusher starts saving the default store in the background, once.
func StartFlusher() {
	flusherOnce.Do(func() { go defaultStore.RunFlusher(context.Background()) })
}

type plugin struct{}

func init() {
	coreusage.RegisterPlugin(&plugin{})
}

// HandleUsage counts the record under the client its API key belongs to.
func (p *plugin) HandleUsage(_ context.Context, record coreusage.Record) {
	name, pool := clientFor(record.APIKey)
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	c := Counts{
		Requests:            1,
		InputTokens:         detail.InputTokens,
		OutputTokens:        detail.OutputTokens,
		CacheReadTokens:     detail.CacheReadTokens,
		CacheCreationTokens: detail.CacheCreationTokens,
		TotalTokens:         detail.TotalTokens,
	}
	if record.Failed {
		c.Failed = 1
	}
	defaultStore.Add(at, name, pool, c)
}

func clientFor(apiKey string) (name, pool string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return InternalClient, ""
	}
	pools := coreauth.CurrentCredentialPools()
	if pools == nil {
		return UnnamedClient, ""
	}
	client := pools.ClientForKey(apiKey)
	if client.Name == "" {
		return UnnamedClient, client.Pool
	}
	return client.Name, client.Pool
}
