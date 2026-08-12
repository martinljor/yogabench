package analysis

// verdict.go — Motor de VEREDICTO (v0.5). Toma las senales que ya tenemos y las
// sintetiza en UNA conclusion accionable: que topa, POR QUE, cuanto se puede
// ganar y que hacer primero. Es deterministico (reglas, no LLM): mismo input =
// mismo veredicto, auditable y sin datos que salgan del sitio.
//
// Senales que entran:
//   1. Observado  (REST)      : stage que topa, % de corridas, rates, duracion.
//   2. Causa      (deep logs) : transporte y por que, 4-stage por VM, opciones.
//   3. Recursos               : task slots vs cores/RAM, capacidad/free del repo.
//   4. Medido     (fio/iperf) : techo real del hardware (pendiente de wire).
//
// Los textos salen como codigo i18n + params (el WebUI traduce) y ademas como
// texto en ingles (fallback, logs y diagnostico).

import (
	"fmt"
	"sort"
	"strings"

	"yogabench/internal/deeplog"
)

// maxCredibleGain: el modelo lineal (T_new ≈ T_now × U_next/U_binding) deja de
// ser creible mas alla de esto; preferimos subestimar antes que prometer.
const maxCredibleGain = 75

// Action: una accion concreta, rankeada por impacto y con ganancia estimada.
type Action struct {
	Rank    int            `json:"rank"`
	Impact  string         `json:"impact"` // high | hygiene | medium | verify | info
	Code    string         `json:"code"`   // clave i18n (vd.<code>)
	Params  map[string]any `json:"params,omitempty"`
	Text    string         `json:"text"` // fallback EN (log/diagnostico)
	GainPct *int           `json:"gainPct,omitempty"`
	Alt     bool           `json:"alt,omitempty"` // alternativa a la anterior (NO acumulativa)
	Stage   string         `json:"stage,omitempty"`
	Source  string         `json:"source"` // observed | deep | resources | model
}

