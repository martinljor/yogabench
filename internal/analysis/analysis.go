// Package analysis implementa el "Carril B": estadistica de bottleneck agregada
// por repositorio y por proxy, leyendo la telemetria de los jobs de Veeam
// (sessions + taskSessions + logs). Solo lectura. Las llamadas por sesion
// (taskSessions + logs) se hacen en paralelo con goroutines para escalar en
// ambientes grandes.
package analysis

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"yogabench/internal/vbr"
)

const (
	emptyGUID    = "00000000-0000-0000-0000-000000000000"
	maxSessions  = 80 // cota de sesiones a analizar (cada una = taskSessions+logs)
	fetchWorkers = 8  // goroutines concurrentes para el N+1
)

var (
	jobHints  = []string{"backup", "replica", "restore", "copy"}
	skipHints = []string{"configuration", "malware", "compliance", "infrastructure"}
	loadRe    = regexp.MustCompile(`Source\s+(\d+)%\s*>\s*Proxy\s+(\d+)%\s*>\s*Network\s+(\d+)%\s*>\s*Target\s+(\d+)%`)
	primaryRe = regexp.MustCompile(`Primary bottleneck:\s*(\w+)`)
)

// --- tipos de salida (JSON que consume el frontend) ------------------------

type Task struct {
	Name            string   `json:"name"`
	RepositoryID    string   `json:"repositoryId"`
	Result          string   `json:"result"`
	Bottleneck      string   `json:"bottleneck"`
	ProcessingRate  string   `json:"processingRate"`
	Duration        string   `json:"duration"`
	ProcessedSize   int64    `json:"processedSize"`
	ReadSize        int64    `json:"readSize"`
	TransferredSize int64    `json:"transferredSize"`
	Reduction       *float64 `json:"reduction"`
}

type Record struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Type            string         `json:"type"`
	Operation       string         `json:"operation"`
	Result          string         `json:"result"`
	Message         string         `json:"message"`
	CreationTime    string         `json:"creationTime"`
	EndTime         string         `json:"endTime"`
	Bottleneck      map[string]any `json:"bottleneck"`
	Tasks           []Task         `json:"tasks"`
	ProcessedSize   int64          `json:"processedSize"`
	TransferredSize int64          `json:"transferredSize"`
	RepoIDs         []string       `json:"repoIds"`
	ProxyIDs        []string       `json:"proxyIds"`
}

type Group struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Runs            int            `json:"runs"`
	Results         map[string]int `json:"results"`
	ProcessedSize   int64          `json:"processedSize"`
	TransferredSize int64          `json:"transferredSize"`
	BottleneckAvg   map[string]any `json:"bottleneckAvg"`
	PrimaryCounts   map[string]int `json:"primaryCounts"`
	Jobs            []Record       `json:"jobs"`
}

type RangeInfo struct {
	From          *string `json:"from"`
	To            *string `json:"to"`
	DaysAvailable int     `json:"days_available"`
}

type Result struct {
	Range        RangeInfo `json:"range"`
	Days         *int      `json:"days"`
	ByRepository []Group   `json:"byRepository"`
	ByProxy      []Group   `json:"byProxy"`
}

// --- API publica ------------------------------------------------------------

// Range: rango real de dias con info (sesion mas vieja y mas nueva).
func Range(ctx context.Context, s *vbr.Session) RangeInfo {
	newest := getItems(ctx, s, "v1/sessions?limit=1&orderColumn=CreationTime&orderAsc=false")
	oldest := getItems(ctx, s, "v1/sessions?limit=1&orderColumn=CreationTime&orderAsc=true")
	lo, okLo := parseDT(first(oldest)["creationTime"])
	hi, okHi := parseDT(first(newest)["creationTime"])
	if !okLo || !okHi {
		return RangeInfo{}
	}
	from, to := lo.Format("2006-01-02"), hi.Format("2006-01-02")
	days := int(hi.Truncate(24*time.Hour).Sub(lo.Truncate(24*time.Hour)).Hours()/24) + 1
	return RangeInfo{From: &from, To: &to, DaysAvailable: days}
}

