package analysis

// Tests del motor de veredicto: al ser deterministico, cada regla queda fijada
// aca. Si mañana cambiamos un umbral, estos tests dicen que veredicto cambia.

import (
	"testing"

	"yogabench/internal/deeplog"
)

// jobRes: resultado de capacidad minimo para ejercitar el motor.
func jobRes(stages []StageInfo, primary string, gain int) *JobCapacityResult {
	r := &JobCapacityResult{
		Confidence: "observed", Runs: 6, RunsWithData: 6, PrimaryPct: 100,
		DurationSec: 1240, RepDurationSec: 1240, Primary: primary, Stages: stages,
	}
	if gain > 0 {
		r.Projection = &Projection{NextStage: "Target", NewDurationSec: 400, ImprovementPct: gain}
	}
	return r
}

func st(s, p, n, t int) []StageInfo {
	util := []int{s, p, n, t}
	max := 0
	for _, u := range util {
		if u > max {
			max = u
		}
	}
	out := make([]StageInfo, 0, 4)
	for i, name := range stageNames {
		out = append(out, StageInfo{Name: name, Util: util[i], Binding: util[i] >= bindThreshold && util[i] >= max-coLimitSpread})
	}
	return out
}

func topAction(v *Verdict) Action {
	if len(v.Actions) == 0 {
		return Action{}
	}
	return v.Actions[0]
}

func hasAction(v *Verdict, code string) bool {
	for _, a := range v.Actions {
		if a.Code == code {
			return true
		}
	}
	return false
}

// Source topando + deep que dice nbd por hotadd no disponible = el caso estrella:
// causa confirmada y la accion #1 es desplegar un proxy hotadd, con ganancia.
func TestVerdictSourceBoundNbdHotadd(t *testing.T) {
	r := jobRes(st(99, 9, 19, 12), "Source", 19)
	d := &deeplog.Result{
		Transport:     "nbd",
		TransportNote: "hotadd unavailable (proxy is not a VM on a suitable ESX) -> failover to network (nbd).",
		VMs:           []deeplog.VMDeep{{Name: "vm1", DurationSec: 300}, {Name: "vm2", DurationSec: 200}},
	}
	v := BuildVerdict(r, d)
	if v.Stage != "Source" || v.Severity != "critical" {
		t.Fatalf("stage/severity: got %s/%s, want Source/critical", v.Stage, v.Severity)
	}
	if !v.CauseKnown || v.CauseCode != "cause.nbdHotadd" {
		t.Fatalf("cause: got %q (known=%v), want cause.nbdHotadd", v.CauseCode, v.CauseKnown)
	}
	top := topAction(v)
	if top.Code != "act.hotadd" || top.Impact != "high" {
		t.Fatalf("top action: got %s/%s, want act.hotadd/high", top.Code, top.Impact)
	}
	if top.GainPct == nil || *top.GainPct != 19 {
		t.Fatalf("top action gain: got %v, want 19", top.GainPct)
	}
	if v.GainPct != 19 || v.TargetSec <= 0 || v.TargetSec >= v.CurrentSec {
		t.Fatalf("projection: gain=%d current=%.0f target=%.0f", v.GainPct, v.CurrentSec, v.TargetSec)
	}
	if hasAction(v, "act.deep") {
		t.Error("should not ask for deep mode when deep data is present")
	}
	// 300+200=500s de tiempo de VM en una corrida de 1240s NO es serializacion
	// (es overhead de la corrida): no debemos opinar de concurrencia.
	if hasAction(v, "act.serial") {
		t.Error("must not report serialization when the VM time is far below the run duration")
	}
}

// Sin deep, la causa de un Source-bound no se puede confirmar: el motor lo dice
// y ofrece correr el deep en vez de inventar una causa.
func TestVerdictSourceBoundNoDeep(t *testing.T) {
	v := BuildVerdict(jobRes(st(97, 20, 30, 25), "Source", 30), nil)
	if v.CauseKnown {
		t.Errorf("cause should NOT be known without deep logs, got %q", v.CauseCode)
	}
	if !hasAction(v, "act.deep") {
		t.Error("expected act.deep to confirm the cause")
	}
	if top := topAction(v); top.Impact != "verify" {
		t.Errorf("without a confirmed cause the top action should be a verification, got %s/%s", top.Code, top.Impact)
	}
	for _, s := range v.Signals {
		if s.Name == "deep" && s.OK {
			t.Error("deep signal should be off")
		}
	}
}

