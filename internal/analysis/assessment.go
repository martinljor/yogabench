package analysis

// assessment.go — VEREDICTO DEL ENTORNO (v0.6). El veredicto por job dice que
// arreglar en UN job; esto dice que arreglar en la INFRA: cuanto sostiene, donde
// esta el cuello recurrente, quien lo causa y si la ventana de backup se pisa.
//
// Todo se pondera por DATO MOVIDO y no por cantidad de corridas: 20 incrementales
// no-op no pueden pesar lo mismo que un full de 2 TB. El cuello que reportamos es
// el que afecta a los bytes reales.
//
// Se calcula desde los Records que ya trajo el modo Global: CERO llamadas REST
// extra (ver Build en analysis.go).

import (
	"fmt"
	"sort"
	"time"
)

const (
	hotspotShare  = 35 // % del dato binding para nombrar a un recurso como hotspot
	staggerJobs   = 3  // jobs distintos arrancando en la misma hora = pico evitable
	envStageShare = 40 // % del dato para declarar un cuello de entorno
)

// Hotspot: recurso (repo/proxy) que aparece como cuello, ponderado por dato.
type Hotspot struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"` // repository | proxy
	Runs      int     `json:"runs"`
	Bytes     int64   `json:"bytes"`     // dato movido en corridas donde ese stage topaba
	SharePct  int     `json:"sharePct"`  // % del dato binding total
	Stage     string  `json:"stage"`     // stage que topa asociado al recurso
	ThrouMBps float64 `json:"throuMBps"` // rate observado del recurso
}

// HourLoad: load per hour of the day (backup window shape and overlap).
type HourLoad struct {
	Hour       int   `json:"hour"`
	Bytes      int64 `json:"bytes"` // per-day average over the window
	Runs       int   `json:"runs"`
	JobStarts  int   `json:"jobStarts"`  // distinct jobs STARTING in that hour
	JobsActive int   `json:"jobsActive"` // distinct jobs RUNNING during that hour
}

// bucket: one absolute clock hour of activity. Bytes are attributed to the hours
// a run spans; spans keep the busy intervals so the rate can be computed over the
// time the infrastructure was actually working, not over the whole clock hour.
type bucket struct {
	bytes int64
	spans []span
	jobs  map[string]bool
}

// Assessment: la foto del entorno.
type Assessment struct {
	Days int `json:"days"`
	Runs int `json:"runs"`
	Jobs int `json:"jobs"`

	// Observed capacity. PeakMBps is the rate DURING THE BUSY TIME of the peak
	// hour (bytes / union of the run intervals in that hour), not bytes/3600: a
	// job that moves 16 GB in 5 minutes sustained ~53 MB/s, and dividing by the
	// whole clock hour would report ~3 MB/s.
	PeakBytesPerHour int64   `json:"peakBytesPerHour"`
	PeakBusySec      float64 `json:"peakBusySec"`
	PeakMBps         float64 `json:"peakMBps"`
	PeakAt           string  `json:"peakAt"`
	TotalBytes       int64   `json:"totalBytes"`

	// Cuello del entorno, ponderado por dato movido.
	StageBytes  map[string]int64 `json:"stageBytes"`
	TopStage    string           `json:"topStage"`
	TopStagePct int              `json:"topStagePct"`
	DataRuns    int              `json:"dataRuns"` // corridas que movieron datos
	NoDataJobs  int              `json:"noDataJobs"`

	Hotspots []Hotspot  `json:"hotspots"`
	Hours    []HourLoad `json:"hours"`

	// Backup window. Busiest* is where the load actually is (jobs RUNNING);
	// Stagger* is where most jobs START, which is what can be moved apart.
	BusiestHour    int            `json:"busiestHour"`
	BusiestJobs    int            `json:"busiestJobs"`
	BusiestPct     int            `json:"busiestPct"`
	StaggerHour    int            `json:"staggerHour"`
	StaggerJobs    int            `json:"staggerJobs"`
	StaggerPct     int            `json:"staggerPct"`
	Actions        []Action       `json:"actions"`
	Severity       string         `json:"severity"` // critical | warn | ok | unknown
	HeadlineCode   string         `json:"headlineCode"`
	Headline       string         `json:"headline"`
	HeadlineParams map[string]any `json:"headlineParams,omitempty"`
}

