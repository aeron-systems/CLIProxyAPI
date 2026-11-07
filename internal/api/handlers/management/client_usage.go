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
	label, window, ok := usageWindow(c)
	if !ok {
		return
	}
	summary := clientusage.Default().Summarize(label, window)
	if h != nil && h.cfg != nil {
		groups := h.clientGroups()
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

// usageWindow reads the window query (default 24h), answering 400 if it is not valid.
func usageWindow(c *gin.Context) (string, time.Duration, bool) {
	label := strings.ToLower(strings.TrimSpace(c.Query("window")))
	if label == "" {
		label = "24h"
	}
	window, ok := clientUsageWindows[label]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be one of 1h, 24h, 7d, 30d, all"})
		return "", 0, false
	}
	return label, window, true
}

func (h *Handler) clientGroups() map[string]string {
	groups := make(map[string]string, len(h.cfg.Routing.PoolClients))
	for _, pc := range h.cfg.Routing.PoolClients {
		if g := strings.ToLower(strings.TrimSpace(pc.Group)); g != "" {
			groups[pc.Name] = g
		}
	}
	return groups
}

// costResponse is the cost route's answer: dollars per client and per model, and
// what those dollars rest on.
type costResponse struct {
	clientusage.CostSummary
	Prices costPrices `json:"prices"`
	// Warnings are reasons the dollars may mislead, in plain words.
	Warnings []string `json:"warnings"`
}

type costPrices struct {
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
}

// GetClientCost reports what each client and model cost over the last window, at
// the prices in the price file. Traffic on subscription pools is an estimate at
// list price (estimated_dollars); only pools billed by the token are real spend
// (real_dollars). A row with tokens but no price has "dollars": null, never 0.
func (h *Handler) GetClientCost(c *gin.Context) {
	label, window, ok := usageWindow(c)
	if !ok {
		return
	}
	table, problem := clientusage.DefaultPrices().Current()
	resp := costResponse{
		CostSummary: clientusage.Default().Cost(label, window, table),
		Prices:      costPrices{AsOf: table.AsOf, Source: table.Source},
		Warnings:    []string{},
	}
	if problem != "" {
		resp.Warnings = append(resp.Warnings, "The price file could not be read ("+problem+"), so the built-in prices were used.")
	}
	if len(table.Invalid) > 0 {
		resp.Warnings = append(resp.Warnings, "These price rows have a missing or negative rate and were ignored: "+strings.Join(table.Invalid, ", ")+".")
	}
	if h != nil && h.cfg != nil {
		groups := h.clientGroups()
		for i := range resp.Clients {
			resp.Clients[i].Group = groups[resp.Clients[i].Name]
		}
		// A client is labelled by its own pool, so traffic that spills over into
		// a paid pool would be counted as an estimate although it was really billed.
		for _, pool := range h.cfg.Routing.Pools {
			if clientusage.IsPaidPool(pool.Fallback) {
				resp.Warnings = append(resp.Warnings, "Pool \""+pool.Name+"\" can fall back to a paid pool; anything that spills over is counted here as an estimate, not real spend.")
			}
		}
	}
	c.JSON(http.StatusOK, resp)
}