// Target topando con cores/RAM conocidos y slots por debajo del hardware:
// recomendacion FIRME de subir los slots, con ganancia.
func TestVerdictTargetBoundRaiseSlots(t *testing.T) {
	r := jobRes(st(30, 40, 20, 96), "Target", 40)
	r.Resources = []ResourceInfo{{Name: "repo01", Kind: "repository", MaxTaskCount: 2, Cores: 8, RamGB: 32}}
	v := BuildVerdict(r, nil)
	top := topAction(v)
	if top.Code != "act.slots" || top.Impact != "high" {
		t.Fatalf("top action: got %s/%s, want act.slots/high", top.Code, top.Impact)
	}
	if top.Params["target"] != 8 || top.Params["cur"] != 2 {
		t.Fatalf("slots params: got %v, want cur=2 target=8", top.Params)
	}
	if v.CauseCode != "cause.slots" {
		t.Errorf("cause: got %q, want cause.slots", v.CauseCode)
	}
	if !hasAction(v, "act.fio") {
		t.Error("expected act.fio to measure the repo ceiling")
	}
}

// Gate de viabilidad: un host chico no se arregla subiendo concurrencia.
func TestVerdictTargetBoundLowResourceHost(t *testing.T) {
	r := jobRes(st(30, 40, 20, 96), "Target", 40)
	r.Resources = []ResourceInfo{{Name: "repo-mini", Kind: "repository", MaxTaskCount: 4, Cores: 2, RamGB: 4}}
	v := BuildVerdict(r, nil)
	if top := topAction(v); top.Code != "act.scaleHost" {
		t.Fatalf("top action: got %s, want act.scaleHost (raising slots must not be advised)", top.Code)
	}
	if hasAction(v, "act.slots") {
		t.Error("must not advise raising slots on a 2-core host")
	}
}

// Sin cores/RAM el consejo de concurrencia no puede ser firme: queda como
// verificacion, nunca como accion de alto impacto.
func TestVerdictSlotsUnknownHardware(t *testing.T) {
	r := jobRes(st(30, 96, 20, 40), "Proxy", 30)
	r.Resources = []ResourceInfo{{Name: "proxy01", Kind: "proxy", MaxTaskCount: 4}}
	v := BuildVerdict(r, nil)
	if !hasAction(v, "act.slotsUnknown") {
		t.Fatal("expected act.slotsUnknown")
	}
	for _, a := range v.Actions {
		if a.Code == "act.slotsUnknown" && a.Impact != "verify" {
			t.Errorf("slotsUnknown impact: got %s, want verify", a.Impact)
		}
		if a.Code == "act.slots" {
			t.Error("must not promise a slot number without knowing the hardware")
		}
	}
}

// Corridas no-op: no hay veredicto de performance, y se dice claramente.
func TestVerdictInsufficientData(t *testing.T) {
	r := jobRes(st(0, 0, 0, 0), "", 0)
	r.Confidence, r.RunsWithData = "insufficient", 0
	v := BuildVerdict(r, nil)
	if v.Severity != "unknown" || v.HeadlineCode != "hl.nodata" {
		t.Fatalf("got %s/%s, want unknown/hl.nodata", v.Severity, v.HeadlineCode)
	}
	if len(v.Actions) != 1 || v.Actions[0].Code != "act.window" {
		t.Fatalf("actions: got %v, want only act.window", v.Actions)
	}
	if v.GainPct != 0 {
		t.Error("must not promise a gain without data")
	}
}

