package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// GetRoutingQuotas reports every account's polled usage windows (5-hour, 7-day,
// per-model 7-day, with reset times) and which account a new session would go
// to. Read-only: it reads the poller's last snapshot and never polls.
//
// Query: model (default claude-opus-5), and pool or client (a pool-client name
// whose pool is used). With neither, the default pool.
func (h *Handler) GetRoutingQuotas(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "claude-opus-5"
	}
	pool := strings.TrimSpace(c.Query("pool"))
	client := strings.TrimSpace(c.Query("client"))
	strategy := ""
	if h != nil && h.cfg != nil {
		strategy = h.cfg.Routing.Strategy
		if pool == "" && client != "" {
			for _, pc := range h.cfg.Routing.PoolClients {
				if pc.Name == client {
					pool = pc.Pool
				}
			}
		}
		if pool == "" {
			pool = h.cfg.Routing.DefaultPool
		}
	}
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{"model": model, "pool": pool, "credentials": []any{}, "order": []string{}})
		return
	}
	c.JSON(http.StatusOK, h.authManager.QuotaView(c.Request.Context(), pool, model, strategy))
}
