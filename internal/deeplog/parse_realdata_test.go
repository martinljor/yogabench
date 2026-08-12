package deeplog

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestParseRealData corre solo si YOGA_DEEPLOG_DIR apunta a una carpeta de job de
// Veeam (Job.*.log + Task.*.log). No se commitean logs; valida el parser en campo.
func TestParseRealData(t *testing.T) {
	dir := os.Getenv("YOGA_DEEPLOG_DIR")
	if dir == "" {
		t.Skip("set YOGA_DEEPLOG_DIR to a Veeam job log folder")
	}
	jobs, _ := filepath.Glob(filepath.Join(dir, "Job.*.log"))
	if len(jobs) == 0 {
		t.Fatalf("no Job.*.log in %s", dir)
	}
	jobLog, err := os.ReadFile(jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	tasks, _ := filepath.Glob(filepath.Join(dir, "Task.*.log"))
	taskLogs := map[string]string{}
	for _, f := range tasks {
		vm := vmFromTaskFile(f)
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		taskLogs[vm] = string(b)
	}

	r := Parse(string(jobLog), taskLogs)

	t.Logf("transport=%q note=%q", r.Transport, r.TransportNote)
	if r.Aggregate != nil {
		t.Logf("aggregate Load: S%d P%d N%d T%d  primary=%s", r.Aggregate.Source, r.Aggregate.Proxy, r.Aggregate.Network, r.Aggregate.Target, r.Primary)
	}
	if r.Dedup != nil {
		t.Logf("dedup=%v compression=%v blockKB=%v", *r.Dedup, deref(r.Compression), deref(r.BlockSizeKB))
	}
	for _, v := range r.VMs {
		busy := "nil"
		if v.Busy != nil {
			busy = strconv.Itoa(v.Busy.Source) + "/" + strconv.Itoa(v.Busy.Proxy) + "/" + strconv.Itoa(v.Busy.Network) + "/" + strconv.Itoa(v.Busy.Target)
		}
		t.Logf("VM %-34s busy=%s primary=%s dur=%.0fs disks=%d", v.Name, busy, v.Primary, v.DurationSec, len(v.Disks))
	}

	if !strings.Contains(r.Transport, "nbd") {
		t.Errorf("expected nbd transport, got %q", r.Transport)
	}
	if r.Aggregate == nil || r.Aggregate.Source < 50 {
		t.Errorf("expected source-heavy aggregate, got %+v", r.Aggregate)
	}
	if len(r.VMs) == 0 {
		t.Errorf("no VMs parsed")
	}
	withBusy, withDur := 0, 0
	for _, v := range r.VMs {
		if v.Busy != nil {
			withBusy++
		}
		if v.DurationSec > 0 {
			withDur++
		}
	}
	if withBusy == 0 {
		t.Errorf("no per-VM Busy parsed")
	}
	if withDur == 0 {
		t.Errorf("no per-VM durations parsed")
	}
}

func vmFromTaskFile(f string) string {
	b := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "Task."), ".log")
	if i := strings.LastIndex(b, "."); i >= 0 {
		if _, err := strconv.Atoi(b[i+1:]); err == nil {
			b = b[:i]
		}
	}
	return b
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