// Pipeline balanceado: nada que corregir (y se dice, no se inventa un cuello).
func TestVerdictBalanced(t *testing.T) {
	v := BuildVerdict(jobRes(st(40, 50, 30, 45), "", 0), nil)
	if v.Severity != "ok" || v.HeadlineCode != "hl.balanced" {
		t.Fatalf("got %s/%s, want ok/hl.balanced", v.Severity, v.HeadlineCode)
	}
	if top := topAction(v); top.Code != "act.balanced" {
		t.Fatalf("top action: got %s, want act.balanced", top.Code)
	}
}

// Dos stages co-limitando: el headline los nombra a los dos.
func TestVerdictCoLimiting(t *testing.T) {
	v := BuildVerdict(jobRes(st(94, 20, 30, 91), "Source", 20), nil)
	if v.HeadlineCode != "hl.bound2" || v.Stage != "Source" || v.Stage2 != "Target" {
		t.Fatalf("got %s (%s/%s), want hl.bound2 (Source/Target)", v.HeadlineCode, v.Stage, v.Stage2)
	}
}

// Repo casi lleno: entra como accion aunque no sea el cuello de performance.
func TestVerdictRepoAlmostFull(t *testing.T) {
	r := jobRes(st(40, 50, 30, 45), "", 0)
	capGB, freeGB := 1000.0, 30.0
	r.Resources = []ResourceInfo{{Name: "repo01", Kind: "repository", CapacityGB: &capGB, FreeGB: &freeGB}}
	v := BuildVerdict(r, nil)
	if top := topAction(v); top.Code != "act.repoFree" || top.Impact != "high" {
		t.Fatalf("top action: got %s/%s, want act.repoFree/high (3%% free is critical)", top.Code, top.Impact)
	}
}

// VMs procesadas casi en serie: el deep lo detecta comparando la suma de las
// duraciones por VM contra la duracion de la corrida (600s de corrida para
// 600s de tiempo de VM = no se solaparon).
func TestVerdictSerialVMs(t *testing.T) {
	r := jobRes(st(96, 30, 20, 40), "Source", 25)
	r.RepDurationSec = 600
	d := &deeplog.Result{Transport: "hotadd", VMs: []deeplog.VMDeep{
		{Name: "vm1", DurationSec: 300}, {Name: "vm2", DurationSec: 300},
	}}
	if v := BuildVerdict(r, d); !hasAction(v, "act.serial") {
		t.Error("expected act.serial when the VM times add up to the run duration")
	}
	// Bien solapadas (600s de corrida para 900s de VM) no debe avisar nada.
	d.VMs = []deeplog.VMDeep{{Name: "vm1", DurationSec: 450}, {Name: "vm2", DurationSec: 450}}
	if v := BuildVerdict(r, d); hasAction(v, "act.serial") {
		t.Error("must not report serialization when the VMs overlapped")
	}
}

// El ranking siempre pone las acciones de alto impacto primero y numera desde 1.
func TestVerdictActionRanking(t *testing.T) {
	r := jobRes(st(30, 40, 20, 96), "Target", 40)
	capGB, freeGB := 1000.0, 80.0 // 8% libre -> higiene
	r.Resources = []ResourceInfo{
		{Name: "repo01", Kind: "repository", MaxTaskCount: 2, Cores: 8, RamGB: 32, CapacityGB: &capGB, FreeGB: &freeGB},
	}
	v := BuildVerdict(r, nil)
	last := -1
	for i, a := range v.Actions {
		if a.Rank != i+1 {
			t.Fatalf("action %d has rank %d", i, a.Rank)
		}
		if o := impactOrder[a.Impact]; o < last {
			t.Fatalf("action %s (%s) is out of impact order", a.Code, a.Impact)
		} else {
			last = o
		}
	}
	if v.Actions[0].Impact != "high" {
		t.Errorf("first action should be high impact, got %s", v.Actions[0].Impact)
	}
}

// La ganancia proyectada se acota: el modelo lineal no es creible al 90%.
func TestVerdictGainIsCapped(t *testing.T) {
	v := BuildVerdict(jobRes(st(99, 5, 5, 5), "Source", 95), nil)
	if v.GainPct != maxCredibleGain {
		t.Fatalf("gain: got %d, want capped at %d", v.GainPct, maxCredibleGain)
	}
}
