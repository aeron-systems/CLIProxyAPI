package clientusage

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// defaultPrices is written out as the price file the first time, and used if
// that file later becomes unreadable.
//
//go:embed default_prices.json
var defaultPrices []byte

// ModelPrice is what one model costs, in dollars per million tokens.
type ModelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Dollars prices mutually exclusive token counts. Unclassified tokens have no
// rate and are reported by the caller as unpriced.
func (p ModelPrice) Dollars(c Counts) float64 {
	return (float64(c.InputTokens)*p.Input +
		float64(c.OutputTokens)*p.Output +
		float64(c.CacheReadTokens)*p.CacheRead +
		float64(c.CacheCreationTokens)*p.CacheWrite) / 1_000_000
}

// PriceTable is the loaded price file.
type PriceTable struct {
	AsOf   string
	Source string
	models map[string]ModelPrice
	// Invalid names rows left out because a rate was missing or negative. A
	// missing rate must not turn into a free one, so such a model has no price.
	Invalid []string
}

type priceFile struct {
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
	Models map[string]struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"models"`
}

// ParsePrices reads the price file format.
func ParsePrices(raw []byte) (*PriceTable, error) {
	var f priceFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Models == nil {
		return nil, fmt.Errorf("no \"models\" object")
	}
	t := &PriceTable{AsOf: f.AsOf, Source: f.Source, models: make(map[string]ModelPrice, len(f.Models))}
	for name, row := range f.Models {
		rates := []*float64{row.Input, row.Output, row.CacheRead, row.CacheWrite}
		valid := true
		for _, rate := range rates {
			if rate == nil || *rate < 0 {
				valid = false
			}
		}
		if !valid {
			t.Invalid = append(t.Invalid, name)
			continue
		}
		t.models[strings.ToLower(strings.TrimSpace(name))] = ModelPrice{Input: *row.Input, Output: *row.Output, CacheRead: *row.CacheRead, CacheWrite: *row.CacheWrite}
	}
	sort.Strings(t.Invalid)
	return t, nil
}

// A key matches a longer name only across a version suffix, so "claude-opus-5"
// prices "claude-opus-5-20260101" but never "claude-opus-5-6", a different model
// that must show as no price until it has a row of its own.
var versionSuffix = regexp.MustCompile(`^(-\d{8}|-latest|@\d{8})?(-v\d+(:\d+)?)?$`)

// Lookup finds a model's price. The longest matching row wins, and a row ending
// in "*" matches anything starting with the text before it.
func (t *PriceTable) Lookup(model string) (ModelPrice, bool) {
	if t == nil {
		return ModelPrice{}, false
	}
	name := bareModelName(model)
	if price, ok := t.models[name]; ok {
		return price, true
	}
	var best ModelPrice
	bestLen, found := -1, false
	for key, price := range t.models {
		var matches bool
		prefix := key
		if strings.HasSuffix(key, "*") {
			prefix = strings.TrimSuffix(key, "*")
			matches = strings.HasPrefix(name, prefix)
		} else {
			matches = strings.HasPrefix(name, key) && versionSuffix.MatchString(name[len(key):])
		}
		if matches && len(prefix) > bestLen {
			best, bestLen, found = price, len(prefix), true
		}
	}
	return best, found
}

// bareModelName drops routing prefixes such as "anthropic/" and "us.anthropic.".
func bareModelName(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "anthropic."); i >= 0 {
		name = name[i+len("anthropic."):]
	}
	return name
}

// PriceBook keeps the price table in step with its file, so an edit takes effect
// without a restart.
type PriceBook struct {
	mu      sync.Mutex
	path    string
	modTime time.Time
	size    int64
	table   *PriceTable
	err     string
}

var defaultPriceBook = &PriceBook{}

// DefaultPrices returns the process-wide price book.
func DefaultPrices() *PriceBook { return defaultPriceBook }

// PricesPath is where the price file lives, given the usage file's path.
func PricesPath(usagePath string) string {
	return filepath.Join(filepath.Dir(usagePath), "model-prices.json")
}

// SetPath points the book at its file, writing the committed defaults there if
// there is no file yet, so there is always one table to read and edit.
func (b *PriceBook) SetPath(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if path == b.path {
		return
	}
	b.path, b.modTime, b.size, b.table, b.err = path, time.Time{}, 0, nil, ""
	if path == "" {
		return
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			log.Warnf("client usage: cannot create price file: %v", err)
			return
		}
		if err := os.WriteFile(path, defaultPrices, 0o600); err != nil {
			log.Warnf("client usage: cannot create price file: %v", err)
		}
	}
}

// Current returns the table to price with, and why the file could not be used
// if the table had to fall back to the committed defaults.
func (b *PriceBook) Current() (*PriceTable, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.path != "" {
		info, err := os.Stat(b.path)
		switch {
		case err != nil:
			b.table, b.err = nil, err.Error()
			b.modTime, b.size = time.Time{}, 0
		case b.table == nil || !info.ModTime().Equal(b.modTime) || info.Size() != b.size:
			b.modTime, b.size = info.ModTime(), info.Size()
			b.table, b.err = nil, ""
			raw, errRead := os.ReadFile(b.path)
			if errRead == nil {
				b.table, errRead = ParsePrices(raw)
			}
			if errRead != nil {
				b.table, b.err = nil, errRead.Error()
			}
		}
	}
	if b.table != nil {
		return b.table, b.err
	}
	table, err := ParsePrices(defaultPrices)
	if err != nil {
		// Covered by a test; the committed file always parses.
		return &PriceTable{models: map[string]ModelPrice{}}, err.Error()
	}
	return table, b.err
}
