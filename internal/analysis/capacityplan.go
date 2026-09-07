package analysis

// capacityplan.go — the three "conclusion tiles" of the environment verdict:
// time spent waiting for slots, machines without a fresh copy, and days until
// the busiest repository fills at the observed write rate. Each one exists to
// be a finished conclusion, not a chart the user has to interpret.

import "fmt"

// AddCapacity estimates the space runway per repository: free space (from the
// states endpoint, cached) divided by the bytes the window wrote there per day.
// The estimate ignores retention deletes on purpose — it is the pessimistic
// "at the current write rate" bound, and says so in the UI.
func (a *Assessment) AddCapacity(states []map[string]any, repoNames map[string]string, perDay map[string]int64) {
	if a == nil {
		return
	}
	const gib = int64(1) << 30
	best := -1
	for _, st := range states {
		id := str(st["id"])
		rate := perDay[id]
		if rate < gib/4 { // under ~256 MiB/day the estimate is noise
			continue
		}
		free := num(st["freeGB"]) * gib
		if free <= 0 {
			continue
		}
		days := int(float64(free) / float64(rate))
		if best < 0 || days < best {
			best = days
			a.FillRepo = firstNonEmpty(repoNames[id], str(st["name"]), "repository")
			a.FillDays = days
			a.FillFreeBytes = free
			a.FillPerDay = rate
		}
	}
}

// FinishActions folds the waiting/stale/space findings into the action list.
// Called once everything is computed (after AddCapacity and AddReliability).
func (a *Assessment) FinishActions() {
	if a == nil {
		return
	}
	var acts []Action
	add := func(impact, code, text string, params map[string]any) {
		acts = append(acts, Action{Impact: impact, Code: code, Text: text, Params: params, Source: "observed"})
	}
	if a.WaitPct >= 10 && a.WaitSec >= 300 {
		impact := "medium"
		if a.WaitPct >= 25 {
			impact = "high"
		}
		add(impact, "act.envWaits",
			fmt.Sprintf("The runs spent %s waiting for free proxy/repository slots — %d%% of the total job time: staggering starts or adding task slots recovers it without new hardware.",
				secsHuman(a.WaitSec), a.WaitPct),
			map[string]any{"wait": secsHuman(a.WaitSec), "pct": a.WaitPct})
	}
	if n := len(a.StaleVMs); n > 0 {
		top := a.StaleVMs[0]
		age := "no successful copy in the window"
		if top.AgeHours >= 0 {
			age = fmt.Sprintf("oldest: %s, %d h ago", top.Name, top.AgeHours)
		}
		add("medium", "act.envStale",
			fmt.Sprintf("%d machine(s) have no successful copy in the last 24 h (%s).", n, age),
			map[string]any{"n": n, "vm": top.Name, "age": top.AgeHours})
	}
	if a.FillRepo != "" && a.FillDays < 30 {
		impact := "medium"
		if a.FillDays < 7 {
			impact = "high"
		}
		add(impact, "act.envSpace",
			fmt.Sprintf("%s has %s free and receives ~%s/day: about %d day(s) until full at the current write rate (retention deletes not counted).",
				a.FillRepo, bytesHuman(a.FillFreeBytes), bytesHuman(a.FillPerDay), a.FillDays),
			map[string]any{"repo": a.FillRepo, "days": a.FillDays, "free": bytesHuman(a.FillFreeBytes), "rate": bytesHuman(a.FillPerDay)})
	}
	if len(acts) == 0 {
		return
	}
	// Appended, not prepended: within the same impact the stable rank keeps the
	// earlier findings first, and a job failing NOW must stay ahead of these.
	a.Actions = rank(append(a.Actions, acts...))
}

// RepoBytesPerDay: bytes written to each repository per day of the ACTUAL span
// of the analyzed runs (not the requested window, which the session cap can
// leave much wider than the data).
func RepoBytesPerDay(recs []Record) map[string]int64 {
	total := map[string]int64{}
	var lo, hi string
	for _, r := range recs {
		if r.CreationTime != "" && (lo == "" || r.CreationTime < lo) {
			lo = r.CreationTime
		}
		if r.CreationTime > hi {
			hi = r.CreationTime
		}
		for _, id := range r.RepoIDs {
			total[id] += r.TransferredSize
		}
	}
	days := 1
	if tLo, ok1 := parseDT(lo); ok1 {
		if tHi, ok2 := parseDT(hi); ok2 {
			if d := int(tHi.Sub(tLo).Hours()/24) + 1; d > 1 {
				days = d
			}
		}
	}
	out := make(map[string]int64, len(total))
	for id, b := range total {
		out[id] = b / int64(days)
	}
	return out
}

// secsHuman: "2h 06m" / "45m" / "38s".
func secsHuman(sec float64) string {
	s := int(sec + 0.5)
	switch {
	case s >= 3600:
		return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
	case s >= 60:
		return fmt.Sprintf("%dm", s/60)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
