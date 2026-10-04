package auth

import (
	"testing"
	"time"
)

func TestBuildQuotaViewOrdersAndReportsWindows(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a.json", Provider: "claude", Status: StatusActive}
	b := &Auth{ID: "b.json", Provider: "claude", Status: StatusActive}
	c := &Auth{ID: "c.json", Provider: "claude", Status: StatusActive}
	storePolledQuota("a.json", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(72 * time.Hour), utilization: 40}})
	storePolledQuota("b.json", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(24 * time.Hour), utilization: 80},
		fiveHour: polledWindow{reset: now.Add(time.Hour), utilization: 12}})
	storePolledQuota("c.json", &polledQuota{observedAt: now, weekly: polledWindow{reset: now.Add(2 * time.Hour), utilization: 100, exhausted: true}})
	defer func() {
		for _, id := range []string{"a.json", "b.json", "c.json"} {
			polledQuotas.Delete(id)
		}
	}()

	view := BuildQuotaView([]*Auth{a, b, c}, "", "claude-opus-5", "reset-first", now)
	if view.Next != "b.json" {
		t.Fatalf("next = %q, want b.json (soonest reset that is not used up)", view.Next)
	}
	if len(view.Order) != 3 || view.Order[2] != "c.json" {
		t.Fatalf("order = %v, want the used-up account last", view.Order)
	}
	for _, cr := range view.Credentials {
		switch cr.ID {
		case "b.json":
			if cr.FiveHour == nil || cr.FiveHour.Utilization != 12 || !cr.Usable {
				t.Fatalf("b: %+v", cr)
			}
		case "c.json":
			if cr.Usable || cr.SevenDay == nil || !cr.SevenDay.Exhausted {
				t.Fatalf("c should be shown used up and not usable: %+v", cr)
			}
		}
	}
}
