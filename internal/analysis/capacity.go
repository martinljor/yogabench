package analysis

// capacity.go — Capacity & Headroom model (v0.4). Per-job drill-down:
// derives the pipeline ceiling, headroom and TIME projection from one real
// data-moving run, plus resource-gated recommendations. Read-only (REST).
// See docs/capacity-model-spec.md.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"yogabench/internal/vbr"
)

const (
	maxWindowRuns = 40               // cap de corridas a agregar en la ventana (N+1 acotado)
	satBytes      = int64(256) << 20 // 256 MiB movidos (max leido/transferido) = corrida "observed" (rate firme)
	lowFloor      = int64(16) << 20  // <16 MiB movidos = no-op -> insufficient (evita rates 0/0 y reduccion absurda)
	bindThreshold = 85               // util% a partir del cual un stage se considera "topando"
	coLimitSpread = 6                // stages dentro de este spread del maximo tambien co-limitan
)

var stageNames = []string{"Source", "Proxy", "Network", "Target"}

// JobsPath: one single query for the job list, so every caller shares the same
// cache entry (see cacheable in internal/vbr). On a loaded VBR this endpoint can
// take 12 s or time out, so it must not be asked for twice with two limits.
const JobsPath = "v1/jobs?limit=1000"

const jobsPath = JobsPath

// --- tipos de salida --------------------------------------------------------

type JobItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled"`
	// Source: "config" = listed by v1/jobs · "sessions" = only seen in the session
	// history. v1/jobs does not return every job type (plugins, NAS, object
	// storage, some agents), and on a loaded VBR it can time out — in both cases
	// the session history still knows the job, and it is analyzable from there.
	Source string `json:"source"`
}

type StageInfo struct {
	Name    string `json:"name"`
	Util    int    `json:"util"`
	Binding bool   `json:"binding"`
}

type ResourceInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"` // proxy | repository
	HostID       string   `json:"hostId"`
	MaxTaskCount int      `json:"maxTaskCount"`
	Cores        int      `json:"cores,omitempty"`      // manual (0 = desconocido)
	RamGB        int      `json:"ramGB,omitempty"`      // manual
	CapacityGB   *float64 `json:"capacityGB,omitempty"` // repos (de /repositories/states)
	FreeGB       *float64 `json:"freeGB,omitempty"`
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
	Verdict         *Verdict       `json:"verdict"` // la conclusion accionable (ver verdict.go)
	Notes           []string       `json:"notes"`
	RepDurationSec  float64        `json:"repDurationSec"` // duracion de la corrida representativa
	// Ventana (promedios sobre las corridas del periodo).
	Days         int          `json:"days"`
	Runs         int          `json:"runs"`         // corridas analizadas en la ventana
	RunsWithData int          `json:"runsWithData"` // cuantas movieron datos
	FullRuns     int          `json:"fullRuns"`     // mezcla de la ventana: se analizan TODAS
	IncrRuns     int          `json:"incrRuns"`
	PrimaryPct   int          `json:"primaryPct"` // % de corridas con ese cuello dominante
	RunList      []RunSummary `json:"runList"`    // resumen por corrida (detalle)
}

// RunSummary: una corrida del job dentro de la ventana (para el detalle).
type RunSummary struct {
	SessionID       string  `json:"sessionId"`
	CreationTime    string  `json:"creationTime"`
	Result          string  `json:"result"`
	DurationSec     float64 `json:"durationSec"`
	ReadMBps        float64 `json:"readMBps"`
	WriteMBps       float64 `json:"writeMBps"`
	Primary         string  `json:"primary"`
	TransferredSize int64   `json:"transferredSize"`
	Algorithm       string  `json:"algorithm"` // Full | Increment (de las tareas)
}

// runAlgorithm: the run is a Full if any of its tasks ran as Full.
func runAlgorithm(tasks []Task) string {
	out := ""
	for _, t := range tasks {
		if strings.EqualFold(t.Algorithm, "Full") {
			return "Full"
		}
		if t.Algorithm != "" {
			out = t.Algorithm
		}
	}
	return out
}

// --- API publica ------------------------------------------------------------

