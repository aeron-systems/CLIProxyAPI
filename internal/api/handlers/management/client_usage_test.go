package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGetClientUsage_FilesClientsUnderTheirConfiguredGroup(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.PoolClients = []config.PoolClient{
		{Name: "group-test-moon-user", Pool: "shared", Group: " Moon "},
		{Name: "group-test-plain", Pool: "shared"},
		{Name: "group-test-silent", Pool: "shared", Group: "moon"},
	}
	clientusage.Default().Add(time.Now(), "group-test-moon-user", "shared", clientusage.Counts{Requests: 1, TotalTokens: 10})
	clientusage.Default().Add(time.Now(), "group-test-plain", "shared", clientusage.Counts{Requests: 1, TotalTokens: 5})

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/clients?window=1h", nil)
	h.GetClientUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body clientusage.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{}
	for _, c := range body.Clients {
		groups[c.Name] = c.Group
	}
	if groups["group-test-moon-user"] != "moon" {
		t.Fatalf("moon user group = %q, want moon (lower-cased, trimmed)", groups["group-test-moon-user"])
	}
	if _, ok := groups["group-test-silent"]; !ok || groups["group-test-silent"] != "moon" {
		t.Fatalf("a grouped client with no traffic must still be listed: %v", groups)
	}
	if groups["group-test-plain"] != "" {
		t.Fatalf("plain client group = %q, want none", groups["group-test-plain"])
	}
}

func TestGetClientUsage_RejectsUnknownWindow(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/clients?window=3y", nil)
	h.GetClientUsage(ginCtx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGetClientCost_PricesRowsAndWarnsOnSpillIntoPaid(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.PoolClients = []config.PoolClient{{Name: "cost-test-moon", Pool: "shared", Group: "moon"}}
	cfg.Routing.Pools = []config.CredentialPool{{Name: "shared-spill", Fallback: "paid"}}
	counts := clientusage.Counts{Requests: 1, OutputTokens: 1_000_000, TotalTokens: 1_000_000}
	clientusage.Default().AddModel(time.Now(), "cost-test-moon", "shared", "claude-opus-5-5", counts, counts)
	clientusage.Default().AddModel(time.Now(), "cost-test-none", "shared", "cost-test-model", counts, counts)

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/cost?window=1h", nil)
	h.GetClientCost(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Clients []struct {
			Name             string   `json:"name"`
			Group            string   `json:"group"`
			Dollars          *float64 `json:"dollars"`
			EstimatedDollars float64  `json:"estimated_dollars"`
		} `json:"clients"`
		UnpricedModels []string `json:"unpriced_models"`
		Prices         struct {
			AsOf string `json:"as_of"`
		} `json:"prices"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range body.Clients {
		switch row.Name {
		case "cost-test-moon":
			seen++
			if row.Group != "moon" || row.Dollars == nil || *row.Dollars != 20 || row.EstimatedDollars != 20 {
				t.Fatalf("moon row = %+v", row)
			}
		case "cost-test-none":
			seen++
			if row.Dollars != nil {
				t.Fatalf("no-price row must be null, got %v", *row.Dollars)
			}
		}
	}
	if seen != 2 || body.Prices.AsOf == "" || len(body.Warnings) != 1 {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestGetClientCost_RejectsBadWindow(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/cost?window=2y", nil)
	h.GetClientCost(ginCtx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}
