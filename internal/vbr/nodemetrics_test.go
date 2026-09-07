package vbr

// Parsing of the Node Exporter exposition that v13.1+ appliances serve.

import (
	"bufio"
	"strings"
	"testing"
)

func TestParseNodeMetrics(t *testing.T) {
	body := `# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.
node_cpu_seconds_total{cpu="0",mode="idle"} 1000.5
node_cpu_seconds_total{cpu="0",mode="user"} 22.1
node_cpu_seconds_total{cpu="1",mode="idle"} 999.2
node_cpu_seconds_total{cpu="2",mode="idle"} 998.0
node_cpu_seconds_total{cpu="3",mode="idle"} 997.7
# HELP node_memory_MemTotal_bytes Memory information field MemTotal_bytes.
node_memory_MemTotal_bytes 8.201818112e+09
`
	cores, ram, err := ParseNodeMetrics(bufio.NewScanner(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	if cores != 4 {
		t.Errorf("cores: got %d, want 4 (distinct cpu labels)", cores)
	}
	// 8.2e9 bytes ≈ 7.64 GiB -> rounds to 8.
	if ram != 8 {
		t.Errorf("ram: got %d, want 8 (rounded to the nominal size)", ram)
	}
}

func TestAutoResCachesFailures(t *testing.T) {
	s := &Session{}
	calls := 0
	probe := func(string) (int, int, bool) { calls++; return 0, 0, false }
	if _, ok := s.AutoRes("h1", probe); ok {
		t.Fatal("failed probe must report no data")
	}
	if _, ok := s.AutoRes("h1", probe); ok || calls != 1 {
		t.Fatalf("a failed host must not be probed again (calls=%d)", calls)
	}
	okProbe := func(string) (int, int, bool) { calls++; return 8, 16, true }
	if r, ok := s.AutoRes("h2", okProbe); !ok || r.Cores != 8 || r.RamGB != 16 {
		t.Fatalf("got %+v ok=%v", r, ok)
	}
	if r, ok := s.AutoRes("h2", okProbe); !ok || r.Cores != 8 || calls != 2 {
		t.Fatalf("cached result expected (calls=%d, r=%+v)", calls, r)
	}
}