// BuildAssessment arma el veredicto del entorno desde las corridas de la ventana.
// repoNames/proxyNames vienen del mismo mapa que usa el agregado por repo/proxy.
func BuildAssessment(recs []Record, days int, repoNames, proxyNames map[string]string) *Assessment {
	if len(recs) == 0 {
		return nil
	}
	a := &Assessment{Days: days, StageBytes: map[string]int64{}}
	if a.Days <= 0 {
		a.Days = 1
	}

	buckets := map[time.Time]*bucket{} // activity per absolute clock hour
	hourRuns := map[int]int{}          // run starts per hour of the day
	hourJobs := map[int]map[string]bool{}
	startBytes := map[int]int64{} // bytes of the runs STARTING in each hour
	jobs := map[string]bool{}
	jobData := map[string]int64{}
	resBytes := map[string]int64{} // "kind|id" -> dato binding
	resRuns := map[string]int{}
	resStage := map[string]string{}
	resSpans := map[string][]span{} // para el rate: tiempo de PARED, no suma de duraciones
	var bindingTotal int64

	for _, r := range recs {
		jobID := firstNonEmpty(r.JobID, r.Name)
		jobs[jobID] = true
		a.Runs++
		a.TotalBytes += r.TransferredSize
		jobData[jobID] += r.TransferredSize

		start, okS := parseDT(r.CreationTime)
		end, okE := parseDT(r.EndTime)
		if okS {
			h := start.Hour()
			hourRuns[h]++
			startBytes[h] += r.TransferredSize
			if hourJobs[h] == nil {
				hourJobs[h] = map[string]bool{}
			}
			hourJobs[h][jobID] = true
		}
		// Attribute the bytes to the hours the run spans (a run from 02:00 to 06:00
		// did not move everything at 02:00), and keep its busy interval per hour.
		if okS && okE && end.After(start) && r.TransferredSize > 0 {
			spread(buckets, start, end, r.TransferredSize, jobID)
		} else if okS {
			bk := getBucket(buckets, start.Truncate(time.Hour))
			bk.bytes += r.TransferredSize
			bk.jobs[jobID] = true
		}

		// Cuello ponderado por dato: solo cuentan las corridas que movieron algo.
		if r.TransferredSize < lowFloor {
			continue
		}
		a.DataRuns++
		prim := ""
		if r.Bottleneck != nil {
			prim, _ = r.Bottleneck["primary"].(string)
		}
		if prim == "" {
			continue
		}
		a.StageBytes[prim] += r.TransferredSize
		bindingTotal += r.TransferredSize
		// El recurso "dueño" del stage: Target -> repo, Proxy/Network -> proxy.
		var ids []string
		var kind string
		switch prim {
		case "Target":
			ids, kind = r.RepoIDs, "repository"
		case "Proxy", "Network":
			ids, kind = r.ProxyIDs, "proxy"
		}
		for _, id := range ids {
			k := kind + "|" + id
			resBytes[k] += r.TransferredSize
			resRuns[k]++
			resStage[k] = prim
			if okS && okE && end.After(start) {
				resSpans[k] = append(resSpans[k], span{start, end})
			}
		}
	}
	a.Jobs = len(jobs)
	for _, b := range jobData {
		if b < lowFloor {
			a.NoDataJobs++
		}
	}

	// Peak hour and load per hour of the day: both come from the same buckets, so
	// the load reflects real ACTIVITY, not the start time. Buckets below lowFloor
	// are no-op noise (a 32-byte incremental is not a peak).
	hourBytes := map[int]int64{}
	hourActive := map[int]map[string]bool{}
	var peak *bucket
	for t, bk := range buckets {
		if bk.bytes < lowFloor {
			continue
		}
		if peak == nil || bk.bytes > peak.bytes {
			peak, a.PeakAt = bk, t.Format("2006-01-02 15:04")
		}
		hourBytes[t.Hour()] += bk.bytes
		if hourActive[t.Hour()] == nil {
			hourActive[t.Hour()] = map[string]bool{}
		}
		for j := range bk.jobs {
			hourActive[t.Hour()][j] = true
		}
	}
	if peak != nil {
		a.PeakBytesPerHour = peak.bytes
		a.PeakBusySec = round1(unionSeconds(peak.spans))
		// Rate over the time it was actually working, not over the clock hour.
		if a.PeakBusySec > 0 {
			a.PeakMBps = round1(float64(peak.bytes) / a.PeakBusySec / 1e6)
		}
	}

	for h := 0; h < 24; h++ {
		a.Hours = append(a.Hours, HourLoad{
			Hour: h, Bytes: hourBytes[h] / int64(a.Days), Runs: hourRuns[h],
			JobStarts: len(hourJobs[h]), JobsActive: len(hourActive[h]),
		})
	}
	// Busiest hour by data, and how many distinct jobs were RUNNING then. Starts
	// are a different question and are used by the staggering rule below.
	a.BusiestHour = -1
	var maxH int64
	for h, b := range hourBytes {
		if b > maxH {
			maxH, a.BusiestHour = b, h
		}
	}
	if a.BusiestHour >= 0 {
		a.BusiestJobs = len(hourActive[a.BusiestHour])
		if a.TotalBytes > 0 {
			a.BusiestPct = pct(maxH, a.TotalBytes)
		}
	}
	// Staggering candidate: the hour where most distinct jobs START.
	a.StaggerHour = -1
	for h, jobs := range hourJobs {
		if len(jobs) > a.StaggerJobs {
			a.StaggerJobs, a.StaggerHour = len(jobs), h
		}
	}
	// The share is what those runs CARRY, not what lands in that clock hour: three
	// jobs starting at 02:00 may move most of their data after 03:00.
	if a.StaggerHour >= 0 && a.TotalBytes > 0 {
		a.StaggerPct = pct(startBytes[a.StaggerHour], a.TotalBytes)
	}

	// Cuello del entorno.
	for st, b := range a.StageBytes {
		if b > a.StageBytes[a.TopStage] || a.TopStage == "" {
			a.TopStage = st
		}
	}
	if bindingTotal > 0 && a.TopStage != "" {
		a.TopStagePct = pct(a.StageBytes[a.TopStage], bindingTotal)
	}

	// Hotspots: recursos que concentran el dato binding.
	for k, b := range resBytes {
		kind, id := splitKey(k)
		name := repoNames[id]
		if kind == "proxy" {
			name = proxyNames[id]
		}
		if name == "" {
			name = id
		}
		h := Hotspot{ID: id, Name: name, Kind: kind, Runs: resRuns[k], Bytes: b,
			SharePct: pct(b, bindingTotal), Stage: resStage[k]}
		// Rate del recurso = dato / tiempo de PARED en que estuvo ocupado (union de
		// intervalos). Sumar duraciones duplicaria el tiempo cuando dos jobs le
		// pegan en paralelo y subestimaria el rate.
		if d := unionSeconds(resSpans[k]); d > 0 {
			h.ThrouMBps = round1(float64(b) / d / 1e6)
		}
		a.Hotspots = append(a.Hotspots, h)
	}
	sort.SliceStable(a.Hotspots, func(i, j int) bool { return a.Hotspots[i].Bytes > a.Hotspots[j].Bytes })
	if len(a.Hotspots) > 5 {
		a.Hotspots = a.Hotspots[:5]
	}

	a.verdict()
	return a
}

