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
