package topology

import (
	"context"
	"strconv"
	"strings"

	"yogabench/internal/vbr"
)

// Recommendation: sugerencia de cambio en la asignación de proxies de un job.
type Recommendation struct {
	Job     string `json:"job"`
	Current string `json:"current"`
	Suggest string `json:"suggest"`
	Kind    string `json:"kind"` // auto | balance
}

// Recommend analiza los jobs de VMware y sus proxies (ViProxy) y sugiere:
//   - jobs con proxy AUTOMÁTICO -> asignar un proxy fijo (el de menor carga)
//   - desbalance -> mover jobs del proxy más cargado al menos cargado
//
// Solo lectura. Aplica a VMware (es donde el proxy se asigna por job).
func Recommend(ctx context.Context, s *vbr.Session) ([]Recommendation, error) {
	proxies, err := getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000")
	if err != nil {
		return nil, err
	}
	type px struct{ id, name string }
	var vi []px
	name := map[string]string{}
	for _, p := range proxies {
		if strings.EqualFold(str(p["type"]), "ViProxy") {
			id := str(p["id"])
			nm := strOr(p["name"], "proxy")
			vi = append(vi, px{id, nm})
			name[id] = nm
		}
	}

	jobs, _ := getItems(ctx, s, "v1/jobs?limit=500")
	load := map[string]int{} // proxyId -> nº de jobs VMware asignados explícito
	type jinfo struct {
		name string
		auto bool
	}
	var vmJobs []jinfo
	for _, j := range jobs {
		t := strings.ToLower(str(j["type"]))
		if !strings.Contains(t, "vsphere") && !strings.Contains(t, "vmware") && !strings.Contains(t, "clouddirector") {
			continue // solo VMware: ahí el proxy se elige por job
		}
		bp := nestedMap(j, "storage", "backupProxies")
		auto, _ := bp["autoSelectEnabled"].(bool)
		vmJobs = append(vmJobs, jinfo{strOr(j["name"], "job"), auto})
		if !auto {
			for _, id := range strSlice(bp["proxyIds"]) {
				load[id]++
			}
		}
	}

	leastLoaded := func() (string, string) {
		best, bn := "", 1<<30
		for _, p := range vi {
			if load[p.id] < bn {
				bn, best = load[p.id], p.id
			}
		}
		return best, name[best]
	}

	var recs []Recommendation
	// 1. Jobs en automático -> sugerir proxy fijo (el menos cargado), y contarlo
	//    hipotéticamente para repartir los siguientes.
	for _, jb := range vmJobs {
		if jb.auto && len(vi) > 0 {
			lid, lname := leastLoaded()
			recs = append(recs, Recommendation{
				Job: jb.name, Kind: "auto",
				Current: "automatic proxy",
				Suggest: "assign a fixed proxy: " + lname + " (least loaded)",
			})
			load[lid]++
		}
	}
	// 2. Balance entre ViProxies (si uno concentra >= 2 jobs más que otro).
	if len(vi) >= 2 {
		maxP, minP, maxN, minN := "", "", -1, 1<<30
		for _, p := range vi {
			n := load[p.id]
			if n > maxN {
				maxN, maxP = n, p.name
			}
			if n < minN {
				minN, minP = n, p.name
			}
		}
		if maxN-minN >= 2 {
			recs = append(recs, Recommendation{
				Job: "(load balancing)", Kind: "balance",
				Current: maxP + " handles " + strconv.Itoa(maxN) + " job(s)",
				Suggest: "move some to " + minP + " (" + strconv.Itoa(minN) + " job(s))",
			})
		}
	}
	return recs, nil
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
