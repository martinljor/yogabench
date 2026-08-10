package analysis

// capacity.go — Capacity & Headroom model (v0.4). Per-job drill-down:
// derives the pipeline ceiling, headroom and TIME projection from one real
// data-moving run, plus resource-gated recommendations. Read-only (REST).
// See docs/capacity-model-spec.md.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"yogabench/internal/vbr"
)

const (
	scanSessions   = 8          // cuantas sesiones recientes del job miramos para hallar una con datos
	satBytes       = int64(1) << 30 // ~1 GiB transferido = corrida "con datos" (piso para dar rate absoluto)
	bindThreshold  = 85         // util% a partir del cual un stage se considera "topando"
	coLimitSpread  = 6          // stages dentro de este spread del maximo tambien co-limitan
)

var stageNames = []string{"Source", "Proxy", "Network", "Target"}

// --- tipos de salida --------------------------------------------------------

type JobItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled"`
}

type StageInfo struct {
	Name    string `json:"name"`
	Util    int    `json:"util"`
	Binding bool   `json:"binding"`
}

type ResourceInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"` // proxy | repository
	MaxTaskCount int    `json:"maxTaskCount"`
}

type Reco struct {
	Stage      string `json:"stage"`
	Severity   string `json:"severity"` // info | warn | high
	Text       string `json:"text"`
	Confidence string `json:"confidence"` // estimate | observed
}

type Projection struct {
	NextStage      string  `json:"nextStage"`
	NewDurationSec float64 `json:"newDurationSec"`
	ImprovementPct int     `json:"improvementPct"`
}

type JobCapacityResult struct {
	JobID           string         `json:"jobId"`
	JobName         string         `json:"jobName"`
	SessionID       string         `json:"sessionId"`
	SessionName     string         `json:"sessionName"`
	Result          string         `json:"result"`
	CreationTime    string         `json:"creationTime"`
	EndTime         string         `json:"endTime"`
	DurationSec     float64        `json:"durationSec"`
	Stages          []StageInfo    `json:"stages"`
	Primary         string         `json:"primary"`
	ProcessedSize   int64          `json:"processedSize"`
	ReadSize        int64          `json:"readSize"`
	TransferredSize int64          `json:"transferredSize"`
	Reduction       *float64       `json:"reduction"`
	ReadMBps        float64        `json:"readMBps"`
	WriteMBps       float64        `json:"writeMBps"`
	Saturated       bool           `json:"saturated"`  // movio datos suficientes para dar MB/s absoluto
	Confidence      string         `json:"confidence"` // observed | insufficient
	Tasks           []Task         `json:"tasks"`
	Resources       []ResourceInfo `json:"resources"`
	Projection      *Projection    `json:"projection"`
	Recommendations []Reco         `json:"recommendations"`
	Notes           []string       `json:"notes"`
}

// --- API publica ------------------------------------------------------------

