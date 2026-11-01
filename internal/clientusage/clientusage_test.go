package clientusage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSummarizeSharesAndWindow(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
	s := NewStore()
	s.now = func() time.Time { return now }

	s.Add(now, "hydron-engine", "shared", Counts{Requests: 1, TotalTokens: 300})
	s.Add(now, "t3", "shared", Counts{Requests: 1, TotalTokens: 100})
	s.Add(now, "t3", "shared", Counts{Requests: 1, Failed: 1})
	s.Add(now.Add(-48*time.Hour), "t3", "shared", Counts{Requests: 5, TotalTokens: 600})

	day := s.Summarize("24h", 24*time.Hour)
	if day.Total.TotalTokens != 400 || day.Total.Requests != 3 {
		t.Fatalf("24h total = %+v", day.Total)
	}
	if len(day.Clients) != 2 || day.Clients[0].Name != "hydron-engine" {
		t.Fatalf("clients = %+v", day.Clients)
	}
	if got := day.Clients[0].ShareTokens; got != 0.75 {
		t.Fatalf("hydron-engine token share = %v, want 0.75", got)
	}
	if got := day.Clients[1].Failed; got != 1 {
		t.Fatalf("t3 failed = %d, want 1", got)
	}

	all := s.Summarize("all", 0)
	if all.Total.TotalTokens != 1000 || all.Clients[0].Name != "t3" {
		t.Fatalf("all = %+v", all)
	}
}

func TestSummarizeEmptyIsNotNull(t *testing.T) {
	sum := NewStore().Summarize("24h", 24*time.Hour)
	if sum.Clients == nil || len(sum.Clients) != 0 {
		t.Fatalf("clients = %#v, want empty non-nil slice", sum.Clients)
	}
}

func TestFlushAndReload(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-by-client.json")

	a := NewStore()
	a.now = func() time.Time { return now }
	a.SetPath(path)
	a.Add(now, "norea", "shared", Counts{Requests: 2, TotalTokens: 50})
	a.Add(now.Add(-40*24*time.Hour), "old", "", Counts{Requests: 9})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	b := NewStore()
	b.now = func() time.Time { return now }
	b.SetPath(path)
	got := b.Summarize("all", 0)
	if len(got.Clients) != 1 || got.Clients[0].Name != "norea" || got.Clients[0].TotalTokens != 50 {
		t.Fatalf("reloaded = %+v (the 40-day-old bucket must be dropped)", got)
	}
	if got.Clients[0].Pool != "shared" {
		t.Fatalf("pool = %q", got.Clients[0].Pool)
	}
}
