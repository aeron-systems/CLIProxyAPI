package clientusage

import (
	"sort"
	"strings"
	"time"
)

// IsPaidPool says whether a pool's credentials are billed by the token. Pools
// named "paid" or "api", or starting with "paid-" or "api-", count as real spend.
// Every other pool is on subscriptions, where the dollars are only what the same
// traffic would cost at list price.
func IsPaidPool(pool string) bool {
	pool = strings.ToLower(strings.TrimSpace(pool))
	return pool == "paid" || pool == "api" || strings.HasPrefix(pool, "paid-") || strings.HasPrefix(pool, "api-")
}

// CostRow is the cost of one client or one model.
type CostRow struct {
	Name  string `json:"name"`
	Pool  string `json:"pool,omitempty"`
	Group string `json:"group,omitempty"`
	Counts
	// Dollars is estimated plus real spend. It is null when the row has tokens
	// and none of them could be priced: that is "no price", which is not $0.
	Dollars          *float64 `json:"dollars"`
	RealDollars      float64  `json:"real_dollars"`
	EstimatedDollars float64  `json:"estimated_dollars"`
	// UnpricedTokens are in the row's tokens but not in its dollars, so a row
	// with both is a lower bound.
	UnpricedTokens int64 `json:"unpriced_tokens"`
}

// CostSummary is the answer to "what did each client and model cost over <window>".
type CostSummary struct {
	Window  string    `json:"window"`
	Since   string    `json:"since"`
	Total   CostRow   `json:"total"`
	Clients []CostRow `json:"clients"`
	Models  []CostRow `json:"models"`
	// UnpricedModels names the models with tokens but no price.
	UnpricedModels []string `json:"unpriced_models"`
}

func (r *CostRow) addCost(pool string, m Counts, price ModelPrice, priced bool) {
	r.add(m)
	if !priced {
		r.UnpricedTokens += m.TotalTokens
		return
	}
	r.UnpricedTokens += m.UnclassifiedTokens
	if IsPaidPool(pool) {
		r.RealDollars += price.Dollars(m)
	} else {
		r.EstimatedDollars += price.Dollars(m)
	}
}

func (r *CostRow) finish() {
	if r.UnpricedTokens > 0 && r.UnpricedTokens >= r.TotalTokens {
		r.Dollars = nil
		return
	}
	dollars := r.RealDollars + r.EstimatedDollars
	r.Dollars = &dollars
}

// Cost prices every bucket over the last window. A zero window means all
// retained history. Each bucket is priced by the pool the client was in at the
// time, so moving a client to another pool does not rewrite what it already cost.
func (s *Store) Cost(label string, window time.Duration, prices *PriceTable) CostSummary {
	now := s.now()
	sum := CostSummary{Window: label, Total: CostRow{Name: "total"}, Clients: []CostRow{}, Models: []CostRow{}, UnpricedModels: []string{}}
	var since time.Time
	if window > 0 {
		since = now.Add(-window).Truncate(time.Hour)
		sum.Since = since.UTC().Format(time.RFC3339)
	}
	clients := make(map[string]*CostRow)
	models := make(map[string]*CostRow)
	unpriced := make(map[string]bool)
	s.mu.Lock()
	for hour, byClient := range s.data {
		if !since.IsZero() && hour < since.Unix() {
			continue
		}
		for name, cl := range byClient {
			client := clients[name]
			if client == nil {
				client = &CostRow{Name: name}
				clients[name] = client
			}
			if cl.Pool != "" {
				client.Pool = cl.Pool
			}
			for model, m := range cl.Models {
				row := models[model]
				if row == nil {
					row = &CostRow{Name: model}
					models[model] = row
				}
				price, priced := prices.Lookup(model)
				if !priced && m.TotalTokens > 0 {
					unpriced[model] = true
				}
				client.addCost(cl.Pool, *m, price, priced)
				row.addCost(cl.Pool, *m, price, priced)
				sum.Total.addCost(cl.Pool, *m, price, priced)
			}
		}
	}
	s.mu.Unlock()
	sum.Total.finish()
	sum.Clients = sortedCostRows(clients)
	sum.Models = sortedCostRows(models)
	// A model with no price never reads as $0, even if all it has is failed requests.
	for i := range sum.Models {
		if _, priced := prices.Lookup(sum.Models[i].Name); !priced {
			sum.Models[i].Dollars = nil
		}
	}
	for model := range unpriced {
		sum.UnpricedModels = append(sum.UnpricedModels, model)
	}
	sort.Strings(sum.UnpricedModels)
	return sum
}

// sortedCostRows orders by dollars, then rows with no price by their tokens.
func sortedCostRows(rows map[string]*CostRow) []CostRow {
	out := make([]CostRow, 0, len(rows))
	for _, row := range rows {
		row.finish()
		out = append(out, *row)
	}
	dollars := func(r CostRow) float64 {
		if r.Dollars == nil {
			return 0
		}
		return *r.Dollars
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if dollars(a) != dollars(b) {
			return dollars(a) > dollars(b)
		}
		if a.TotalTokens != b.TotalTokens {
			return a.TotalTokens > b.TotalTokens
		}
		return a.Name < b.Name
	})
	return out
}