// verdict: headline + acciones del entorno (mismo tipo Action que el veredicto
// por job, asi el WebUI las pinta igual).
func (a *Assessment) verdict() {
	var acts []Action
	add := func(impact, code, text string, params map[string]any) {
		acts = append(acts, Action{Impact: impact, Code: code, Text: text, Params: params, Source: "observed"})
	}

	if a.DataRuns == 0 {
		a.Severity = "unknown"
		a.HeadlineCode, a.Headline = "env.nodata", "No run in the window moved meaningful data: the infrastructure cannot be assessed"
		add("verify", "act.envNoData",
			fmt.Sprintf("%d job(s) only ran near-empty incrementals in these %d day(s): widen the window or include an Active Full to measure the infrastructure.", a.NoDataJobs, a.Days),
			map[string]any{"n": a.NoDataJobs, "days": a.Days})
		a.Actions = rank(acts)
		return
	}

	// Headline: cuanto sostiene + donde topa.
	a.Severity = "warn"
	if a.TopStage != "" && a.TopStagePct >= envStageShare {
		a.HeadlineCode = "env.bound"
		a.Headline = fmt.Sprintf("The infrastructure sustained %.0f MB/s at peak; %s is the recurring bottleneck (%d%% of the data moved)", a.PeakMBps, a.TopStage, a.TopStagePct)
		a.HeadlineParams = map[string]any{"mbps": a.PeakMBps, "stage": a.TopStage, "pct": a.TopStagePct}
	} else {
		a.Severity = "ok"
		a.HeadlineCode = "env.spread"
		a.Headline = fmt.Sprintf("The infrastructure sustained %.0f MB/s at peak; no single stage dominates the bottleneck", a.PeakMBps)
		a.HeadlineParams = map[string]any{"mbps": a.PeakMBps}
	}

	// 1) El recurso que concentra el cuello: es donde conviene medir y escalar.
	for _, h := range a.Hotspots {
		if h.SharePct < hotspotShare {
			continue
		}
		if h.Kind == "repository" {
			add("high", "act.envRepo",
				fmt.Sprintf("%s is the bottleneck for %d%% of the data (%.0f MB/s observed over %d run(s)): measure it with fio and check its task slots before adding more jobs to it.", h.Name, h.SharePct, h.ThrouMBps, h.Runs),
				map[string]any{"name": h.Name, "pct": h.SharePct, "mbps": h.ThrouMBps, "runs": h.Runs})
		} else {
			add("high", "act.envProxy",
				fmt.Sprintf("%s is the bottleneck for %d%% of the data (%s stage, %.0f MB/s observed): validate its link with iperf and its CPU/task slots.", h.Name, h.SharePct, h.Stage, h.ThrouMBps),
				map[string]any{"name": h.Name, "pct": h.SharePct, "stage": h.Stage, "mbps": h.ThrouMBps})
		}
		a.Severity = "critical"
		break // solo el principal: el resto queda en la tabla de hotspots
	}

	// 2) Source side: no single resource owns it, so name the read path instead of
	// leaving the environment with nothing but "go measure".
	if a.TopStage == "Source" && a.TopStagePct >= envStageShare {
		add("high", "act.envSource",
			fmt.Sprintf("The source side is the bottleneck for %d%% of the data: the read path (transport mode, datastore/volume latency, CBT) sets the pace, so adding proxies or repositories will not help. Use deep mode on the heaviest job to get the actual transport.", a.TopStagePct),
			map[string]any{"pct": a.TopStagePct})
	}

	// 3) Overlap: several jobs STARTING in the same hour and concentrating the
	// data. Moving schedules is free, so it comes before any hardware.
	if a.StaggerJobs >= staggerJobs && a.StaggerPct >= 40 {
		add("medium", "act.envStagger",
			fmt.Sprintf("%d different jobs start at %02d:00 and concentrate %d%% of the data: staggering them spreads the peak without buying anything.", a.StaggerJobs, a.StaggerHour, a.StaggerPct),
			map[string]any{"jobs": a.StaggerJobs, "hour": fmt.Sprintf("%02d", a.StaggerHour), "pct": a.StaggerPct})
	}

	// 4) Jobs that cannot be judged.
	if a.NoDataJobs > 0 {
		add("verify", "act.envNoData",
			fmt.Sprintf("%d job(s) only ran near-empty incrementals in these %d day(s): widen the window or include an Active Full to measure the infrastructure.", a.NoDataJobs, a.Days),
			map[string]any{"n": a.NoDataJobs, "days": a.Days})
	}

	// 5) The measured ceiling is still the missing input.
	add("verify", "act.envMeasure",
		"Measure the ceiling of the busiest repository (fio) and of the proxy<->repository link (iperf): that turns \"what the jobs did\" into \"what the hardware can do\".", nil)

	a.Actions = rank(acts)
}

