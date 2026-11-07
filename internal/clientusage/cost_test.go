package clientusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func defaultTable(t *testing.T) *PriceTable {
	t.Helper()
	table, err := ParsePrices(defaultPrices)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Invalid) != 0 {
		t.Fatalf("committed price file has invalid rows: %v", table.Invalid)
	}
	return table
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func TestLookupMatchesVersionsNotOtherModels(t *testing.T) {
	table := defaultTable(t)
	priced := map[string]float64{
		"claude-opus-5-5":                            4,
		"CLAUDE-OPUS-5-5":                            4,
		"claude-opus-5":                              5,
		"claude-opus-5-20260101":                     5,
		"claude-haiku-4-5-20251001":                  1,
		"claude-opus-4-20250514":                     15,
		"anthropic/claude-sonnet-5-5":                2,
		"us.anthropic.claude-opus-4-5-20251101-v1:0": 5,
		"claude-sonnet-4-5@20250929":                 3,
	}
	for model, input := range priced {
		price, ok := table.Lookup(model)
		if !ok || price.Input != input {
			t.Errorf("Lookup(%q) = %+v, %v; want input %v", model, price, ok, input)
		}
	}
	for _, model := range []string{"claude-opus-5-6", "claude-opus-55", "gpt-5", "glm-4.7", UnknownModel, ""} {
		if price, ok := table.Lookup(model); ok {
			t.Errorf("Lookup(%q) = %+v, want no price", model, price)
		}
	}
}