// JobList: lista de jobs para el listbox del drill-down (solo lectura).
func JobList(ctx context.Context, s *vbr.Session) []JobItem {
	items := getItems(ctx, s, "v1/jobs?limit=1000")
	out := make([]JobItem, 0, len(items))
	for _, j := range items {
		out = append(out, JobItem{
			ID: str(j["id"]), Name: str(j["name"]), Type: str(j["type"]),
			Disabled: boolOf(j["isDisabled"]),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// JobCapacity: modelo de capacidad para un job, sobre su ultima corrida con datos.
func JobCapacity(ctx context.Context, s *vbr.Session, jobID string) (*JobCapacityResult, error) {
	// Sesiones recientes del job (data jobs, no Failed).
	all := getItems(ctx, s, "v1/sessions?limit=200&orderColumn=CreationTime&orderAsc=false")
	var cand []map[string]any
	for _, x := range all {
		if str(x["jobId"]) != jobID || !isDataJob(x) || resultOf(x) == "Failed" {
			continue
		}
		cand = append(cand, x)
	}
	if len(cand) == 0 {
		return nil, fmt.Errorf("no successful data sessions found for this job")
	}

	// Buscar la corrida mas reciente que movio datos (>= satBytes). Si ninguna,
	// usar la mas reciente (veredicto relativo, sin MB/s absoluto).
	var chosen map[string]any
	var chosenTasks []Task
	var chosenXfer int64
	scanned := 0
	for _, x := range cand {
		if scanned >= scanSessions {
			break
		}
		scanned++
		tasks := buildTasks(getItems(ctx, s, "v1/sessions/"+str(x["id"])+"/taskSessions"))
		var xfer int64
		for _, t := range tasks {
			xfer += t.TransferredSize
		}
		if chosen == nil { // fallback: la mas reciente
			chosen, chosenTasks, chosenXfer = x, tasks, xfer
		}
		if xfer >= satBytes {
			chosen, chosenTasks, chosenXfer = x, tasks, xfer
			break
		}
	}

	sid := str(chosen["id"])
	logs := getItems(ctx, s, "v1/sessions/"+sid+"/logs")
	res, _ := chosen["result"].(map[string]any)

	var processed, read int64
	repoSet := map[string]bool{}
	for _, t := range chosenTasks {
		processed += t.ProcessedSize
		read += t.ReadSize
		if t.RepositoryID != "" && t.RepositoryID != emptyGUID {
			repoSet[t.RepositoryID] = true
		}
	}
	transferred := chosenXfer

	durSec := 0.0
	if a, okA := parseDT(chosen["creationTime"]); okA {
		if b, okB := parseDT(chosen["endTime"]); okB && b.After(a) {
			durSec = b.Sub(a).Seconds()
		}
	}

	out := &JobCapacityResult{
		JobID: jobID, JobName: str(chosen["name"]), SessionID: sid, SessionName: str(chosen["name"]),
		Result: str(res["result"]), CreationTime: str(chosen["creationTime"]), EndTime: str(chosen["endTime"]),
		DurationSec: durSec, ProcessedSize: processed, ReadSize: read, TransferredSize: transferred,
		Tasks: chosenTasks, Notes: []string{},
	}
	if transferred > 0 {
		v := round1(float64(processed) / float64(transferred))
		out.Reduction = &v
	}
	if durSec > 0 {
		out.ReadMBps = round1(float64(read) / durSec / 1e6)
		out.WriteMBps = round1(float64(transferred) / durSec / 1e6)
	}
	out.Saturated = transferred >= satBytes
	if out.Saturated {
		out.Confidence = "observed"
	} else {
		out.Confidence = "insufficient"
		out.Notes = append(out.Notes, "This run moved little/no data (incremental no-op): the bottleneck stage is valid (relative), but absolute MB/s and time projection are not meaningful. Run an Active Full for a firm number.")
	}

	// Stages desde la linea Load: (nivel job). Si no hay, usar el bottleneck por tarea.
	bneck := bottleneckFromLogs(logs)
	if bneck == nil {
		if p := dominantTaskBottleneck(chosenTasks); p != "" {
			bneck = map[string]any{"primary": p}
			out.Notes = append(out.Notes, "No 'Load:' line in logs; using the per-task dominant stage (no per-stage %).")
		}
	}
	out.Primary, _ = bneck["primary"].(string)
	out.Stages = buildStages(bneck)

	// Recursos (REST): proxies del job + repos usados, con su maxTaskCount provisto.
	out.Resources = jobResources(ctx, s, jobID, keys(repoSet))

	// Proyeccion de tiempo + recomendaciones (solo con datos utiles).
	out.Projection = projectTime(out.Stages, durSec, out.Saturated)
	out.Recommendations = recommend(out)

	return out, nil
}

// --- helpers del modelo -----------------------------------------------------

// buildStages arma los 4 stages con util% y marca los binding (topando).
func buildStages(bneck map[string]any) []StageInfo {
	util := map[string]int{}
	hasPct := false
	if bneck != nil {
		for _, k := range []string{"source", "proxy", "network", "target"} {
			if v, ok := bneck[k]; ok {
				util[strings.Title(k)] = toInt(v)
				hasPct = true
			}
		}
	}
	primary, _ := bneck["primary"].(string)
	// Si no hay %, marcamos solo el primary como binding (100 nominal).
	if !hasPct {
		out := make([]StageInfo, 0, 4)
		for _, n := range stageNames {
			out = append(out, StageInfo{Name: n, Util: 0, Binding: n == primary})
		}
		return out
	}
	max := 0
	for _, n := range stageNames {
		if util[n] > max {
			max = util[n]
		}
	}
	out := make([]StageInfo, 0, 4)
	for _, n := range stageNames {
		u := util[n]
		out = append(out, StageInfo{Name: n, Util: u, Binding: u >= bindThreshold && u >= max-coLimitSpread})
	}
	return out
}

// projectTime: T_new ≈ T_now × (U_next / U_binding). U_next = mayor util entre
// los NO binding. Solo si la corrida movio datos y hay margen real.
func projectTime(stages []StageInfo, durSec float64, saturated bool) *Projection {
	if !saturated || durSec <= 0 {
		return nil
	}
	bindUtil, nextUtil, nextName := 0, 0, ""
	for _, s := range stages {
		if s.Binding && s.Util > bindUtil {
			bindUtil = s.Util
		}
	}
	for _, s := range stages {
		if !s.Binding && s.Util > nextUtil {
			nextUtil, nextName = s.Util, s.Name
		}
	}
	if bindUtil == 0 || nextUtil == 0 || nextUtil >= bindUtil {
		return nil
	}
	ratio := float64(nextUtil) / float64(bindUtil)
	return &Projection{
		NextStage:      nextName,
		NewDurationSec: round1(durSec * ratio),
		ImprovementPct: int((1 - ratio) * 100),
	}
}

// recommend: sugerencias atadas al/los stage(s) que topan, resource-gated. En
// prod (solo maxTaskCount) las recomendaciones de recursos son CONDICIONALES.
func recommend(r *JobCapacityResult) []Reco {
	var recs []Reco
	slots := func(kind string) string {
		for _, res := range r.Resources {
			if res.Kind == kind {
				return fmt.Sprintf("%s has %d task slot(s)", res.Name, res.MaxTaskCount)
			}
		}
		return ""
	}
	for _, st := range r.Stages {
		if !st.Binding {
			continue
		}
		switch st.Name {
		case "Source":
			recs = append(recs, Reco{st.Name, "warn", "Source (VMware read) is the ceiling: check the datastore/storage read speed, CBT health and snapshot handling. Adding proxy concurrency only helps if the source can serve more.", "estimate"})
		case "Proxy":
			recs = append(recs, Reco{st.Name, "high", "Proxy processing is the ceiling. " + slots("proxy") + ". If the proxy host has more CPU cores/RAM than task slots (~1 task/core + 2GB), raise the task slots (free win); otherwise add CPU/RAM or another proxy.", "estimate"})
		case "Network":
			recs = append(recs, Reco{st.Name, "high", "The proxy↔repository network is the ceiling. Validate the real link with the iperf test (Benchmark › Network); consider co-locating proxy and repository or a faster link.", "estimate"})
		case "Target":
			recs = append(recs, Reco{st.Name, "high", "Repository write is the ceiling. " + slots("repository") + ". Raise repo task slots if the host has spare cores; otherwise use a faster repo / add SOBR extents. Measure the disk with fio (Benchmark › Disk).", "estimate"})
		}
	}
	if r.Projection != nil {
		recs = append(recs, Reco{
			Stage: r.Projection.NextStage, Severity: "info",
			Text:       fmt.Sprintf("If the bottleneck is relieved, the next limit is %s — estimated ~%d%% faster (validate on the next run).", r.Projection.NextStage, r.Projection.ImprovementPct),
			Confidence: "estimate",
		})
	}
	if len(recs) == 0 && r.Saturated {
		recs = append(recs, Reco{Stage: "", Severity: "info", Text: "No single stage is saturated — the pipeline looks balanced for this run.", Confidence: "observed"})
	}
	return recs
}

// jobResources: proxies del job + repos usados, con su maxTaskCount (REST).
func jobResources(ctx context.Context, s *vbr.Session, jobID string, repoIDs []string) []ResourceInfo {
	var out []ResourceInfo
	// Proxies del job.
	proxyMax := map[string]int{}
	proxyName := map[string]string{}
	for _, p := range getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000") {
		id := str(p["id"])
		proxyName[id] = str(p["name"])
		if srv, ok := p["server"].(map[string]any); ok {
			proxyMax[id] = toInt(srv["maxTaskCount"])
		}
	}
	for _, pid := range cleanIDs(jobProxyMap(ctx, s)[jobID]) {
		out = append(out, ResourceInfo{ID: pid, Name: proxyName[pid], Kind: "proxy", MaxTaskCount: proxyMax[pid]})
	}
	// Repos usados por las tareas.
	repoMax := map[string]int{}
	repoName := map[string]string{}
	for _, rp := range allRepositories(ctx, s) {
		id := str(rp["id"])
		repoName[id] = str(rp["name"])
		if inner, ok := rp["repository"].(map[string]any); ok {
			repoMax[id] = toInt(inner["maxTaskCount"])
		}
	}
	for _, rid := range repoIDs {
		out = append(out, ResourceInfo{ID: rid, Name: repoName[rid], Kind: "repository", MaxTaskCount: repoMax[rid]})
	}
	return out
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}
