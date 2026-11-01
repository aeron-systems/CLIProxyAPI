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
	c.JSON(http.StatusOK, clientusage.Default().Summarize(label, window))
}
