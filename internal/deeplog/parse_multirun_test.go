package deeplog

// Los Job/Task logs de Veeam ACUMULAN varias corridas en el mismo archivo. El
// analisis tiene que hablar de la corrida MAS RECIENTE, y no mezclar datos de
// dos corridas distintas (que era el bug: el Load salia de la primera y las
// duraciones de la ultima).

import (
	"strings"
	"testing"
)

// Job log con dos corridas: la vieja iba a un repo lento por SAN; la nueva quedo
// Source-bound leyendo por nbd. Formato calcado de los logs reales.
const twoRunJobLog = `[04.08.2026 02:00:01] <01> Info     [JobSession] Starting job Domain Backups
[04.08.2026 02:00:05] <01> Info     [ProxyDetector] Detected mode [san]
[04.08.2026 02:01:00] <01> Info     Completed: THREAD: dc01.hackshack.local (CancellableThread.Create: 1) in 0:12:00
[04.08.2026 02:12:30] <01> Info     [JobSession] Load: Source 10% > Proxy 20% > Network 30% > Target 90%
[04.08.2026 02:12:30] <01> Info     [JobSession] Primary bottleneck: Target
[04.08.2026 02:12:31] <01> Info     <CompressionLevel>4</CompressionLevel>
[04.08.2026 02:12:31] <01> Info     <StgBlockSize>KbBlockSize512</StgBlockSize>
[04.08.2026 02:12:31] <01> Info     <EnableDeduplication>False</EnableDeduplication>
[05.08.2026 02:00:01] <01> Info     [JobSession] Starting job Domain Backups
[05.08.2026 02:00:04] <01> Info     [ProxyDetector] Proxy VM is not on suitable ESX host, no hotadd
[05.08.2026 02:00:05] <01> Info     [ProxyDetector] Detected mode [nbd]
[05.08.2026 02:00:20] <01> Info     Completed: THREAD: dc01.hackshack.local (CancellableThread.Create: 1) in 0:03:20
[05.08.2026 02:03:40] <01> Info     [JobSession] Load: Source 99% > Proxy 9% > Network 19% > Target 12%
[05.08.2026 02:03:40] <01> Info     [JobSession] Primary bottleneck: Source
[05.08.2026 02:03:41] <01> Info     <CompressionLevel>5</CompressionLevel>
[05.08.2026 02:03:41] <01> Info     <StgBlockSize>KbBlockSize1024</StgBlockSize>
[05.08.2026 02:03:41] <01> Info     <EnableDeduplication>True</EnableDeduplication>
`

const twoRunTaskLog = `[04.08.2026 02:00:10] <01> Info     [AP] (dc01) Disk: label "Hard disk 1", path "[ds1] dc01/dc01.vmdk", capacity 100 GB, thinProvisioned "True"
[04.08.2026 02:12:29] <01> Info     [AP] (dc01) Busy: Source 12% > Proxy 22% > Network 33% > Target 88%
[04.08.2026 02:12:29] <01> Info     [AP] (dc01) Primary bottleneck: Target
[05.08.2026 02:00:10] <01> Info     [AP] (dc01) Disk: label "Hard disk 1", path "[ds1] dc01/dc01.vmdk", capacity 100 GB, thinProvisioned "True"
[05.08.2026 02:03:39] <01> Info     [AP] (dc01) Busy: Source 94% > Proxy 15% > Network 32% > Target 23%
[05.08.2026 02:03:39] <01> Info     [AP] (dc01) Primary bottleneck: Source
`

