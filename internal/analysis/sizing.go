package analysis

// sizing.go — proxy sizing verdict, the same math the field health check runs
// with local PowerShell, done remotely: task slots versus what the host's
// hardware sustains (best practice: 1 task slot = 1 core + 2 GB RAM).
//
// Where the resources come from, in order:
//   metrics — Node Exporter on v13.1+ appliances (read-only HTTP, no creds)
//   manual  — cores/RAM the user entered for the host (any version, any OS)
// Hosts with neither stay "no data": the verdict is not guessed.

import (
	"context"
	"fmt"
	"strings"

	"yogabench/internal/vbr"
)

// ProxySizing: one proxy's slots against its host's resources.
type ProxySizing struct {
	Proxy    string `json:"proxy"`
	Host     string `json:"host"`
	Slots    int    `json:"slots"`
	Cores    int    `json:"cores"`
	RamGB    int    `json:"ramGB"`
	Viable   int    `json:"viable"` // min(cores, RAM/2GB)
	Source   string `json:"source"` // metrics | manual | none
	State    string `json:"state"`  // deficit | oversized | ok | nodata
	Overload int    `json:"overload,omitempty"`
}

// sizingState: deficit when the slots exceed what the hardware sustains;
// oversized when the hardware sustains at least twice the configured slots.
func sizingState(slots, viable int) string {
	switch {
	case viable <= 0 || slots <= 0:
		return "nodata"
	case slots > viable:
		return "deficit"
	case viable >= slots*2:
		return "oversized"
	default:
		return "ok"
	}
}

// BuildSizing computes the sizing rows for every proxy whose host resources are
// known. probe scrapes an appliance's metrics endpoint (nil disables scraping —
// used by tests and the demo).
func BuildSizing(ctx context.Context, s *vbr.Session, probe func(string) (int, int, bool)) []ProxySizing {
	servers := map[string]map[string]any{}
	for _, m := range getItems(ctx, s, "v1/backupInfrastructure/managedServers?limit=1000") {
		servers[str(m["id"])] = m
	}
	manual := s.HostResAll()

	var out []ProxySizing
	for _, p := range getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000") {
		srv, _ := p["server"].(map[string]any)
		hostID := str(srv["hostId"])
		host := servers[hostID]
		row := ProxySizing{
			Proxy: strOr(p["name"], "proxy"),
			Host:  strOr(host["name"], hostID),
			Slots: toInt(srv["maxTaskCount"]),
			State: "nodata", Source: "none",
		}
		if r, ok := manual[hostID]; ok && (r.Cores > 0 || r.RamGB > 0) {
			row.Cores, row.RamGB, row.Source = r.Cores, r.RamGB, "manual"
		} else if probe != nil && boolOf(host["isVBRLinuxAppliance"]) {
			if r, ok := s.AutoRes(strOr(host["name"], hostID), probe); ok {
				row.Cores, row.RamGB, row.Source = r.Cores, r.RamGB, "metrics"
			}
		}
		if row.Cores > 0 && row.RamGB > 0 {
			row.Viable = row.Cores
			if byRAM := row.RamGB / 2; byRAM < row.Viable {
				row.Viable = byRAM
			}
			row.State = sizingState(row.Slots, row.Viable)
			if row.State == "deficit" {
				row.Overload = row.Slots - row.Viable
			}
		}
		out = append(out, row)
	}
	return out
}

// AddSizing folds the sizing rows into the assessment: a tile-worthy summary
// plus an action when there is a deficit.
func (a *Assessment) AddSizing(rows []ProxySizing) {
	if a == nil || len(rows) == 0 {
		return
	}
	a.Sizing = rows
	var deficit []ProxySizing
	for _, r := range rows {
		if r.State == "deficit" {
			deficit = append(deficit, r)
		}
	}
	if len(deficit) == 0 {
		return
	}
	top := deficit[0]
	var names []string
	for _, d := range deficit {
		names = append(names, d.Proxy)
	}
	impact := "medium"
	if top.Overload >= top.Viable { // configured at 2x what the host sustains
		impact = "high"
	}
	a.Actions = rank(append(a.Actions, Action{
		Impact: impact, Code: "act.envSizing", Source: "observed",
		Text: fmt.Sprintf("%d prox(ies) have more task slots than the host sustains (1 slot = 1 core + 2 GB RAM): %s has %d slots on %d cores / %d GB (%d viable). Lower the slots or grow the host — oversubscribed slots queue work and stretch the window.",
			len(deficit), top.Proxy, top.Slots, top.Cores, top.RamGB, top.Viable),
		Params: map[string]any{"n": len(deficit), "proxy": top.Proxy, "slots": top.Slots,
			"cores": top.Cores, "ram": top.RamGB, "viable": top.Viable, "list": strings.Join(names, ", ")},
	}))
}
