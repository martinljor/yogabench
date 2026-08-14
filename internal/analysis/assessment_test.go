package analysis

// Tests del veredicto del entorno. El mas importante es
// TestAssessmentWeightsByDataNotRuns: fija la ponderacion por dato movido.

import (
	"testing"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// rec: una corrida. start/end en "HH:MM" del 2026-08-10 (o del dia que se pase).
func rec(jobID, day, start, end string, xfer int64, primary string, repos, proxies []string) Record {
	r := Record{
		JobID: jobID, Name: jobID, Result: "Success",
		CreationTime: day + "T" + start + ":00", EndTime: day + "T" + end + ":00",
		TransferredSize: xfer, RepoIDs: repos, ProxyIDs: proxies,
	}
	if primary != "" {
		r.Bottleneck = map[string]any{"primary": primary}
	}
	if t1, ok1 := parseDT(r.CreationTime); ok1 {
		if t2, ok2 := parseDT(r.EndTime); ok2 {
			r.DurationSec = t2.Sub(t1).Seconds()
		}
	}
	return r
}

var names = map[string]string{"r1": "repo01", "r2": "repo-slow", "p1": "proxy01"}

// Contar corridas hace que 20 incrementales no-op le ganen a un full de 200 GiB.
// Ponderamos por dato movido, asi que el cuello que reportamos es el que afecta a
// los bytes reales.
func TestAssessmentWeightsByDataNotRuns(t *testing.T) {
	var recs []Record
	for i := 0; i < 20; i++ { // 20 no-op "Source" (32 B cada uno): ruido
		recs = append(recs, rec("noop-job", "2026-08-10", "01:00", "01:05", 32, "Source", []string{"r1"}, []string{"p1"}))
	}
	// 1 full real de 200 GiB con el repo topando.
	recs = append(recs, rec("full-job", "2026-08-10", "02:00", "04:00", 200*gib, "Target", []string{"r2"}, []string{"p1"}))

	a := BuildAssessment(recs, 1, names, names)
	if a.TopStage != "Target" {
		t.Fatalf("cuello del entorno: got %q, want Target (los 20 no-op no deben ganarle a 200 GiB)", a.TopStage)
	}
	if a.TopStagePct != 100 {
		t.Errorf("TopStagePct: got %d, want 100 (los no-op quedan afuera por lowFloor)", a.TopStagePct)
	}
	if a.DataRuns != 1 || a.Runs != 21 {
		t.Errorf("runs: got %d totales / %d con datos, want 21/1", a.Runs, a.DataRuns)
	}
	if len(a.Hotspots) == 0 || a.Hotspots[0].Name != "repo-slow" {
		t.Fatalf("hotspot: got %+v, want repo-slow primero", a.Hotspots)
	}
	if h := a.Hotspots[0]; h.SharePct != 100 || h.Kind != "repository" {
		t.Errorf("hotspot: got share=%d kind=%s, want 100/repository", h.SharePct, h.Kind)
	}
}

// El pico se reparte entre las horas que abarca la corrida: 200 GiB en 2 h son
// ~100 GiB/h, NO 200 GiB adjudicados a la hora de arranque.
func TestAssessmentPeakSpreadsOverHours(t *testing.T) {
	recs := []Record{rec("j1", "2026-08-10", "02:00", "04:00", 200*gib, "Target", []string{"r1"}, nil)}
	a := BuildAssessment(recs, 1, names, names)
	want := 100 * gib
	if d := a.PeakBytesPerHour - want; d > want/50 || d < -want/50 {
		t.Fatalf("pico: got %d B/h, want ~%d (200 GiB en 2 h)", a.PeakBytesPerHour, want)
	}
	// 100 GiB/h ≈ 29.8 MB/s sostenidos.
	if a.PeakMBps < 28 || a.PeakMBps > 31 {
		t.Errorf("PeakMBps: got %.1f, want ~29.8", a.PeakMBps)
	}
	if a.PeakAt == "" {
		t.Error("PeakAt vacio: no sabriamos cuando fue el pico")
	}
}

// Solape: 3 jobs distintos arrancando a las 02:00 con el grueso del dato = pico
// evitable moviendo horarios (gratis, sin hardware).
func TestAssessmentDetectsStagger(t *testing.T) {
	recs := []Record{
		rec("j1", "2026-08-10", "02:00", "03:00", 50*gib, "Target", []string{"r1"}, nil),
		rec("j2", "2026-08-10", "02:10", "03:00", 40*gib, "Target", []string{"r1"}, nil),
		rec("j3", "2026-08-10", "02:20", "03:00", 30*gib, "Target", []string{"r1"}, nil),
		rec("j4", "2026-08-10", "10:00", "10:30", 5*gib, "Source", nil, nil),
	}
	a := BuildAssessment(recs, 1, names, names)
	if a.BusiestHour != 2 || a.BusiestJobs != 3 {
		t.Fatalf("hora mas cargada: got %02d:00 con %d jobs, want 02:00 con 3", a.BusiestHour, a.BusiestJobs)
	}
	if !hasEnvAction(a, "act.envStagger") {
		t.Errorf("esperaba act.envStagger; acciones: %v", codes(a))
	}
	// Y la carga por hora debe estar poblada (para el histograma).
	if len(a.Hours) != 24 || a.Hours[2].Bytes == 0 {
		t.Errorf("histograma por hora mal armado: %+v", a.Hours[2])
	}
}

// Un solo job puntual no dispara el consejo de escalonar (no hay nada que mover).
func TestAssessmentNoStaggerWithOneJob(t *testing.T) {
	recs := []Record{rec("j1", "2026-08-10", "02:00", "03:00", 50*gib, "Target", []string{"r1"}, nil)}
	if a := BuildAssessment(recs, 1, names, names); hasEnvAction(a, "act.envStagger") {
		t.Error("no debe aconsejar escalonar con un solo job")
	}
}

// Ventana sin datos: no se puede dictaminar la infra, y se dice.
func TestAssessmentNoData(t *testing.T) {
	var recs []Record
	for i := 0; i < 5; i++ {
		recs = append(recs, rec("j1", "2026-08-10", "01:00", "01:02", 32, "Source", []string{"r1"}, nil))
	}
	a := BuildAssessment(recs, 7, names, names)
	if a.Severity != "unknown" || a.HeadlineCode != "env.nodata" {
		t.Fatalf("got %s/%s, want unknown/env.nodata", a.Severity, a.HeadlineCode)
	}
	if len(a.Actions) != 1 || a.Actions[0].Code != "act.envNoData" {
		t.Fatalf("acciones: %v", codes(a))
	}
	if a.PeakMBps != 0 {
		t.Error("no debe reportar un pico con corridas vacias")
	}
}

// Cuello repartido entre stages: no se declara un cuello de entorno.
func TestAssessmentSpreadBottleneck(t *testing.T) {
	recs := []Record{
		rec("j1", "2026-08-10", "02:00", "03:00", 30*gib, "Target", []string{"r1"}, nil),
		rec("j2", "2026-08-10", "04:00", "05:00", 30*gib, "Source", nil, nil),
		rec("j3", "2026-08-10", "06:00", "07:00", 30*gib, "Network", nil, []string{"p1"}),
	}
	a := BuildAssessment(recs, 1, names, names)
	if a.HeadlineCode != "env.spread" || a.Severity != "ok" {
		t.Fatalf("got %s/%s, want env.spread/ok (33%% por stage no es un cuello)", a.HeadlineCode, a.Severity)
	}
	if hasEnvAction(a, "act.envRepo") {
		t.Error("no debe senalar un hotspot cuando el cuello esta repartido")
	}
}

// El proxy tambien puede ser el hotspot (stage Proxy/Network).
func TestAssessmentProxyHotspot(t *testing.T) {
	recs := []Record{
		rec("j1", "2026-08-10", "02:00", "03:00", 100*gib, "Network", nil, []string{"p1"}),
		rec("j2", "2026-08-10", "04:00", "05:00", 10*gib, "Target", []string{"r1"}, nil),
	}
	a := BuildAssessment(recs, 1, names, names)
	if a.TopStage != "Network" {
		t.Fatalf("cuello: got %s, want Network", a.TopStage)
	}
	if !hasEnvAction(a, "act.envProxy") {
		t.Fatalf("esperaba act.envProxy; acciones: %v", codes(a))
	}
	if a.Hotspots[0].Name != "proxy01" || a.Hotspots[0].Kind != "proxy" {
		t.Errorf("hotspot: got %+v", a.Hotspots[0])
	}
}

// Siempre queda la accion de medir el techo (fio/iperf): es la pata que falta.
func TestAssessmentAlwaysSuggestsMeasuring(t *testing.T) {
	recs := []Record{rec("j1", "2026-08-10", "02:00", "03:00", 50*gib, "Target", []string{"r1"}, nil)}
	a := BuildAssessment(recs, 1, names, names)
	if !hasEnvAction(a, "act.envMeasure") {
		t.Errorf("esperaba act.envMeasure; acciones: %v", codes(a))
	}
	last := -1
	for i, x := range a.Actions {
		if x.Rank != i+1 {
			t.Fatalf("rank fuera de orden en %d: %d", i, x.Rank)
		}
		if o := impactOrder[x.Impact]; o < last {
			t.Fatalf("accion %s (%s) fuera de orden de impacto", x.Code, x.Impact)
		} else {
			last = o
		}
	}
}

// El rate de un recurso usa el tiempo de PARED (union de intervalos): si dos
// jobs le pegan al mismo repo en paralelo, sumar duraciones lo subestimaria.
func TestAssessmentHotspotRateUsesWallClock(t *testing.T) {
	// Dos jobs de 1 h EN PARALELO, 36 GB cada uno -> 72 GB en 1 h de pared = 20 MB/s.
	// (Sumando duraciones darian 2 h y 10 MB/s: la mitad.)
	gb := int64(36_000_000_000)
	recs := []Record{
		rec("j1", "2026-08-10", "02:00", "03:00", gb, "Target", []string{"r1"}, nil),
		rec("j2", "2026-08-10", "02:00", "03:00", gb, "Target", []string{"r1"}, nil),
	}
	a := BuildAssessment(recs, 1, names, names)
	if got := a.Hotspots[0].ThrouMBps; got < 19 || got > 21 {
		t.Fatalf("rate del repo: got %.1f MB/s, want ~20 (tiempo de pared, no suma de duraciones)", got)
	}
}

// unionSeconds: solapes contados una vez, huecos respetados.
func TestUnionSeconds(t *testing.T) {
	sp := func(a, b string) span {
		x, _ := parseDT("2026-08-10T" + a + ":00")
		y, _ := parseDT("2026-08-10T" + b + ":00")
		return span{x, y}
	}
	cases := []struct {
		name string
		in   []span
		want float64
	}{
		{"uno", []span{sp("02:00", "03:00")}, 3600},
		{"solapados", []span{sp("02:00", "03:00"), sp("02:30", "03:30")}, 5400},
		{"contenido", []span{sp("02:00", "04:00"), sp("02:30", "03:00")}, 7200},
		{"con hueco", []span{sp("02:00", "03:00"), sp("05:00", "06:00")}, 7200},
		{"desordenados", []span{sp("05:00", "06:00"), sp("02:00", "03:00")}, 7200},
		{"vacio", nil, 0},
	}
	for _, c := range cases {
		if got := unionSeconds(c.in); got != c.want {
			t.Errorf("%s: got %.0fs, want %.0fs", c.name, got, c.want)
		}
	}
}

// La carga por hora refleja la ACTIVIDAD, no la hora de arranque: una corrida de
// 22:00 a 02:00 reparte su dato entre esas horas (y cruza la medianoche).
func TestAssessmentHourLoadFollowsActivity(t *testing.T) {
	recs := []Record{rec("j1", "2026-08-10", "22:00", "23:59", 40*gib, "Target", []string{"r1"}, nil)}
	a := BuildAssessment(recs, 1, names, names)
	if a.Hours[22].Bytes == 0 || a.Hours[23].Bytes == 0 {
		t.Fatalf("la carga deberia estar en 22h y 23h: 22=%d 23=%d", a.Hours[22].Bytes, a.Hours[23].Bytes)
	}
	if a.Hours[22].JobStarts != 1 || a.Hours[23].JobStarts != 0 {
		t.Errorf("los arranques van solo en la hora de inicio: 22=%d 23=%d", a.Hours[22].JobStarts, a.Hours[23].JobStarts)
	}
	// Ninguna hora concentra el total: se repartio.
	if a.Hours[22].Bytes >= 40*gib {
		t.Errorf("22h se quedo con todo el dato (%d): no se repartio", a.Hours[22].Bytes)
	}
}

func hasEnvAction(a *Assessment, code string) bool {
	for _, x := range a.Actions {
		if x.Code == code {
			return true
		}
	}
	return false
}

func codes(a *Assessment) []string {
	var out []string
	for _, x := range a.Actions {
		out = append(out, x.Impact+":"+x.Code)
	}
	return out
}
