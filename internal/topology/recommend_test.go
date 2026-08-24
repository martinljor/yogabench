package topology

import "testing"

// Load is jobs PER SLOT. Counting jobs made a 24-slot proxy with 3 jobs look
// busier than a 4-slot proxy with 2, which is backwards.
func TestPerSlotLoad(t *testing.T) {
	big := &pxInfo{name: "big", slots: 24, jobs: 3}
	small := &pxInfo{name: "small", slots: 4, jobs: 2}
	if big.perSlot() >= small.perSlot() {
		t.Fatalf("per slot: big=%.2f small=%.2f — the 24-slot proxy must be the lighter one",
			big.perSlot(), small.perSlot())
	}
	// Unknown slots fall back to the plain job count.
	unknown := &pxInfo{name: "u", slots: 0, jobs: 5}
	if unknown.perSlot() != 5 {
		t.Errorf("unknown slots: got %.1f, want 5", unknown.perSlot())
	}
	// No jobs is the lightest of all.
	if (&pxInfo{slots: 4}).perSlot() != 0 {
		t.Error("a proxy with no jobs should be 0")
	}
}

// The rule that matters: never advise moving load onto the resource the
// assessment already reports as the bottleneck.
func TestBestSkipsTheBottleneck(t *testing.T) {
	vi := map[string]*pxInfo{
		"hot":  {id: "hot", name: "proxy-hot", slots: 16, jobs: 1},  // lightest per slot...
		"cool": {id: "cool", name: "proxy-cool", slots: 8, jobs: 3}, // ...but this one is fine
	}
	order := []string{"hot", "cool"}
	hot := map[string]Hotspot{"hot": {ID: "hot", Kind: "proxy", SharePct: 60}}
	isHot := func(id string) (Hotspot, bool) {
		h, ok := hot[id]
		return h, ok && h.SharePct >= hotShare
	}
	best := func() *pxInfo {
		var b *pxInfo
		for _, id := range order {
			if _, bad := isHot(id); bad {
				continue
			}
			if p := vi[id]; b == nil || p.perSlot() < b.perSlot() {
				b = p
			}
		}
		return b
	}
	got := best()
	if got == nil || got.id != "cool" {
		t.Fatalf("got %v, want proxy-cool: proxy-hot is lighter per slot but it is the bottleneck", got)
	}
	// Below the share threshold it is a valid target again.
	hot["hot"] = Hotspot{ID: "hot", SharePct: hotShare - 1}
	if got := best(); got == nil || got.id != "hot" {
		t.Fatalf("got %v, want proxy-hot once it is no longer a hotspot", got)
	}
}
