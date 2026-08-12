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
	"time"

	"yogabench/internal/vbr"
)

const (
	maxWindowRuns  = 40           // cap de corridas a agregar en la ventana (N+1 acotado)
	satBytes       = int64(256) << 20 // 256 MiB movidos (max leido/transferido) = corrida "observed" (rate firme)
	lowFloor       = int64(16) << 20  // <16 MiB movidos = no-op -> insufficient (evita rates 0/0 y reduccion absurda)
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
	// Ventana (promedios estilo Veeam ONE).
	Days         int          `json:"days"`
	Runs         int          `json:"runs"`         // corridas analizadas en la ventana
	RunsWithData int          `json:"runsWithData"` // cuantas movieron datos
	PrimaryPct   int          `json:"primaryPct"`   // % de corridas con ese cuello dominante
	RunList      []RunSummary `json:"runList"`      // resumen por corrida (detalle)
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

// JobDeepTarget: nombre del job + OS del VBR (donde viven los Job/Task logs),
// para el modo deep. osKind = "windows" | "linux".
func JobDeepTarget(ctx context.Context, s *vbr.Session, jobID string) (name, osKind string, err error) {
	for _, j := range getItems(ctx, s, "v1/jobs?limit=1000") {
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
// (promedios en los KPIs, estilo Veeam ONE). days<=0 = todo el historico reciente.
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

	out := &JobCapacityResult{JobID: jobID, JobName: str(cand[0]["name"]), Days: days, Notes: []string{}}

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
	out.Recommendations = recommend(out)
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

// recommend: sugerencias atadas al/los stage(s) que topan. Con cores/RAM del host
// (ingresados por el usuario) la recomendacion es FIRME y aplica el gate de
// viabilidad; sin ellos, queda CONDICIONAL (rule of thumb ~1 task/core + 2GB/task).
func recommend(r *JobCapacityResult) []Reco {
	var recs []Reco
	// advice: consejo resource-gated para un stage que "posee" un host (proxy/repo).
	advice := func(kind, scaleOut string) (string, string) {
		var res *ResourceInfo
		for i := range r.Resources {
			if r.Resources[i].Kind == kind {
				res = &r.Resources[i]
				break
			}
		}
		if res == nil {
			return "", "estimate"
		}
		if res.Cores <= 0 { // sin datos de hardware -> condicional
			return fmt.Sprintf("%s has %d task slot(s). Enter its CPU cores/RAM (Resources) for a firm recommendation (~1 task/core + 2GB/task).", res.Name, res.MaxTaskCount), "estimate"
		}
		viable := res.Cores
		if res.RamGB > 0 && res.RamGB/2 < viable {
			viable = res.RamGB / 2
		}
		base := fmt.Sprintf("%s: %d cores / %d GB -> ~%d viable task(s), %d configured. ", res.Name, res.Cores, res.RamGB, viable, res.MaxTaskCount)
		switch {
		case viable <= 2: // gate de viabilidad
			return base + "Low-resource host: raising concurrency won't help — " + scaleOut + ".", "firm"
		case res.MaxTaskCount < viable:
			return base + fmt.Sprintf("Raise task slots to %d (free win, no new hardware).", viable), "firm"
		default:
			return base + "Already at capacity — " + scaleOut + ".", "firm"
		}
	}
	for _, st := range r.Stages {
		if !st.Binding {
			continue
		}
		switch st.Name {
		case "Source":
			recs = append(recs, Reco{st.Name, "warn", "Source (VMware read) is the ceiling: check datastore/storage read speed, CBT health and the transport mode (nbd vs hotadd/SAN). The deep-log mode reports the actual transport and why.", "estimate"})
		case "Proxy":
			txt, conf := advice("proxy", "add CPU/RAM or another proxy")
			recs = append(recs, Reco{st.Name, "high", "Proxy processing is the ceiling. " + txt, conf})
		case "Network":
			recs = append(recs, Reco{st.Name, "high", "The proxy<->repository network is the ceiling. Validate the real link with iperf (Benchmark > Network); consider co-locating proxy and repository or a faster link.", "estimate"})
		case "Target":
			txt, conf := advice("repository", "use a faster repo / add SOBR extents")
			recs = append(recs, Reco{st.Name, "high", "Repository write is the ceiling. " + txt + " Measure the disk with fio (Benchmark > Disk).", conf})
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
