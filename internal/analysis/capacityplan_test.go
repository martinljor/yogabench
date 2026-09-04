package analysis

// The three conclusion tiles: time waiting for slots, machines without a fresh
// copy, and days until the busiest repository fills.

import (
	"testing"
	"time"
)

func TestWaitSecFromLogs(t *testing.T) {
	logs := []map[string]any{
		{"title": "Job started", "startTime": "2026-09-01T22:00:00.000000-07:00", "updateTime": "2026-09-01T22:00:00.000000-07:00"},
		// 10 minutes queued: the interval of the record IS the wait.
		{"title": "Resource not ready: backup repository", "startTime": "2026-09-01T22:00:05.000000-07:00", "updateTime": "2026-09-01T22:10:05.000000-07:00"},
		{"title": "Queued for processing at 9/1/2026 10:10:05 PM", "startTime": "2026-09-01T22:10:05.000000-07:00", "updateTime": "2026-09-01T22:12:05.000000-07:00"},
		{"title": "Load: Source 30% > Proxy 40% > Network 20% > Target 96%"},
	}
	if got := waitSecFromLogs(logs); got != 720 {
		t.Errorf("wait: got %.0fs, want 720 (10m + 2m)", got)
	}
	if got := waitSecFromLogs(nil); got != 0 {
		t.Errorf("no logs: got %.0f, want 0", got)
	}
}

// A machine whose last good copy is fresh is not at risk; one whose latest copy
// is old, or whose only copies failed, is.
func TestStaleMachines(t *testing.T) {
	now := time.Now()
	mk := func(job, when string, tasks ...Task) Record {
		return Record{JobID: job, Name: job, CreationTime: when, EndTime: when, Tasks: tasks, TransferredSize: 2 * gib,
			Bottleneck: map[string]any{"primary": "Source"}}
	}
	tstr := func(d time.Duration) string { return now.Add(-d).Format("2006-01-02T15:04:05.000000-07:00") }
	recs := []Record{
		mk("Job A", tstr(2*time.Hour), Task{Name: "vm-fresh", Result: "Success"}),
		mk("Job A", tstr(30*time.Hour), Task{Name: "vm-old", Result: "Success"}),
		mk("Job B", tstr(3*time.Hour), Task{Name: "vm-neverok", Result: "Failed"}),
	}
	a := BuildAssessment(recs, 7, names, names)
	if a.VMsProtected != 3 {
		t.Fatalf("protected: got %d, want 3", a.VMsProtected)
	}
	if a.VMsFresh != 1 {
		t.Errorf("fresh: got %d, want 1 (only vm-fresh)", a.VMsFresh)
	}
	if len(a.StaleVMs) != 2 {
		t.Fatalf("stale: got %d, want 2", len(a.StaleVMs))
	}
	// No-copy-at-all first, then oldest.
	if a.StaleVMs[0].Name != "vm-neverok" || a.StaleVMs[0].AgeHours != -1 {
		t.Errorf("first stale: got %+v, want vm-neverok with no copy", a.StaleVMs[0])
	}
	if a.StaleVMs[1].Name != "vm-old" || a.StaleVMs[1].AgeHours < 29 || a.StaleVMs[1].AgeHours > 31 {
		t.Errorf("second stale: got %+v, want vm-old at ~30h", a.StaleVMs[1])
	}
}

func TestAddCapacityPicksTheRepoThatFillsFirst(t *testing.T) {
	a := &Assessment{}
	states := []map[string]any{
		{"id": "r1", "name": "Big", "freeGB": float64(10000)}, // ~1000 dias
		{"id": "r2", "name": "Tight", "freeGB": float64(100)}, // 100 GiB libres
		{"id": "r3", "name": "Idle", "freeGB": float64(1)},    // sin escritura -> se ignora
	}
	perDay := map[string]int64{"r1": 10 * gib, "r2": 10 * gib}
	a.AddCapacity(states, map[string]string{"r2": "Tight Repo"}, perDay)
	if a.FillRepo != "Tight Repo" || a.FillDays != 10 {
		t.Fatalf("got %q %dd, want Tight Repo 10d", a.FillRepo, a.FillDays)
	}
	a.FinishActions()
	if !hasEnvAction(a, "act.envSpace") {
		t.Errorf("expected act.envSpace under 30 days; actions: %v", codes(a))
	}
}

func TestFinishActionsWaits(t *testing.T) {
	a := &Assessment{WaitSec: 3600, WaitPct: 30}
	a.FinishActions()
	if len(a.Actions) != 1 || a.Actions[0].Code != "act.envWaits" || a.Actions[0].Impact != "high" {
		t.Fatalf("got %+v, want one high act.envWaits", a.Actions)
	}
	// Under the floor: no action (5 minutes of waiting is not a finding).
	b := &Assessment{WaitSec: 200, WaitPct: 40}
	b.FinishActions()
	if len(b.Actions) != 0 {
		t.Errorf("short waits must not fire: %v", codes(b))
	}
}

func TestRepoBytesPerDayUsesActualSpan(t *testing.T) {
	recs := []Record{
		{CreationTime: "2026-09-01T22:00:00.000000-07:00", RepoIDs: []string{"r1"}, TransferredSize: 10 * gib},
		{CreationTime: "2026-09-05T22:00:00.000000-07:00", RepoIDs: []string{"r1"}, TransferredSize: 10 * gib},
	}
	got := RepoBytesPerDay(recs)
	// 20 GiB over a 5-day span -> 4 GiB/day.
	if want := 4 * gib; got["r1"] != want {
		t.Errorf("per day: got %d, want %d", got["r1"], want)
	}
}