// Build: estadistica agregada por repo y proxy sobre una ventana de dias.
func Build(ctx context.Context, s *vbr.Session, days *int) (Result, error) {
	sess := getItems(ctx, s, "v1/sessions?limit=2000&orderColumn=CreationTime&orderAsc=false")
	rng := Range(ctx, s)

	var dataSess []map[string]any
	for _, x := range sess {
		if isDataJob(x) {
			dataSess = append(dataSess, x)
		}
	}
	if days != nil {
		cutoff := time.Now().AddDate(0, 0, -*days)
		var filtered []map[string]any
		for _, x := range dataSess {
			if t, ok := parseDT(x["creationTime"]); ok && !t.Before(cutoff) {
				filtered = append(filtered, x)
			}
		}
		dataSess = filtered
	}
	if len(dataSess) > maxSessions {
		dataSess = dataSess[:maxSessions]
	}

	repoNames := nameMap(allRepositories(ctx, s))
	proxyNames := nameMap(getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000"))
	jobProxies := jobProxyMap(ctx, s)

	// N+1 en paralelo: cada sesion trae taskSessions + logs.
	records := make([]*Record, len(dataSess))
	sem := make(chan struct{}, fetchWorkers)
	var wg sync.WaitGroup
	for i, x := range dataSess {
		if resultOf(x) == "Failed" { // omitimos los fallidos
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, x map[string]any) {
			defer wg.Done()
			defer func() { <-sem }()
			records[i] = buildRecord(ctx, s, x, jobProxies)
		}(i, x)
	}
	wg.Wait()

	var recs []Record
	for _, r := range records {
		if r != nil {
			recs = append(recs, *r)
		}
	}

	return Result{
		Range:        rng,
		Days:         days,
		ByRepository: aggregate(recs, func(r Record) []string { return r.RepoIDs }, repoNames, "(sin repositorio)"),
		ByProxy:      aggregate(recs, func(r Record) []string { return r.ProxyIDs }, proxyNames, "(sin proxy identificado)"),
	}, nil
}

// --- construccion de cada job ----------------------------------------------

func buildRecord(ctx context.Context, s *vbr.Session, sess map[string]any, jobProxies map[string][]string) *Record {
	sid := str(sess["id"])
	res, _ := sess["result"].(map[string]any)
	stype := str(sess["type"])
	if stype == "" {
		stype = str(sess["sessionType"])
	}
	op := "backup"
	if strings.Contains(strings.ToLower(stype), "restore") {
		op = "restore"
	}

	tasks := buildTasks(getItems(ctx, s, "v1/sessions/"+sid+"/taskSessions"))
	logs := getItems(ctx, s, "v1/sessions/"+sid+"/logs")

	var processed, transferred int64
	repoSet := map[string]bool{}
	for _, t := range tasks {
		processed += t.ProcessedSize
		transferred += t.TransferredSize
		if t.RepositoryID != "" && t.RepositoryID != emptyGUID {
			repoSet[t.RepositoryID] = true
		}
	}

	return &Record{
		ID: sid, Name: str(sess["name"]), Type: stype, Operation: op,
		Result: str(res["result"]), Message: str(res["message"]),
		CreationTime: str(sess["creationTime"]), EndTime: str(sess["endTime"]),
		Bottleneck:      bottleneckFromLogs(logs),
		Tasks:           tasks,
		ProcessedSize:   processed,
		TransferredSize: transferred,
		RepoIDs:         keys(repoSet),
		ProxyIDs:        cleanIDs(jobProxies[str(sess["jobId"])]),
	}
}

func buildTasks(items []map[string]any) []Task {
	out := make([]Task, 0, len(items))
	for _, tk := range items {
		pg, _ := tk["progress"].(map[string]any)
		res, _ := tk["result"].(map[string]any)
		processed := num(pg["processedSize"])
		transferred := num(pg["transferredSize"])
		var reduction *float64
		if transferred >= 1048576 {
			v := round1(float64(processed) / float64(transferred))
			reduction = &v
		}
		out = append(out, Task{
			Name: str(tk["name"]), RepositoryID: str(tk["repositoryId"]),
			Result: str(res["result"]), Bottleneck: str(pg["bottleneck"]),
			ProcessingRate: str(pg["processingRate"]), Duration: str(pg["duration"]),
			ProcessedSize: processed, ReadSize: num(pg["readSize"]),
			TransferredSize: transferred, Reduction: reduction,
		})
	}
	return out
}

// bottleneckFromLogs parsea la(s) linea(s) "Load: Source% > Proxy% > ..." y
// "Primary bottleneck:". Con varias VMs toma el peor caso por componente.
func bottleneckFromLogs(logs []map[string]any) map[string]any {
	var loads [][4]int
	primary := ""
	for _, r := range logs {
		txt := str(r["title"]) + " " + str(r["description"])
		if m := loadRe.FindStringSubmatch(txt); m != nil {
			loads = append(loads, [4]int{atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])})
		}
		if m := primaryRe.FindStringSubmatch(txt); m != nil {
			primary = m[1]
		}
	}
	if len(loads) == 0 {
		if primary != "" {
			return map[string]any{"primary": primary}
		}
		return nil
	}
	comp := [4]int{}
	for _, l := range loads {
		for i := 0; i < 4; i++ {
			if l[i] > comp[i] {
				comp[i] = l[i]
			}
		}
	}
	if primary == "" {
		names := []string{"Source", "Proxy", "Network", "Target"}
		mi := 0
		for i := 1; i < 4; i++ {
			if comp[i] > comp[mi] {
				mi = i
			}
		}
		primary = names[mi]
	}
	return map[string]any{"source": comp[0], "proxy": comp[1], "network": comp[2], "target": comp[3], "primary": primary}
}

