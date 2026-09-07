package analysis

// Proxy sizing: task slots vs what the host hardware sustains (1 slot = 1 core
// + 2 GB RAM) — the health-check math, computed remotely.

import "testing"

func TestSizingState(t *testing.T) {
	cases := []struct {
		slots, viable int
		want          string
	}{
		{8, 4, "deficit"},   // 8 slots on hardware for 4
		{4, 8, "oversized"}, // hardware sustains twice the slots
		{4, 6, "ok"},
		{4, 4, "ok"},
		{4, 0, "nodata"},
		{0, 4, "nodata"},
	}
	for _, c := range cases {
		if got := sizingState(c.slots, c.viable); got != c.want {
			t.Errorf("slots=%d viable=%d: got %s, want %s", c.slots, c.viable, got, c.want)
		}
	}
}

func TestAddSizingFiresOnDeficit(t *testing.T) {
	a := &Assessment{}
	a.AddSizing([]ProxySizing{
		{Proxy: "proxy01", Host: "h1", Slots: 16, Cores: 4, RamGB: 8, Viable: 4, Source: "manual", State: "deficit", Overload: 12},
		{Proxy: "proxy02", Host: "h2", Slots: 4, Cores: 8, RamGB: 32, Viable: 8, Source: "metrics", State: "ok"},
	})
	if len(a.Sizing) != 2 {
		t.Fatalf("rows: got %d, want 2", len(a.Sizing))
	}
	if len(a.Actions) != 1 || a.Actions[0].Code != "act.envSizing" || a.Actions[0].Impact != "high" {
		t.Fatalf("got %+v, want one high act.envSizing (16 slots on 4 viable = 2x oversubscribed)", a.Actions)
	}
	// All ok: no action, rows kept for the table.
	b := &Assessment{}
	b.AddSizing([]ProxySizing{{Proxy: "p", State: "ok"}, {Proxy: "q", State: "nodata"}})
	if len(b.Actions) != 0 || len(b.Sizing) != 2 {
		t.Errorf("ok/nodata must not fire: actions=%v", codes(b))
	}
}
