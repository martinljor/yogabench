package topology

// recommend.go — proxy/repository assignment advice for the Architecture view.
//
// It reasons in three layers, and each one changes the answer:
//   1. Config    (v1/jobs)      : who points at whom, fixed vs automatic proxy.
//   2. Capacity  (maxTaskCount) : load is jobs PER SLOT, not jobs. A job on a
//      24-slot proxy does not weigh the same as one on a 4-slot proxy, so
//      "least loaded" counted in jobs picks the wrong target.
//   3. Observed  (assessment)   : which resource actually tops out, weighted by
//      data moved. Without it the advice can move load ONTO the bottleneck.
//
// Layer 3 is optional: it is only available once the environment assessment has
// been computed in this session (the Global analysis). The caller passes it in, so
// this package does not depend on the analysis package.
// Read-only.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"yogabench/internal/vbr"
)

const (
	// jobsPerSlotGap: difference in jobs-per-slot that makes a rebalance worth
	// suggesting. Below this the imbalance is noise.
	jobsPerSlotGap = 0.5
	// hotShare: % of the binding data that makes a resource a bottleneck we must
	// not move more load onto. Same threshold the assessment uses.
	hotShare = 35
)

// Hotspot: a resource the assessment saw topping out (layer 3). Kept minimal so
// this package stays independent of the analysis package.
type Hotspot struct {
	ID       string
	Kind     string // proxy | repository
	Stage    string
	SharePct int
	MBps     float64
}

// Recommendation: one row of the advice table. Why carries the numbers that back
// the suggestion — it is what gets shown to a customer.
type Recommendation struct {
	Job     string `json:"job"`
	Current string `json:"current"`
	Suggest string `json:"suggest"`
	Why     string `json:"why"`
	Kind    string `json:"kind"`   // auto | balance | ok | hotspot
	Impact  string `json:"impact"` // high | medium | ok
}

// ResourceLoad: what each proxy/repository carries. Replaces the amber badge on
// the diagram node, which showed a count with no way to tell if it was a problem.
type ResourceLoad struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"` // proxy | repository
	Slots       int     `json:"slots"`
	Jobs        int     `json:"jobs"`
	JobsPerSlot float64 `json:"jobsPerSlot"`
	State       string  `json:"state"`              // hotspot | concentration | headroom | ok
	SharePct    int     `json:"sharePct,omitempty"` // observed: % of the binding data
	MBps        float64 `json:"mbps,omitempty"`
}

// Advice: what the Architecture view renders.
type Advice struct {
	Recommendations []Recommendation `json:"recommendations"`
	Resources       []ResourceLoad   `json:"resources"`
	HasObserved     bool             `json:"hasObserved"` // layer 3 available
}

type pxInfo struct {
	id, name string
	slots    int
	jobs     int
}

// perSlot: with unknown slots we fall back to the plain job count, which is what
// the old advice did for everything.
func (p *pxInfo) perSlot() float64 {
	if p.slots <= 0 {
		return float64(p.jobs)
	}
	return float64(p.jobs) / float64(p.slots)
}