// --- agregacion -------------------------------------------------------------

func aggregate(records []Record, idsFn func(Record) []string, names map[string]string, unknown string) []Group {
	groups := map[string][]Record{}
	var order []string
	for _, r := range records {
		ids := idsFn(r)
		if len(ids) == 0 {
			ids = []string{unknown}
		}
		for _, id := range ids {
			if _, seen := groups[id]; !seen {
				order = append(order, id)
			}
			groups[id] = append(groups[id], r)
		}
	}
	out := make([]Group, 0, len(order))
	for _, id := range order {
		name := names[id]
		if name == "" {
			name = id
		}
		if id == unknown {
			name = unknown
		}
		out = append(out, groupStats(id, name, groups[id]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Runs > out[j].Runs })
	return out
}

func groupStats(id, name string, recs []Record) Group {
	counts := map[string]int{"Success": 0, "Warning": 0, "Failed": 0}
	primaryCounts := map[string]int{}
	var processed, transferred int64
	sum := [4]int{}
	n := 0
	for _, r := range recs {
		counts[r.Result]++
		processed += r.ProcessedSize
		transferred += r.TransferredSize
		if r.Bottleneck != nil {
			if p, ok := r.Bottleneck["primary"].(string); ok && p != "" {
				primaryCounts[p]++
			}
			if _, ok := r.Bottleneck["source"]; ok {
				sum[0] += toInt(r.Bottleneck["source"])
				sum[1] += toInt(r.Bottleneck["proxy"])
				sum[2] += toInt(r.Bottleneck["network"])
				sum[3] += toInt(r.Bottleneck["target"])
				n++
			}
		}
	}
	var avg map[string]any
	if n > 0 {
		a := map[string]any{
			"source": sum[0] / n, "proxy": sum[1] / n, "network": sum[2] / n, "target": sum[3] / n,
		}
		names := []string{"source", "proxy", "network", "target"}
		labels := []string{"Source", "Proxy", "Network", "Target"}
		mi := 0
		for i := 1; i < 4; i++ {
			if a[names[i]].(int) > a[names[mi]].(int) {
				mi = i
			}
		}
		a["primary"] = labels[mi]
		avg = a
	}
	return Group{
		ID: id, Name: name, Runs: len(recs), Results: counts,
		ProcessedSize: processed, TransferredSize: transferred,
		BottleneckAvg: avg, PrimaryCounts: primaryCounts, Jobs: recs,
	}
}
