// Package deeplog parsea los logs en disco del VBR (Job.<name>.log + Task.<vm>.log)
// para enriquecer el analisis por job con lo que la REST NO expone: 4-stage POR VM,
// modo de transporte (nbd/hotadd/san) y POR QUE, duraciones/concurrencia precisas,
// y opciones del job. Es un parser de texto puro (sin acceso a red): quien obtiene
// los archivos (SSH/SMB) le pasa el contenido. Ver internal/deeplog/fetch.go.
package deeplog

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// --- tipos de salida --------------------------------------------------------

type Stage4 struct {
	Source  int `json:"source"`
	Proxy   int `json:"proxy"`
	Network int `json:"network"`
	Target  int `json:"target"`
}

type DiskInfo struct {
	Label      string `json:"label"`
	Path       string `json:"path"`
	CapacityGB int    `json:"capacityGB"`
	Thin       bool   `json:"thin"`
}

type VMDeep struct {
	Name        string     `json:"name"`
	Busy        *Stage4    `json:"busy"` // 4-stage POR VM (del Task log)
	Primary     string     `json:"primary"`
	DurationSec float64    `json:"durationSec"`
	Disks       []DiskInfo `json:"disks"`

	scanned bool // los discos salieron del escaneo de paths, no de la linea "Disk:"
}

type Result struct {
	JobName       string   `json:"jobName"`
	RunAt         string   `json:"runAt"`         // corrida analizada (timestamp tal cual lo escribe el log)
	Transport     string   `json:"transport"`     // nbd | hotadd | san | mixed | ""
	TransportNote string   `json:"transportNote"` // por que (ej: hotadd no disponible)
	Aggregate     *Stage4  `json:"aggregate"`     // Load: agregado del job
	Primary       string   `json:"primary"`
	VMs           []VMDeep `json:"vms"`
	Dedup         *bool    `json:"dedup,omitempty"`
	Compression   *int     `json:"compression,omitempty"`
	BlockSizeKB   *int     `json:"blockSizeKB,omitempty"`
	Notes         []string `json:"notes"`

	jobDurations map[string]float64 // interno: duracion por thread de VM (del Job log)
}

// --- regex ------------------------------------------------------------------

var (
	loadRe = regexp.MustCompile(`Source\s+(\d+)%\s*>\s*Proxy\s+(\d+)%\s*>\s*Network\s+(\d+)%\s*>\s*Target\s+(\d+)%`)
	primRe = regexp.MustCompile(`Primary bottleneck:\s*(\w+)`)
	modeRe = regexp.MustCompile(`Detected mode \[(\w+)\]`)
	durRe  = regexp.MustCompile(`Completed: THREAD: ([^\s(]+) \(CancellableThread\.Create: \d+\) in\s+(\d+):(\d+):(\d+)`)
	diskRe = regexp.MustCompile(`Disk: label "([^"]+)", path "([^"]+)", capacity (\d+) GB.*?thinProvisioned "(\w+)"`)
	// Fallback cuando no hay linea "Disk: label ..." (Hyper-V, agentes): contamos
	// los discos virtuales nombrados en el log. Los .avhdx son checkpoints
	// (delta), no discos del guest, y quedan afuera a proposito.
	vdiskRe  = regexp.MustCompile(`(?i)['"\[]((?:[A-Za-z]:\\|\\\\)[^'"\]\r\n]+?\.(?:vhdx|vhd|vmdk))['"\]]`)
	dedupRe  = regexp.MustCompile(`<EnableDeduplication>(\w+)</EnableDeduplication>`)
	compRe   = regexp.MustCompile(`<CompressionLevel>(\d+)</CompressionLevel>`)
	blockRe  = regexp.MustCompile(`<StgBlockSize>KbBlockSize(\d+)</StgBlockSize>`)
	hotaddNo = regexp.MustCompile(`(?i)not on suitable ESX|No disks can be processed through hotadd`)
	// Prefijo de fecha de la linea de log (`[05.08.2026 02:03:40] ...`). NO lo
	// parseamos: lo mostramos tal cual, asi no dependemos del formato de fecha,
	// que cambia con el locale del VBR.
	tsRe = regexp.MustCompile(`\[([^\]\r\n]{6,40})\]`)
)

// lastRunSegment: tramo del log de la corrida MAS RECIENTE. Los Job/Task logs de
// Veeam acumulan varias corridas en el mismo archivo, y la linea "Load:"/"Busy:"
// aparece al cerrar cada una: todo lo que viene despues de la PENULTIMA pertenece
// a la ultima. Es la unica frontera confiable sin depender de marcas internas de
// Veeam (que cambian entre versiones).
func lastRunSegment(s string) string {
	locs := loadRe.FindAllStringIndex(s, -1)
	if len(locs) < 2 {
		return s
	}
	return s[locs[len(locs)-2][1]:]
}