func TestLookupWildcardAndLongestRowWin(t *testing.T) {
	table, err := ParsePrices([]byte(`{"models":{
		"gpt-*":{"input":1,"output":1,"cache_read":1,"cache_write":1},
		"gpt-5*":{"input":2,"output":2,"cache_read":2,"cache_write":2},
		"local":{"input":0,"output":0,"cache_read":0,"cache_write":0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := table.Lookup("gpt-5.2-codex"); !ok || p.Input != 2 {
		t.Fatalf("gpt-5.2-codex = %+v, %v", p, ok)
	}
	if p, ok := table.Lookup("gpt-4"); !ok || p.Input != 1 {
		t.Fatalf("gpt-4 = %+v, %v", p, ok)
	}
	// A rate of zero that someone wrote down is a price; it means free.
	if _, ok := table.Lookup("local"); !ok {
		t.Fatal("explicit zero price was not found")
	}
}

func TestParsePricesMissingRateIsNoPriceNotFree(t *testing.T) {
	table, err := ParsePrices([]byte(`{"models":{
		"a":{"input":1,"output":2,"cache_read":3},
		"b":{"input":-1,"output":2,"cache_read":3,"cache_write":4}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Lookup("a"); ok {
		t.Fatal("row without cache_write must have no price")
	}
	if _, ok := table.Lookup("b"); ok {
		t.Fatal("row with a negative rate must have no price")
	}
	if len(table.Invalid) != 2 {
		t.Fatalf("invalid = %v", table.Invalid)
	}
	if _, err := ParsePrices([]byte(`{}`)); err == nil {
		t.Fatal("file without models must be an error")
	}
}

func TestCostDollarsBillingAndNoPrice(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
	s := NewStore()
	s.now = func() time.Time { return now }
	opus := Counts{Requests: 1, InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheCreationTokens: 1_000_000, TotalTokens: 4_000_000}
	s.AddModel(now, "t3", "shared", "claude-opus-5-5", opus, opus)
	s.AddModel(now, "billing-app", "paid", "claude-opus-5-5", opus, opus)
	strange := Counts{Requests: 1, InputTokens: 10, TotalTokens: 10}
	s.AddModel(now, "t3", "shared", "mystery-1", strange, strange)
	s.AddModel(now, "only-strange", "shared", "mystery-1", strange, strange)
	s.AddModel(now.Add(-48*time.Hour), "t3", "shared", "claude-opus-5-5", opus, opus)

	sum := s.Cost("24h", 24*time.Hour, defaultTable(t))

	// 4 + 20 + 0.20 + 5 for each of the two opus requests in the window.
	const each = 29.2
	if sum.Total.Dollars == nil || !near(*sum.Total.Dollars, 2*each) {
		t.Fatalf("total dollars = %v", sum.Total.Dollars)
	}
	if !near(sum.Total.RealDollars, each) || !near(sum.Total.EstimatedDollars, each) {
		t.Fatalf("total = %+v", sum.Total)
	}
	if sum.Total.UnpricedTokens != 20 {
		t.Fatalf("total unpriced tokens = %d", sum.Total.UnpricedTokens)
	}
	rows := map[string]CostRow{}
	for _, row := range sum.Clients {
		rows[row.Name] = row
	}
	if r := rows["t3"]; r.Dollars == nil || !near(r.EstimatedDollars, each) || r.RealDollars != 0 || r.UnpricedTokens != 10 {
		t.Fatalf("t3 = %+v", r)
	}
	if r := rows["billing-app"]; !near(r.RealDollars, each) || r.EstimatedDollars != 0 {
		t.Fatalf("billing-app = %+v", r)
	}
	if r := rows["only-strange"]; r.Dollars != nil {
		t.Fatalf("a client with only unpriced tokens must have no dollars, got %v", *r.Dollars)
	}
	if len(sum.UnpricedModels) != 1 || sum.UnpricedModels[0] != "mystery-1" {
		t.Fatalf("unpriced models = %v", sum.UnpricedModels)
	}
	if sum.Models[0].Name != "claude-opus-5-5" || sum.Models[1].Dollars != nil {
		t.Fatalf("models = %+v", sum.Models)
	}
}

func TestModelWithNoPriceIsNeverZeroDollars(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.now = func() time.Time { return now }
	failed := Counts{Requests: 1, Failed: 1}
	s.AddModel(now, "t3", "shared", "mystery-1", failed, failed)
	sum := s.Cost("all", 0, defaultTable(t))
	if sum.Models[0].Dollars != nil {
		t.Fatalf("failed requests on a no-price model = %v, want null", *sum.Models[0].Dollars)
	}
}

func TestIsPaidPool(t *testing.T) {
	for pool, want := range map[string]bool{"paid": true, " PAID ": true, "paid-eu": true, "api": true, "api-live3": true, "shared": false, "norea": false, "": false, "paidless": false} {
		if got := IsPaidPool(pool); got != want {
			t.Errorf("IsPaidPool(%q) = %v", pool, got)
		}
	}
}

func TestOldFileLoadsAsUnknownModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-by-client.json")
	raw := []byte(`{"version":1,"hours":{"1793448000":{"t3":{"requests":3,"failed":1,"input_tokens":100,"output_tokens":50,"cache_read_tokens":7,"cache_creation_tokens":8,"total_tokens":165,"pool":"shared"}}}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	s.now = func() time.Time { return time.Unix(1793448000, 0) }
	s.SetPath(path)

	// The old route's figures are unchanged.
	sum := s.Summarize("all", 0)
	if len(sum.Clients) != 1 || sum.Total.TotalTokens != 165 || sum.Total.Failed != 1 || sum.Clients[0].Pool != "shared" {
		t.Fatalf("summary = %+v", sum)
	}
	cost := s.Cost("all", 0, defaultTable(t))
	if len(cost.Models) != 1 || cost.Models[0].Name != UnknownModel {
		t.Fatalf("models = %+v", cost.Models)
	}
	if cost.Models[0].Dollars != nil || cost.Clients[0].Dollars != nil || cost.Total.Dollars != nil {
		t.Fatalf("old rows must be no price, not $0: %+v", cost)
	}
	if cost.Total.UnpricedTokens != 165 || cost.UnpricedModels[0] != UnknownModel {
		t.Fatalf("cost = %+v", cost)
	}
}

func TestModelsSurviveSaveAndReloadWithoutBecomingUnknown(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-by-client.json")
	a := NewStore()
	a.now = func() time.Time { return now }
	a.SetPath(path)
	c := Counts{Requests: 2, InputTokens: 1_000_000, TotalTokens: 1_000_000}
	a.AddModel(now, "t3", "shared", "claude-opus-5-5", c, c)
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	b := NewStore()
	b.now = func() time.Time { return now }
	b.SetPath(path)
	cost := b.Cost("all", 0, defaultTable(t))
	if len(cost.Models) != 1 || cost.Models[0].Name != "claude-opus-5-5" || !near(*cost.Total.Dollars, 4) {
		t.Fatalf("cost = %+v", cost)
	}
}

func TestPricedCountsDoNotChargeCachedTokensTwice(t *testing.T) {
	// OpenAI-style providers include cached tokens in input; Claude does not.
	subset := coreusage.EnsureTokenBreakdownForProvider(coreusage.Detail{InputTokens: 100, CacheReadTokens: 80, OutputTokens: 10, TotalTokens: 110}, "codex", "")
	got := pricedCounts(Counts{InputTokens: 100}, subset)
	if got.InputTokens != 20 || got.CacheReadTokens != 80 || got.OutputTokens != 10 {
		t.Fatalf("subset = %+v", got)
	}
	independent := coreusage.EnsureTokenBreakdownForProvider(coreusage.Detail{InputTokens: 20, CacheReadTokens: 80, CacheCreationTokens: 5, OutputTokens: 10, TotalTokens: 115}, "claude", "")
	got = pricedCounts(Counts{InputTokens: 20}, independent)
	if got.InputTokens != 20 || got.CacheReadTokens != 80 || got.CacheCreationTokens != 5 || got.OutputTokens != 10 {
		t.Fatalf("independent = %+v", got)
	}
}

func TestCleanModelKeepsOnlySafeNames(t *testing.T) {
	long := make([]byte, maxModelNameLength+1)
	for i := range long {
		long[i] = 'a'
	}
	for in, want := range map[string]string{
		"Claude-Opus-5-5":         "claude-opus-5-5",
		"claude-opus-5-5[1m]":     "claude-opus-5-5",
		"claude-opus-5-5(high)":   "claude-opus-5-5",
		"anthropic/claude-5:beta": "anthropic/claude-5:beta",
		"":                        UnknownModel,
		"has space":               UnknownModel,
		"{\"prompt\":\"secret\"}": UnknownModel,
		string(long):              UnknownModel,
	} {
		if got := cleanModel(in); got != want {
			t.Errorf("cleanModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelNamesPerBucketAreCapped(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.now = func() time.Time { return now }
	for i := 0; i < maxModelsPerCell*2; i++ {
		s.AddModel(now, "t3", "shared", "m"+time.Duration(i).String(), Counts{Requests: 1}, Counts{Requests: 1})
	}
	sum := s.Cost("all", 0, defaultTable(t))
	if len(sum.Models) > maxModelsPerCell+1 || sum.Total.Requests != int64(maxModelsPerCell*2) {
		t.Fatalf("models = %d, requests = %d", len(sum.Models), sum.Total.Requests)
	}
}

func TestPriceBookWritesDefaultsReloadsEditsAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-prices.json")
	b := &PriceBook{}
	b.SetPath(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default price file not written: %v", err)
	}
	table, problem := b.Current()
	if problem != "" {
		t.Fatal(problem)
	}
	if p, ok := table.Lookup("claude-opus-5-5"); !ok || p.Input != 4 {
		t.Fatalf("opus = %+v, %v", p, ok)
	}

	edited := []byte(`{"models":{"claude-opus-5-5":{"input":40,"output":0,"cache_read":0,"cache_write":0}}}`)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	table, _ = b.Current()
	if p, _ := table.Lookup("claude-opus-5-5"); p.Input != 40 {
		t.Fatalf("edit not picked up: %+v", p)
	}
	if _, ok := table.Lookup("claude-sonnet-5-5"); ok {
		t.Fatal("the file is the whole table; a removed row must have no price")
	}

	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	table, problem = b.Current()
	if problem == "" {
		t.Fatal("a broken file must be reported")
	}
	if p, ok := table.Lookup("claude-opus-5-5"); !ok || p.Input != 4 {
		t.Fatalf("broken file should fall back to defaults: %+v, %v", p, ok)
	}
}