// --- helpers ----------------------------------------------------------------

func getBucket(m map[time.Time]*bucket, t time.Time) *bucket {
	bk := m[t]
	if bk == nil {
		bk = &bucket{jobs: map[string]bool{}}
		m[t] = bk
	}
	return bk
}

// spread splits a run's bytes across the absolute hours it covers, proportional
// to the time spent in each, and records the busy interval and the job in every
// hour touched. Without this the whole peak lands on the start hour.
func spread(buckets map[time.Time]*bucket, start, end time.Time, bytes int64, jobID string) {
	total := end.Sub(start).Seconds()
	if total <= 0 {
		bk := getBucket(buckets, start.Truncate(time.Hour))
		bk.bytes += bytes
		bk.jobs[jobID] = true
		return
	}
	for t := start.Truncate(time.Hour); t.Before(end); t = t.Add(time.Hour) {
		lo, hi := t, t.Add(time.Hour)
		if lo.Before(start) {
			lo = start
		}
		if hi.After(end) {
			hi = end
		}
		if s := hi.Sub(lo).Seconds(); s > 0 {
			bk := getBucket(buckets, t)
			bk.bytes += int64(float64(bytes) * s / total)
			bk.spans = append(bk.spans, span{lo, hi})
			bk.jobs[jobID] = true
		}
	}
}

// span: un intervalo de actividad [de, a).
type span struct{ from, to time.Time }

// unionSeconds: segundos cubiertos por los intervalos, contando los solapes UNA
// sola vez (tiempo de pared en que el recurso estuvo ocupado).
func unionSeconds(sp []span) float64 {
	if len(sp) == 0 {
		return 0
	}
	s := make([]span, len(sp))
	copy(s, sp)
	sort.Slice(s, func(i, j int) bool { return s[i].from.Before(s[j].from) })
	var total float64
	cur := s[0]
	for _, x := range s[1:] {
		if x.from.After(cur.to) { // hueco: cerramos el tramo
			total += cur.to.Sub(cur.from).Seconds()
			cur = x
			continue
		}
		if x.to.After(cur.to) {
			cur.to = x.to
		}
	}
	return total + cur.to.Sub(cur.from).Seconds()
}

func rank(acts []Action) []Action {
	sort.SliceStable(acts, func(i, j int) bool { return impactOrder[acts[i].Impact] < impactOrder[acts[j].Impact] })
	for i := range acts {
		acts[i].Rank = i + 1
	}
	return acts
}

func pct(part, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(float64(part)/float64(total)*100 + 0.5)
}

func splitKey(k string) (kind, id string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return "", k
}