// Recommend builds the advice. hotspots may be nil (layer 3 missing).
func Recommend(ctx context.Context, s *vbr.Session, hotspots []Hotspot) (Advice, error) {
	var out Advice
	proxies, err := getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000")
	if err != nil {
		return out, err
	}
	hot := map[string]Hotspot{}
	for _, h := range hotspots {
		hot[h.ID] = h
	}
	out.HasObserved = len(hotspots) > 0
	isHot := func(id string) (Hotspot, bool) {
		h, ok := hot[id]
		return h, ok && h.SharePct >= hotShare
	}

	// Layers 1+2: VMware proxies with their slots (that is where the proxy is
	// chosen per job).
	vi := map[string]*pxInfo{}
	var order []string
	for _, p := range proxies {
		if !strings.EqualFold(str(p["type"]), "ViProxy") {
			continue
		}
		id := str(p["id"])
		info := &pxInfo{id: id, name: strOr(p["name"], "proxy")}
		if srv, ok := p["server"].(map[string]any); ok {
			info.slots = toInt(srv["maxTaskCount"])
		}
		vi[id] = info
		order = append(order, id)
	}

	jobs, _ := getItems(ctx, s, jobsPath) // if it fails we still report the load
	repoJobs := map[string]int{}
	type jinfo struct {
		name string
		auto bool
		pin  []string
	}
	var vmJobs []jinfo
	for _, j := range jobs {
		st := nestedMap(j, "storage")
		if rid := str(st["backupRepositoryId"]); rid != "" {
			repoJobs[rid]++
		}
		t := strings.ToLower(str(j["type"]))
		if !strings.Contains(t, "vsphere") && !strings.Contains(t, "vmware") && !strings.Contains(t, "clouddirector") {
			continue // only VMware: that is where the proxy is chosen per job
		}
		bp := nestedMap(st, "backupProxies")
		auto, _ := bp["autoSelectEnabled"].(bool)
		pin := strSlice(bp["proxyIds"])
		vmJobs = append(vmJobs, jinfo{strOr(j["name"], "job"), auto, pin})
		if !auto {
			for _, id := range pin {
				if p := vi[id]; p != nil {
					p.jobs++
				}
			}
		}
	}

	// best: least loaded per slot, skipping anything the assessment flagged as the
	// bottleneck — moving load onto it would make things worse.
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

	for _, jb := range vmJobs {
		if !jb.auto {
			// Pinned to a resource the assessment says is the bottleneck.
			for _, id := range jb.pin {
				h, bad := isHot(id)
				if !bad || vi[id] == nil {
					continue
				}
				if b := best(); b != nil && b.id != id {
					out.Recommendations = append(out.Recommendations, Recommendation{
						Job: jb.name, Kind: "hotspot", Impact: "high",
						Current: vi[id].name,
						Suggest: "move to " + b.name,
						Why: fmt.Sprintf("%s is the bottleneck for %d%% of the data; %s has %d slots and %d job(s)",
							vi[id].name, h.SharePct, b.name, b.slots, b.jobs),
					})
				}
				break
			}
			continue
		}
		b := best()
		if b == nil {
			continue
		}
		out.Recommendations = append(out.Recommendations, Recommendation{
			Job: jb.name, Kind: "auto", Impact: "medium",
			Current: "automatic", Suggest: b.name,
			Why: fmt.Sprintf("%d slots, %d job(s) — least loaded per slot", b.slots, b.jobs),
		})
		b.jobs++ // count it so several automatic jobs get spread
	}

	// Rebalance, measured per slot.
	if len(vi) >= 2 {
		var hi, lo *pxInfo
		for _, id := range order {
			p := vi[id]
			if hi == nil || p.perSlot() > hi.perSlot() {
				hi = p
			}
			if lo == nil || p.perSlot() < lo.perSlot() {
				lo = p
			}
		}
		if hi != nil && lo != nil && hi != lo && hi.perSlot()-lo.perSlot() >= jobsPerSlotGap && hi.jobs >= 2 {
			why := fmt.Sprintf("%.1f vs %.1f jobs per slot", hi.perSlot(), lo.perSlot())
			if h, bad := isHot(hi.id); bad {
				why += fmt.Sprintf("; it is the bottleneck for %d%% of the data", h.SharePct)
			}
			out.Recommendations = append(out.Recommendations, Recommendation{
				Job: "(load balancing)", Kind: "balance", Impact: "medium",
				Current: fmt.Sprintf("%s: %d job(s) on %d slots", hi.name, hi.jobs, hi.slots),
				Suggest: "move some to " + lo.name,
				Why:     why,
			})
		}
	}

	// Jobs that need no change are listed too: "this one is fine" is an answer, and
	// it keeps the reader from wondering whether we skipped them.
	if len(out.Recommendations) > 0 {
		named := map[string]bool{}
		for _, r := range out.Recommendations {
			named[r.Job] = true
		}
		for _, jb := range vmJobs {
			if jb.auto || named[jb.name] {
				continue
			}
			name := "—"
			if len(jb.pin) > 0 && vi[jb.pin[0]] != nil {
				name = vi[jb.pin[0]].name
			}
			out.Recommendations = append(out.Recommendations, Recommendation{
				Job: jb.name, Kind: "ok", Impact: "ok",
				Current: name, Suggest: "no change", Why: "pinned to a proxy with headroom",
			})
		}
	}

	// Load per resource: proxies (jobs per slot) and repositories (jobs pointing at
	// them), ordered by how loaded they are.
	for _, id := range order {
		p := vi[id]
		rl := ResourceLoad{ID: id, Name: p.name, Kind: "proxy", Slots: p.slots, Jobs: p.jobs,
			JobsPerSlot: round1(p.perSlot()), State: "ok"}
		switch h, bad := isHot(id); {
		case bad:
			rl.State, rl.SharePct, rl.MBps = "hotspot", h.SharePct, h.MBps
		case p.perSlot() >= 1:
			rl.State = "concentration"
		case p.jobs == 0 || p.perSlot() <= 0.25:
			rl.State = "headroom"
		}
		out.Resources = append(out.Resources, rl)
	}
	repos, _ := AllRepositories(ctx, s)
	for _, rp := range repos {
		id := str(rp["id"])
		n := repoJobs[id]
		if n == 0 {
			continue // repositories nothing writes to would only add noise
		}
		slots := toInt(nestedMap(rp, "repository")["maxTaskCount"])
		rl := ResourceLoad{ID: id, Name: strOr(rp["name"], "repo"), Kind: "repository",
			Slots: slots, Jobs: n, State: "ok"}
		if slots > 0 {
			rl.JobsPerSlot = round1(float64(n) / float64(slots))
			if rl.JobsPerSlot >= 1 {
				rl.State = "concentration"
			}
		}
		if h, bad := isHot(id); bad {
			rl.State, rl.SharePct, rl.MBps = "hotspot", h.SharePct, h.MBps
		}
		out.Resources = append(out.Resources, rl)
	}
	sort.SliceStable(out.Resources, func(i, j int) bool {
		return out.Resources[i].JobsPerSlot > out.Resources[j].JobsPerSlot
	})
	rank := map[string]int{"high": 0, "medium": 1, "ok": 2}
	sort.SliceStable(out.Recommendations, func(i, j int) bool {
		return rank[out.Recommendations[i].Impact] < rank[out.Recommendations[j].Impact]
	})
	return out, nil
}

// nestedMap navega un camino de claves devolviendo el mapa final (o vacío).
func nestedMap(obj map[string]any, keys ...string) map[string]any {
	cur := obj
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		cur = next
	}
	return cur
}

func strSlice(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		if s := str(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