// JobList: jobs for the drill-down selector (read-only). It is the UNION of the
// configured jobs and the jobs seen in the session history, because neither
// source alone is complete: v1/jobs omits several job types (plugins, NAS, object
// storage) and can time out on a loaded VBR, while the history only knows jobs
// that actually ran.
func JobList(ctx context.Context, s *vbr.Session) []JobItem {
	out := make([]JobItem, 0, 32)
	seen := map[string]bool{}
	for _, j := range getItems(ctx, s, jobsPath) {
		id := str(j["id"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, JobItem{
			ID: id, Name: str(j["name"]), Type: str(j["type"]),
			Disabled: boolOf(j["isDisabled"]), Source: "config",
		})
	}
	for _, x := range getItems(ctx, s, "v1/sessions?limit=2000&orderColumn=CreationTime&orderAsc=false") {
		id := str(x["jobId"])
		if id == "" || seen[id] || !isDataJob(x) {
			continue
		}
		seen[id] = true
		out = append(out, JobItem{
			ID: id, Name: JobNameOf(str(x["name"])), Type: strOr(x["sessionType"], str(x["type"])),
			Source: "sessions",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// runSuffix: Veeam names a session "<job> (Incremental)" / "(Full)" / "(Retry)".
// The suffix describes the RUN, not the job, so showing it as the job name makes
// the analysis look like it only covers that one run.
var runSuffix = regexp.MustCompile(`(?i)\s*\((full|incremental|increment|synthetic full|active full|retry(\s+\d+)?)\)\s*$`)

// JobNameOf turns a session name into the job name.
func JobNameOf(sessionName string) string {
	return strings.TrimSpace(runSuffix.ReplaceAllString(sessionName, ""))
}

// JobDeepTarget: nombre del job + OS del VBR (donde viven los Job/Task logs),
// para el modo deep. osKind = "windows" | "linux".
func JobDeepTarget(ctx context.Context, s *vbr.Session, jobID string) (name, osKind string, err error) {
	for _, j := range getItems(ctx, s, jobsPath) {
		if str(j["id"]) == jobID {
			name = str(j["name"])
			break
		}
	}
	if name == "" {
		return "", "", fmt.Errorf("job not found")
	}
	osKind = "windows"
	for _, m := range getItems(ctx, s, "v1/backupInfrastructure/managedServers?limit=1000") {
		if boolOf(m["isBackupServer"]) {
			if boolOf(m["isVBRLinuxAppliance"]) {
				osKind = "linux"
			}
			break
		}
	}
	return name, osKind, nil
}

// JobCapacity: modelo de capacidad de un job AGREGADO sobre una ventana de N dias
// (promedios en los KPIs). days<=0 = todo el historico reciente.
// El veredicto sale sin deep ni medicion; quien las tenga (el server) rehace el
// veredicto con BuildVerdict — es computo puro, no cuesta REST.
func JobCapacity(ctx context.Context, s *vbr.Session, jobID string, days int) (*JobCapacityResult, error) {
	all := getItems(ctx, s, "v1/sessions?limit=500&orderColumn=CreationTime&orderAsc=false")
	var cutoff time.Time
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}
	var cand []map[string]any
	for _, x := range all {
		if str(x["jobId"]) != jobID || !isDataJob(x) || resultOf(x) == "Failed" {
			continue
		}
		if days > 0 {
			if t, ok := parseDT(x["creationTime"]); ok && t.Before(cutoff) {
				continue
			}
		}
		cand = append(cand, x)
	}
	if len(cand) == 0 {
		return nil, fmt.Errorf("no successful runs for this job in the last %d day(s)", days)
	}
	if len(cand) > maxWindowRuns {
		cand = cand[:maxWindowRuns]
	}

	out := &JobCapacityResult{JobID: jobID, JobName: JobNameOf(str(cand[0]["name"])), Days: days, Notes: []string{}}

	var totProc, totRead, totXfer int64 // sobre corridas con datos
	var sumDur, sumDataDur float64      // avg duracion (todas) / rate (con datos)
	primaryCount := map[string]int{}
	stageSum := [4]int{}
	stageN := 0 // corridas con linea Load: (para promediar %)
	var repTasks []Task
	var repSess map[string]any
	repData := int64(-1)

	for _, x := range cand {
		sid := str(x["id"])
		tasks := buildTasks(getItems(ctx, s, "v1/sessions/"+sid+"/taskSessions"))
		logs := getItems(ctx, s, "v1/sessions/"+sid+"/logs")
		var proc, read, xfer int64
		for _, tk := range tasks {
			proc += tk.ProcessedSize
			read += tk.ReadSize
			xfer += tk.TransferredSize
		}
		durSec := sessionDur(x)
		sumDur += durSec
		maxData := xfer
		if read > maxData {
			maxData = read
		}

		bn := bottleneckFromLogs(logs)
		if bn == nil {
			if p := dominantTaskBottleneck(tasks); p != "" {
				bn = map[string]any{"primary": p}
			}
		}
		if prim, _ := bn["primary"].(string); prim != "" {
			primaryCount[prim]++
		}
		if _, ok := bn["source"]; ok {
			stageSum[0] += toInt(bn["source"])
			stageSum[1] += toInt(bn["proxy"])
			stageSum[2] += toInt(bn["network"])
			stageSum[3] += toInt(bn["target"])
			stageN++
		}
		if maxData >= lowFloor {
			out.RunsWithData++
			totProc += proc
			totRead += read
			totXfer += xfer
			sumDataDur += durSec
		}
		if maxData > repData { // representativa = la que mas datos movio (per-VM + deep)
			repData, repTasks, repSess = maxData, tasks, x
		}
		rs := RunSummary{SessionID: sid, CreationTime: str(x["creationTime"]), Result: resultOf(x), DurationSec: round1(durSec), TransferredSize: xfer}
		rs.Algorithm = runAlgorithm(tasks)
		switch rs.Algorithm {
		case "Full":
			out.FullRuns++
		case "Increment":
			out.IncrRuns++
		}
		rs.Primary, _ = bn["primary"].(string)
		if durSec > 0 {
			rs.ReadMBps = round1(float64(read) / durSec / 1e6)
			rs.WriteMBps = round1(float64(xfer) / durSec / 1e6)
		}
		out.RunList = append(out.RunList, rs)
	}
	out.Runs = len(cand)

	// KPIs: duracion promedio (todas las corridas); throughput/reduccion agregados
	// (sobre las que movieron datos).
	out.DurationSec = round1(sumDur / float64(out.Runs))
	out.ProcessedSize, out.ReadSize, out.TransferredSize = totProc, totRead, totXfer
	if sumDataDur > 0 {
		out.ReadMBps = round1(float64(totRead) / sumDataDur / 1e6)
		out.WriteMBps = round1(float64(totXfer) / sumDataDur / 1e6)
	}
	if totXfer >= satBytes {
		v := round1(float64(totProc) / float64(totXfer))
		out.Reduction = &v
	}
	aggMax := totXfer
	if totRead > aggMax {
		aggMax = totRead
	}
	out.Saturated = aggMax >= satBytes
	switch {
	case out.Saturated:
		out.Confidence = "observed"
	case out.RunsWithData > 0:
		out.Confidence = "low"
		out.Notes = append(out.Notes, "Runs moved little data: rates are indicative. Keep Active Fulls in the window for firm numbers.")
	default:
		out.Confidence = "insufficient"
		out.Notes = append(out.Notes, "No run in the window moved meaningful data (no-op incrementals): the bottleneck is relative only.")
	}

	// Stages promedio + cuello dominante (% de corridas).
	bneck := map[string]any{}
	if stageN > 0 {
		bneck = map[string]any{"source": stageSum[0] / stageN, "proxy": stageSum[1] / stageN, "network": stageSum[2] / stageN, "target": stageSum[3] / stageN}
	}
	top, topN := "", 0
	for k, n := range primaryCount {
		if n > topN {
			top, topN = k, n
		}
	}
	if top != "" {
		bneck["primary"] = top
		out.PrimaryPct = int(float64(topN)/float64(out.Runs)*100 + 0.5)
	}
	out.Primary, _ = bneck["primary"].(string)
	out.Stages = buildStages(bneck)
	if stageN == 0 && top != "" {
		out.Notes = append(out.Notes, "No 'Load:' line in REST logs; showing the dominant per-task stage (no per-stage %). Use deep mode for per-VM stages.")
	}

	// Representativa: per-VM + datos de la sesion (para abrir el deep).
	if repSess != nil {
		out.SessionID, out.SessionName = str(repSess["id"]), str(repSess["name"])
		out.Result, out.CreationTime, out.EndTime = resultOf(repSess), str(repSess["creationTime"]), str(repSess["endTime"])
		out.RepDurationSec = round1(sessionDur(repSess))
		out.Tasks = repTasks
		repoSet := map[string]bool{}
		for _, t := range repTasks {
			if t.RepositoryID != "" && t.RepositoryID != emptyGUID {
				repoSet[t.RepositoryID] = true
			}
		}
		out.Resources = jobResources(ctx, s, jobID, keys(repoSet))
	}

	out.Projection = projectTime(out.Stages, out.DurationSec, out.RunsWithData > 0)
	out.Verdict = BuildVerdict(out, nil, nil) // sin deep ni medicion (ver arriba)
	return out, nil
}

// sessionDur: duracion (creationTime -> endTime) en segundos.
func sessionDur(x map[string]any) float64 {
	if a, okA := parseDT(x["creationTime"]); okA {
		if b, okB := parseDT(x["endTime"]); okB && b.After(a) {
			return b.Sub(a).Seconds()
		}
	}
	return 0
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
func projectTime(stages []StageInfo, durSec float64, hasData bool) *Projection {
	if !hasData || durSec <= 0 {
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

// jobResources: proxies del job + repos usados, con maxTaskCount (REST), cores/RAM
// manuales (de la sesion) y capacidad/free del repo (de /repositories/states).
func jobResources(ctx context.Context, s *vbr.Session, jobID string, repoIDs []string) []ResourceInfo {
	hostRes := s.HostResAll()
	var out []ResourceInfo

	// Proxies del job.
	proxyMax, proxyName, proxyHost := map[string]int{}, map[string]string{}, map[string]string{}
	for _, p := range getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000") {
		id := str(p["id"])
		proxyName[id] = str(p["name"])
		if srv, ok := p["server"].(map[string]any); ok {
			proxyMax[id] = toInt(srv["maxTaskCount"])
			proxyHost[id] = str(srv["hostId"])
		}
	}
	for _, pid := range cleanIDs(jobProxyMap(ctx, s)[jobID]) {
		r := ResourceInfo{ID: pid, Name: proxyName[pid], Kind: "proxy", HostID: proxyHost[pid], MaxTaskCount: proxyMax[pid]}
		if hr, ok := hostRes[proxyHost[pid]]; ok {
			r.Cores, r.RamGB = hr.Cores, hr.RamGB
		}
		out = append(out, r)
	}

	// Repos usados por las tareas.
	repoMax, repoName, repoHost := map[string]int{}, map[string]string{}, map[string]string{}
	for _, rp := range allRepositories(ctx, s) {
		id := str(rp["id"])
		repoName[id] = str(rp["name"])
		repoHost[id] = str(rp["hostId"])
		if inner, ok := rp["repository"].(map[string]any); ok {
			repoMax[id] = toInt(inner["maxTaskCount"])
		}
	}
	// Capacidad/free por repo (endpoint states; nombres de campo defensivos).
	capGB, freeGB := map[string]float64{}, map[string]float64{}
	for _, stt := range getItems(ctx, s, "v1/backupInfrastructure/repositories/states") {
		id := firstNonEmpty(str(stt["id"]), str(stt["repositoryId"]))
		if id == "" {
			continue
		}
		capGB[id] = gbOf(stt, "capacityGB", "capacity")
		freeGB[id] = gbOf(stt, "freeGB", "freeSpace", "free")
	}
	for _, rid := range repoIDs {
		r := ResourceInfo{ID: rid, Name: repoName[rid], Kind: "repository", HostID: repoHost[rid], MaxTaskCount: repoMax[rid]}
		if hr, ok := hostRes[repoHost[rid]]; ok {
			r.Cores, r.RamGB = hr.Cores, hr.RamGB
		}
		if v := capGB[rid]; v > 0 {
			r.CapacityGB = &v
		}
		if v := freeGB[rid]; v > 0 {
			r.FreeGB = &v
		}
		out = append(out, r)
	}
	return out
}

// gbOf lee el primer campo presente y lo normaliza a GB: los "*GB" se toman como
// GB; si el campo trae bytes crudos (valor grande), se convierte a GB.
func gbOf(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		f := toFloat(v)
		if f <= 0 {
			continue
		}
		if strings.Contains(k, "GB") {
			return round1(f)
		}
		if f > 1e9 { // bytes -> GB
			return round1(f / 1e9)
		}
		return round1(f)
	}
	return 0
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
