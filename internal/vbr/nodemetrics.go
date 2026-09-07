package vbr

// nodemetrics.go — best-effort scrape of the Node Exporter metrics that v13.1+
// appliances expose (Observability, VUL license): CPU count and total RAM, over
// a plain read-only HTTP GET. No credentials involved. Where the endpoint does
// not answer (older versions, Windows hosts, feature disabled, no VUL) the
// caller falls back to the manually entered resources.

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// nodeMetricsTimeout: the probe must never make the analysis feel slow — a host
// that does not expose metrics simply stays "no data".
const nodeMetricsTimeout = 3 * time.Second

// ScrapeNodeMetrics reads cores and RAM from a host's Node Exporter endpoint
// (port 9100), trying plain HTTP first (the exporter default) and HTTPS with a
// self-signed cert second.
func ScrapeNodeMetrics(host string) (cores, ramGB int, ok bool) {
	for _, scheme := range []string{"http", "https"} {
		if c, r, err := scrapeOnce(scheme + "://" + host + ":9100/metrics"); err == nil && c > 0 && r > 0 {
			return c, r, true
		}
	}
	return 0, 0, false
}

func scrapeOnce(url string) (cores, ramGB int, err error) {
	client := &http.Client{
		Timeout:   nodeMetricsTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return ParseNodeMetrics(bufio.NewScanner(resp.Body))
}

// ParseNodeMetrics extracts the CPU count (distinct cpu="N" labels of
// node_cpu_seconds_total) and total RAM (node_memory_MemTotal_bytes) from a
// Prometheus text exposition.
func ParseNodeMetrics(sc *bufio.Scanner) (cores, ramGB int, err error) {
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	cpus := map[string]bool{}
	var memBytes float64
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "node_cpu_seconds_total{") {
			if i := strings.Index(line, `cpu="`); i >= 0 {
				rest := line[i+5:]
				if j := strings.IndexByte(rest, '"'); j > 0 {
					cpus[rest[:j]] = true
				}
			}
		} else if strings.HasPrefix(line, "node_memory_MemTotal_bytes") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				memBytes, _ = strconv.ParseFloat(fields[1], 64)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	const gib = float64(1 << 30)
	// Round to the nearest GiB: exporters report usable memory slightly under
	// the nominal size (e.g. 7.8 GiB on an 8 GiB host).
	return len(cpus), int(memBytes/gib + 0.5), nil
}

// AutoRes: per-session cache of scraped host resources, so each appliance is
// probed once per session (a failed probe is cached too — no repeated 3 s
// timeouts on every analysis).
func (s *Session) AutoRes(host string, probe func(string) (int, int, bool)) (HostRes, bool) {
	s.mu.Lock()
	if s.autoRes == nil {
		s.autoRes = map[string]*HostRes{}
	}
	if r, seen := s.autoRes[host]; seen {
		s.mu.Unlock()
		if r == nil {
			return HostRes{}, false
		}
		return *r, true
	}
	s.mu.Unlock()

	cores, ram, ok := probe(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		s.autoRes[host] = nil
		return HostRes{}, false
	}
	r := HostRes{Cores: cores, RamGB: ram}
	s.autoRes[host] = &r
	return r, true
}
