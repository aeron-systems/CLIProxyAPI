package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
)

var clientUsageWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"all": 0,
}

// GetClientUsage reports how much each client app used the proxy, with each
// app's share of the total, over the last window (query: window = 1h, 24h, 7d,
// 30d or all; default 24h). Apps are the pool-client names; the key is never
// returned.
func (h *Handler) GetClientUsage(c *gin.Context) {
	label := strings.ToLower(strings.TrimSpace(c.Query("window")))
	if label == "" {
		label = "24h"
	}
	window, ok := clientUsageWindows[label]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be one of 1h, 24h, 7d, 30d, all"})
		return
	}
	summary := clientusage.Default().Summarize(label, window)
	if h != nil && h.cfg != nil {
		groups := make(map[string]string, len(h.cfg.Routing.PoolClients))
		for _, pc := range h.cfg.Routing.PoolClients {
			if g := strings.ToLower(strings.TrimSpace(pc.Group)); g != "" {
				groups[pc.Name] = g
			}
		}
		seen := make(map[string]bool, len(summary.Clients))
		for i := range summary.Clients {
			summary.Clients[i].Group = groups[summary.Clients[i].Name]
			seen[summary.Clients[i].Name] = true
		}
		// A grouped client with no traffic in this period still gets its row, so
		// a group always lists all of its members.
		for _, pc := range h.cfg.Routing.PoolClients {
			if g := groups[pc.Name]; g != "" && !seen[pc.Name] {
				seen[pc.Name] = true
				summary.Clients = append(summary.Clients, clientusage.Client{Name: pc.Name, Pool: pc.Pool, Group: g})
			}
		}
	}
	c.JSON(http.StatusOK, summary)
}