func TestParseUsesLatestRun(t *testing.T) {
	r := Parse(twoRunJobLog, map[string]string{"dc01": twoRunTaskLog})

	// Job: todo tiene que ser de la corrida del 05, no del 04.
	if r.Aggregate == nil {
		t.Fatal("sin Load agregado")
	}
	if got := *r.Aggregate; got != (Stage4{99, 9, 19, 12}) {
		t.Errorf("Load: got %+v, want {99 9 19 12} (la corrida del 05, no la del 04)", got)
	}
	if r.Primary != "Source" {
		t.Errorf("primary: got %q, want Source", r.Primary)
	}
	if r.Transport != "nbd" {
		t.Errorf("transporte: got %q, want nbd (la corrida vieja fue san)", r.Transport)
	}
	if r.TransportNote == "" {
		t.Error("esperaba el motivo del nbd (hotadd no disponible en el ESX)")
	}
	// Opciones: las de la corrida vigente.
	if r.Compression == nil || *r.Compression != 5 {
		t.Errorf("compresion: got %v, want 5", r.Compression)
	}
	if r.BlockSizeKB == nil || *r.BlockSizeKB != 1024 {
		t.Errorf("bloque: got %v, want 1024", r.BlockSizeKB)
	}
	if r.Dedup == nil || !*r.Dedup {
		t.Errorf("dedup: got %v, want true", r.Dedup)
	}
	// Duracion: la de la ultima corrida (3m20s), no la de la vieja (12m).
	if len(r.VMs) != 1 {
		t.Fatalf("VMs: got %d, want 1", len(r.VMs))
	}
	if got := r.VMs[0].DurationSec; got != 200 {
		t.Errorf("duracion: got %.0fs, want 200 (3m20s de la corrida del 05)", got)
	}
	// Per-VM: el Busy de la ultima corrida.
	if r.VMs[0].Busy == nil || *r.VMs[0].Busy != (Stage4{94, 15, 32, 23}) {
		t.Errorf("Busy por VM: got %+v, want {94 15 32 23}", r.VMs[0].Busy)
	}
	if r.VMs[0].Primary != "Source" {
		t.Errorf("primary por VM: got %q, want Source", r.VMs[0].Primary)
	}
	// Y tenemos que poder DECIR de que corrida hablamos.
	if r.RunAt == "" {
		t.Fatal("RunAt vacio: el usuario no puede verificar que corrida se analizo")
	}
	if !strings.HasPrefix(r.RunAt, "05.08.2026") {
		t.Errorf("RunAt: got %q, want la corrida del 05", r.RunAt)
	}
	// El disco no se cuenta dos veces por aparecer en las dos corridas.
	if n := len(r.VMs[0].Disks); n != 1 {
		t.Errorf("discos: got %d, want 1 (no duplicar entre corridas)", n)
	}
}

// Con una sola corrida el comportamiento no cambia.
func TestParseSingleRunUnaffected(t *testing.T) {
	one := `[05.08.2026 02:00:05] <01> Info     [ProxyDetector] Detected mode [hotadd]
[05.08.2026 02:00:20] <01> Info     Completed: THREAD: sql01 (CancellableThread.Create: 1) in 0:05:00
[05.08.2026 02:05:20] <01> Info     [JobSession] Load: Source 40% > Proxy 90% > Network 30% > Target 50%
[05.08.2026 02:05:20] <01> Info     [JobSession] Primary bottleneck: Proxy
`
	r := Parse(one, map[string]string{"sql01": "Busy: Source 40% > Proxy 90% > Network 30% > Target 50%\nPrimary bottleneck: Proxy\n"})
	if r.Aggregate == nil || *r.Aggregate != (Stage4{40, 90, 30, 50}) {
		t.Fatalf("Load: got %+v", r.Aggregate)
	}
	if r.Transport != "hotadd" || r.Primary != "Proxy" {
		t.Errorf("got transporte=%q primary=%q", r.Transport, r.Primary)
	}
	if len(r.VMs) != 1 || r.VMs[0].DurationSec != 300 {
		t.Errorf("VM: %+v", r.VMs)
	}
}

// Sin Task.*.log el analisis lo dice, en vez de mostrar una tabla vacia sin
// explicacion.
func TestParseWarnsWhenNoTaskLogs(t *testing.T) {
	r := Parse(twoRunJobLog, nil)
	if len(r.VMs) != 0 {
		t.Fatalf("VMs: got %d, want 0", len(r.VMs))
	}
	found := false
	for _, n := range r.Notes {
		if strings.Contains(strings.ToLower(n), "task") {
			found = true
		}
	}
	if !found {
		t.Errorf("esperaba una nota avisando que no hay Task logs; notas: %v", r.Notes)
	}
}