// lastSubmatch: la ULTIMA coincidencia (la mas reciente del log), no la primera.
func lastSubmatch(re *regexp.Regexp, s string) []string {
	all := re.FindAllStringSubmatch(s, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// runAtOf: el timestamp de la linea donde cierra la corrida (la del "Load:"),
// para poder decirle al usuario QUE corrida se analizo.
func runAtOf(s string) string {
	locs := loadRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return ""
	}
	at := locs[len(locs)-1][0]
	start := strings.LastIndexByte(s[:at], '\n') + 1
	if m := tsRe.FindStringSubmatch(s[start:at]); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// --- API --------------------------------------------------------------------

// Parse arma el resultado deep desde el contenido del Job log y los Task logs
// (mapa nombreVM -> contenido). taskLogs puede estar vacio (solo Job log).
func Parse(jobLog string, taskLogs map[string]string) Result {
	r := Result{Notes: []string{}, jobDurations: map[string]float64{}}
	parseJob(jobLog, &r)
	for vm, content := range taskLogs {
		r.VMs = append(r.VMs, parseTask(vm, content))
	}
	r.applyDurations()
	sort.SliceStable(r.VMs, func(i, j int) bool { return r.VMs[i].DurationSec > r.VMs[j].DurationSec })
	for _, vm := range r.VMs {
		if vm.scanned { // avisamos que el listado es best-effort (sin capacidad)
			r.Notes = append(r.Notes, "Disk list taken from the virtual-disk paths in the log (no capacity/thin info): this workload does not log the VMware-style \"Disk:\" line.")
			break
		}
	}
	if len(taskLogs) == 0 {
		r.Notes = append(r.Notes, "No Task.*.log found for this job: per-VM stages and durations are not available (only the job-level view). Agent/plugin jobs do not write per-VM task logs.")
	}
	if r.Aggregate == nil {
		r.Notes = append(r.Notes, "No \"Load:\" line in the job log: the log may belong to a run that did not complete, or the job type does not report it.")
	}
	return r
}

// pathBase: nombre de archivo de un path Windows/UNC (no usamos filepath porque
// el path es del VBR remoto, no del host donde corre esto).
func pathBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// parseJob analiza SOLO la corrida mas reciente del Job log (ver lastRunSegment):
// mezclar corridas daria un veredicto sobre datos que no existieron juntos.
func parseJob(full string, r *Result) {
	r.RunAt = runAtOf(full)
	s := lastRunSegment(full)

	// Transporte de esta corrida (puede haber varios modos; nos quedamos con el conjunto).
	modes := map[string]bool{}
	for _, m := range modeRe.FindAllStringSubmatch(s, -1) {
		modes[strings.ToLower(m[1])] = true
	}
	r.Transport = joinModes(modes)
	if modes["nbd"] && hotaddNo.MatchString(s) {
		r.TransportNote = "hotadd unavailable (proxy is not a VM on a suitable ESX) -> failover to network (nbd). Deploy a hotadd-capable proxy for faster source reads."
	}
	// Load agregado + primary (los ultimos: los de esta corrida).
	if m := lastSubmatch(loadRe, s); m != nil {
		r.Aggregate = &Stage4{atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])}
	}
	if m := lastSubmatch(primRe, s); m != nil {
		r.Primary = m[1]
	}
	// Duraciones por VM (threads de VM, no los internos "VBR.*").
	durs := map[string]float64{}
	for _, m := range durRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if strings.HasPrefix(name, "VBR.") || strings.Contains(name, "Pipeline") {
			continue
		}
		durs[name] = float64(atoi(m[2])*3600 + atoi(m[3])*60 + atoi(m[4]))
	}
	r.jobDurations = durs
	// Opciones vigentes en esta corrida.
	if m := lastSubmatch(dedupRe, s); m != nil {
		v := strings.EqualFold(m[1], "true")
		r.Dedup = &v
	}
	if m := lastSubmatch(compRe, s); m != nil {
		v := atoi(m[1])
		r.Compression = &v
	}
	if m := lastSubmatch(blockRe, s); m != nil {
		v := atoi(m[1])
		r.BlockSizeKB = &v
	}
}

// parseTask: idem, la corrida mas reciente del Task log de esa VM.
func parseTask(vm, full string) VMDeep {
	d := VMDeep{Name: vm}
	s := lastRunSegment(full)
	if m := lastSubmatch(loadRe, s); m != nil {
		d.Busy = &Stage4{atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])}
	}
	if m := lastSubmatch(primRe, s); m != nil {
		d.Primary = m[1]
	}
	seen := map[string]bool{}
	for _, m := range diskRe.FindAllStringSubmatch(s, -1) {
		key := m[1] + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		d.Disks = append(d.Disks, DiskInfo{Label: m[1], Path: m[2], CapacityGB: atoi(m[3]), Thin: strings.EqualFold(m[4], "true")})
	}
	if len(d.Disks) == 0 { // Hyper-V / agentes: sin linea "Disk:", escaneamos paths
		for _, m := range vdiskRe.FindAllStringSubmatch(s, -1) {
			p := m[1]
			if strings.Contains(strings.ToLower(p), ".avhdx") { // checkpoint, no disco del guest
				continue
			}
			key := strings.ToLower(p)
			if seen[key] {
				continue
			}
			seen[key] = true
			d.Disks = append(d.Disks, DiskInfo{Label: pathBase(p), Path: p})
			d.scanned = true
		}
	}
	return d
}

// jobDurations se comparte entre parseJob y Parse via el struct (campo no exportado).
// Lo aplicamos a las VMs por nombre (match exacto o por prefijo del hostname).
func (r *Result) applyDurations() {
	for i := range r.VMs {
		name := r.VMs[i].Name
		if d, ok := r.jobDurations[name]; ok {
			r.VMs[i].DurationSec = d
			continue
		}
		// match por prefijo (Task usa "dc01" y el thread "dc01.hackshack.local")
		for tn, d := range r.jobDurations {
			if strings.HasPrefix(tn, name+".") || strings.HasPrefix(name, tn+".") {
				r.VMs[i].DurationSec = d
				break
			}
		}
	}
}

// --- helpers ----------------------------------------------------------------

func joinModes(m map[string]bool) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) == 0 {
		return ""
	}
	if len(ks) == 1 {
		return ks[0]
	}
	return "mixed (" + strings.Join(ks, ", ") + ")"
}

func atoi(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