// Signal: que dato entro (o falta) en el veredicto. Le dice al usuario como
// hacerlo mas firme.
type Signal struct {
	Name   string         `json:"name"` // observed | deep | resources | measured
	OK     bool           `json:"ok"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	Text   string         `json:"text"`
}

// Verdict: la salida principal del analisis de un job.
type Verdict struct {
	Severity   string `json:"severity"`   // critical | warn | ok | unknown
	Confidence string `json:"confidence"` // observed | low | insufficient
	Stage      string `json:"stage"`      // stage que topa
	Stage2     string `json:"stage2,omitempty"`
	StageUtil  int    `json:"stageUtil"`

	HeadlineCode   string         `json:"headlineCode"`
	HeadlineParams map[string]any `json:"headlineParams,omitempty"`
	Headline       string         `json:"headline"`

	CauseCode   string         `json:"causeCode,omitempty"`
	CauseParams map[string]any `json:"causeParams,omitempty"`
	Cause       string         `json:"cause,omitempty"`
	CauseKnown  bool           `json:"causeKnown"`

	GainPct    int     `json:"gainPct"` // ganancia de tiempo estimada (0 = no estimable)
	CurrentSec float64 `json:"currentSec"`
	TargetSec  float64 `json:"targetSec"`

	Actions []Action `json:"actions"`
	Signals []Signal `json:"signals"`
	HasDeep bool     `json:"hasDeep"`
}

var impactOrder = map[string]int{"high": 0, "hygiene": 1, "medium": 2, "verify": 3, "info": 4}

// --- builder ----------------------------------------------------------------

type vbuild struct {
	v    *Verdict
	r    *JobCapacityResult
	d    *deeplog.Result
	gain *int // ganancia a atribuir a la accion que libera el cuello
	acts []Action
}

func (b *vbuild) add(impact, stage, source, code, text string, params map[string]any) {
	b.acts = append(b.acts, Action{Impact: impact, Stage: stage, Source: source, Code: code, Text: text, Params: params})
}

// addGain: accion que libera el cuello -> lleva la ganancia estimada.
func (b *vbuild) addGain(impact, stage, source, code, text string, params map[string]any) {
	b.acts = append(b.acts, Action{Impact: impact, Stage: stage, Source: source, Code: code, Text: text, Params: params, GainPct: b.gain})
}

// alt: marca la ultima accion agregada como alternativa de la anterior.
func (b *vbuild) alt() {
	if n := len(b.acts); n > 0 {
		b.acts[n-1].Alt = true
	}
}

func (b *vbuild) headline(code, text string, params map[string]any) {
	b.v.HeadlineCode, b.v.Headline, b.v.HeadlineParams = code, text, params
}

func (b *vbuild) cause(known bool, code, text string, params map[string]any) {
	b.v.CauseKnown, b.v.CauseCode, b.v.Cause, b.v.CauseParams = known, code, text, params
}

// res: primer recurso del tipo pedido (proxy | repository).
func (b *vbuild) res(kind string) *ResourceInfo {
	for i := range b.r.Resources {
		if b.r.Resources[i].Kind == kind {
			return &b.r.Resources[i]
		}
	}
	return nil
}

// --- API --------------------------------------------------------------------

// BuildVerdict sintetiza el veredicto del job. d puede ser nil (sin modo deep):
// en ese caso la causa queda por confirmar y se sugiere correr el deep.
func BuildVerdict(r *JobCapacityResult, d *deeplog.Result) *Verdict {
	if r == nil {
		return nil
	}
	v := &Verdict{Confidence: r.Confidence, CurrentSec: r.DurationSec, HasDeep: d != nil}
	b := &vbuild{v: v, r: r, d: d}

	// Sin datos reales movidos no hay veredicto de performance posible.
	if r.Confidence == "insufficient" {
		v.Severity = "unknown"
		b.headline("hl.nodata", "Not enough data to judge this job", nil)
		b.cause(false, "cause.noop",
			fmt.Sprintf("All %d run(s) in the window were near no-op incrementals (almost no data moved): rates and bottleneck are not meaningful.", r.Runs),
			map[string]any{"runs": r.Runs})
		b.add("verify", "", "observed", "act.window",
			"Widen the window (more days) or run an Active Full so the pipeline actually moves data, then analyze again.", nil)
		b.finish()
		return v
	}

	// Ganancia potencial (del modelo de proyeccion, acotada).
	if r.Projection != nil && r.Projection.ImprovementPct > 0 {
		g := r.Projection.ImprovementPct
		if g > maxCredibleGain {
			g = maxCredibleGain
		}
		v.GainPct = g
		v.TargetSec = round1(r.DurationSec * (1 - float64(g)/100))
		b.gain = &g
	}

	bind := bindingStages(r.Stages)
	switch {
	case len(bind) == 0 && r.Primary == "":
		v.Severity = "ok"
		b.headline("hl.balanced", "No stage saturates: the pipeline looks balanced", nil)
		b.cause(true, "cause.balanced", "No single stage is above the saturation threshold for this window.", nil)
		b.add("info", "", "model", "act.balanced",
			"Nothing to fix on this job: to raise the ceiling you have to scale in pairs (proxy + repository) and re-measure.", nil)
	case len(bind) == 0: // hay cuello dominante pero la REST no dio %
		v.Stage = r.Primary
		v.Severity = "warn"
		b.headline("hl.boundnopct",
			fmt.Sprintf("Limited by %s (dominant in %d%% of the runs)", r.Primary, r.PrimaryPct),
			map[string]any{"stage": r.Primary, "pct": r.PrimaryPct})
		b.stageRules(r.Primary, 0)
	default:
		v.Stage, v.StageUtil = bind[0].Name, bind[0].Util
		if bind[0].Util >= 95 {
			v.Severity = "critical"
		} else {
			v.Severity = "warn"
		}
		if len(bind) > 1 {
			v.Stage2 = bind[1].Name
			b.headline("hl.bound2",
				fmt.Sprintf("Limited by %s and %s together (%d%% / %d%%)", bind[0].Name, bind[1].Name, bind[0].Util, bind[1].Util),
				map[string]any{"stage": bind[0].Name, "stage2": bind[1].Name, "util": bind[0].Util, "util2": bind[1].Util})
		} else {
			b.headline("hl.bound",
				fmt.Sprintf("Limited by %s (%d%% busy)", bind[0].Name, bind[0].Util),
				map[string]any{"stage": bind[0].Name, "util": bind[0].Util})
		}
		for _, s := range bind {
			b.stageRules(s.Name, s.Util)
		}
	}

	b.crossCutting()
	b.finish()
	return v
}

// stageRules: catalogo de reglas por stage que topa (causa + acciones).
func (b *vbuild) stageRules(stage string, util int) {
	switch stage {
	case "Source":
		b.sourceRules()
	case "Proxy":
		if !b.v.CauseKnown {
			b.cause(true, "cause.proxyproc", "Proxy processing (dedup/compression/CPU) sets the pace, not the storage.", nil)
		}
		b.slotsRules("proxy", "Proxy", "add CPU/RAM to the proxy or deploy another one")
	case "Network":
		if !b.v.CauseKnown {
			b.cause(true, "cause.netlink", "The proxy<->repository link sets the pace: the data does not fit in the wire fast enough.", nil)
		}
		b.addGain("high", "Network", "model", "act.netUpgrade",
			"Co-locate the proxy with the repository (or move to a faster link) so the transfer stops being the limit.", nil)
		b.add("verify", "Network", "model", "act.iperf",
			"Measure the real proxy<->repository throughput with iperf (Benchmark > Network) to confirm the link is the limit and not the protocol.", nil)
	case "Target":
		if !b.v.CauseKnown {
			b.cause(true, "cause.repowrite", "Writing into the repository sets the pace.", nil)
		}
		b.slotsRules("repository", "Target", "move to a faster repository or add SOBR extents")
		b.add("verify", "Target", "model", "act.fio",
			"Measure the repository disk with fio (Benchmark > Disk) to know its real ceiling and how far the job is from it.", nil)
		b.compressionRule()
	}
}

// sourceRules: el origen topa. Con el deep sabemos el transporte (la causa mas
// frecuente de un Source-bound es leer por nbd cuando hotadd no esta disponible).
func (b *vbuild) sourceRules() {
	if b.d == nil {
		b.cause(false, "cause.srcunknown",
			"The source read is the ceiling, but the reason (transport mode) is only in the VBR logs: run deep mode to confirm it.", nil)
		b.add("verify", "Source", "deep", "act.deep",
			"Run deep mode on this job: it reads the VBR logs and reports the actual transport (nbd/hotadd/SAN), why it was chosen, and the 4-stage per VM.", nil)
		b.add("verify", "Source", "model", "act.srcStorage",
			"Check the source storage read speed/latency and CBT health: without them the job cannot read faster.", nil)
		return
	}
	tr := strings.ToLower(b.d.Transport)
	switch {
	case strings.Contains(tr, "nbd") && b.d.TransportNote != "":
		b.cause(true, "cause.nbdHotadd",
			"It reads over nbd (management network) because hotadd is not available: the proxy is not a VM on a suitable ESX.", nil)
		b.addGain("high", "Source", "deep", "act.hotadd",
			"Deploy a proxy as a VM on an ESX that sees the datastore, so it reads via hotadd instead of nbd: the source read stops going through the management network.", nil)
	case strings.Contains(tr, "nbd"):
		b.cause(true, "cause.nbd",
			"It reads over nbd (management network), the slowest transport mode.", nil)
		b.addGain("high", "Source", "deep", "act.hotadd",
			"Move the read off nbd: a proxy VM on a suitable ESX (hotadd) or a proxy with SAN access reads the datastore directly.", nil)
	case tr != "":
		b.cause(true, "cause.transportOK",
			fmt.Sprintf("Transport is already %s: the limit is the source storage itself, not the read mode.", b.d.Transport),
			map[string]any{"transport": b.d.Transport})
		b.add("verify", "Source", "deep", "act.srcStorage",
			"Check the source storage read speed/latency and CBT health: the transport mode is already the good one.", nil)
	default: // Hyper-V / agentes / NAS: no hay "Detected mode"
		b.cause(true, "cause.srcstore",
			"The source storage read is the ceiling (this workload has no VMware transport mode to change).", nil)
		b.add("verify", "Source", "deep", "act.srcStorage",
			"Check the source storage read speed/latency (host/CSV volume) and CBT/RCT health.", nil)
	}
	b.skewRules()
}

// slotsRules: consejo resource-gated sobre concurrencia. Con cores/RAM del host
// es FIRME (y aplica el gate de viabilidad ~1 task/core + 2GB/task); sin ellos
// queda como verificacion.
func (b *vbuild) slotsRules(kind, stage, scaleOut string) {
	res := b.res(kind)
	if res == nil {
		return
	}
	if res.Cores <= 0 { // no sabemos el hardware -> no prometemos
		if res.MaxTaskCount > 0 {
			b.add("verify", stage, "resources", "act.slotsUnknown",
				fmt.Sprintf("%s runs with %d task slot(s). Check the host CPU/RAM: if it has more than %d cores, raising the slots is a free win.", res.Name, res.MaxTaskCount, res.MaxTaskCount),
				map[string]any{"name": res.Name, "slots": res.MaxTaskCount})
		}
		return
	}
	viable := res.Cores
	if res.RamGB > 0 && res.RamGB/2 < viable {
		viable = res.RamGB / 2
	}
	switch {
	case viable <= 2: // gate de viabilidad: subir concurrencia no ayuda
		b.addGain("high", stage, "resources", "act.scaleHost",
			fmt.Sprintf("%s only has %d cores / %d GB: raising concurrency will not help, %s.", res.Name, res.Cores, res.RamGB, scaleOut),
			map[string]any{"name": res.Name, "cores": res.Cores, "ram": res.RamGB, "scaleOut": scaleOut})
	case res.MaxTaskCount < viable:
		b.addGain("high", stage, "resources", "act.slots",
			fmt.Sprintf("Raise %s task slots from %d to %d: the host has %d cores / %d GB and is under-used (free win, no new hardware).", res.Name, res.MaxTaskCount, viable, res.Cores, res.RamGB),
			map[string]any{"name": res.Name, "cur": res.MaxTaskCount, "target": viable, "cores": res.Cores, "ram": res.RamGB})
		b.cause(true, "cause.slots",
			fmt.Sprintf("%s is configured with %d task slot(s) for %d cores: the concurrency is below the hardware.", res.Name, res.MaxTaskCount, res.Cores),
			map[string]any{"name": res.Name, "slots": res.MaxTaskCount, "cores": res.Cores})
	default:
		b.addGain("high", stage, "resources", "act.atCapacity",
			fmt.Sprintf("%s is already at capacity (%d slots for %d cores): %s.", res.Name, res.MaxTaskCount, res.Cores, scaleOut),
			map[string]any{"name": res.Name, "slots": res.MaxTaskCount, "cores": res.Cores, "scaleOut": scaleOut})
	}
}

// compressionRule: si el repo topa y el job comprime poco, escribir menos bytes
// es la palanca mas barata (no requiere hardware).
func (b *vbuild) compressionRule() {
	if b.d == nil || b.d.Compression == nil || *b.d.Compression >= 5 {
		return
	}
	b.add("medium", "Target", "deep", "act.compression",
		fmt.Sprintf("Compression level is %d: raising it to 5 (optimal) writes fewer bytes into the repository, which is what is saturating.", *b.d.Compression),
		map[string]any{"level": *b.d.Compression})
}

// skewRules: con el deep tenemos duracion por VM. Con S = suma de las duraciones
// y T = duracion de la corrida, S/T es el paralelismo real: ~1 = se procesaron
// casi en serie, ~n = bien solapadas. Si una VM concentra el tiempo, aislarla.
// (S << T no es serializacion sino overhead de la corrida: no opinamos ahi.)
func (b *vbuild) skewRules() {
	if b.d == nil || len(b.d.VMs) < 2 || b.r.RepDurationSec <= 0 {
		return
	}
	var sum, max float64
	var top string
	for _, vm := range b.d.VMs {
		sum += vm.DurationSec
		if vm.DurationSec > max {
			max, top = vm.DurationSec, vm.Name
		}
	}
	if sum <= 0 {
		return
	}
	T := b.r.RepDurationSec
	switch {
	case len(b.d.VMs) >= 3 && max >= 0.6*sum: // una VM se lleva el tiempo
		pct := int(max / sum * 100)
		b.add("medium", "", "deep", "act.skew",
			fmt.Sprintf("%s takes %d%% of the VM time: give it its own job/window so it stops stretching this one.", top, pct),
			map[string]any{"vm": top, "pct": pct})
	case sum >= 0.8*T && sum <= 1.25*T && max <= 0.7*sum: // S ≈ T: sin solape
		b.add("medium", "", "deep", "act.serial",
			fmt.Sprintf("The %d VMs were processed almost serially (%s of VM time in a %s run): raise the job/proxy concurrency to overlap them.", len(b.d.VMs), fmtSec(sum), fmtSec(T)),
			map[string]any{"vms": len(b.d.VMs), "sum": fmtSec(sum), "dur": fmtSec(T)})
	}
}

// crossCutting: reglas que no dependen del stage que topa.
func (b *vbuild) crossCutting() {
	// Espacio del repositorio (higiene, pero puede frenar el backup).
	for _, res := range b.r.Resources {
		if res.Kind != "repository" || res.CapacityGB == nil || res.FreeGB == nil || *res.CapacityGB <= 0 {
			continue
		}
		pct := int(*res.FreeGB / *res.CapacityGB * 100)
		if pct >= 10 {
			continue
		}
		impact := "hygiene"
		if pct < 5 {
			impact = "high"
		}
		b.add(impact, "Target", "resources", "act.repoFree",
			fmt.Sprintf("%s has only %.0f GB free of %.0f GB (%d%%): free space or extend it before it stops the backups.", res.Name, *res.FreeGB, *res.CapacityGB, pct),
			map[string]any{"name": res.Name, "free": *res.FreeGB, "cap": *res.CapacityGB, "pct": pct})
	}
	// Confianza baja: los numeros son indicativos.
	if b.v.Confidence == "low" {
		b.add("verify", "", "observed", "act.window",
			"The runs in the window moved little data, so the rates are indicative: widen the window or include an Active Full for firm numbers.", nil)
	}
	// Sin deep, la causa nunca queda confirmada.
	if b.d == nil && b.v.Stage != "" && b.v.Stage != "Source" {
		b.add("verify", b.v.Stage, "deep", "act.deep",
			"Run deep mode to confirm the cause with the VBR logs: actual transport, 4-stage per VM and the job options.", nil)
	}
}

// finish ordena por impacto (estable), numera y arma las senales.
func (b *vbuild) finish() {
	sort.SliceStable(b.acts, func(i, j int) bool {
		return impactOrder[b.acts[i].Impact] < impactOrder[b.acts[j].Impact]
	})
	for i := range b.acts {
		b.acts[i].Rank = i + 1
	}
	b.v.Actions = b.acts

	r := b.r
	sig := []Signal{{
		Name: "observed", OK: r.Confidence == "observed", Code: "sig.observedD",
		Params: map[string]any{"runs": r.Runs, "data": r.RunsWithData},
		Text:   fmt.Sprintf("REST: %d run(s) in the window, %d moved data.", r.Runs, r.RunsWithData),
	}}
	if b.d != nil {
		tr := b.d.Transport
		if tr == "" {
			tr = "n/a"
		}
		sig = append(sig, Signal{Name: "deep", OK: true, Code: "sig.deepOn",
			Params: map[string]any{"transport": tr, "vms": len(b.d.VMs)},
			Text:   fmt.Sprintf("Deep logs: transport %s, %d VM(s) with per-VM stages.", tr, len(b.d.VMs))})
	} else {
		sig = append(sig, Signal{Name: "deep", OK: false, Code: "sig.deepOff",
			Text: "Deep logs: not read. The cause (transport, per-VM stages) stays unconfirmed."})
	}
	hasRes := false
	for _, x := range r.Resources {
		if x.Cores > 0 {
			hasRes = true
			break
		}
	}
	if hasRes {
		sig = append(sig, Signal{Name: "resources", OK: true, Code: "sig.resOn", Text: "Resources: host CPU/RAM known, concurrency advice is firm."})
	} else {
		sig = append(sig, Signal{Name: "resources", OK: false, Code: "sig.resOff", Text: "Resources: host CPU/RAM unknown, concurrency advice cannot be firm."})
	}
	sig = append(sig, Signal{Name: "measured", OK: false, Code: "sig.measOff",
		Text: "Measured ceiling: no fio/iperf run yet. Measure it to compare what the job does against what the hardware can do."})
	b.v.Signals = sig
}

// --- helpers ----------------------------------------------------------------

// bindingStages: stages que topan, del mas cargado al menos.
func bindingStages(st []StageInfo) []StageInfo {
	var out []StageInfo
	for _, s := range st {
		if s.Binding && s.Util > 0 {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Util > out[j].Util })
	return out
}

func fmtSec(s float64) string {
	m := int(s) / 60
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, int(s)%60)
	}
	return fmt.Sprintf("%ds", int(s))
}
