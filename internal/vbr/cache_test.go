package vbr

import (
	"encoding/json"
	"testing"
	"time"
)

// Only slow-changing endpoints are cached. Sessions must never be, or the analysis
// would report a stale window right after a run finishes.
func TestCacheable(t *testing.T) {
	cached := []string{
		"v1/jobs?limit=1000",
		"v1/backupInfrastructure/proxies?limit=1000",
		"v1/backupInfrastructure/repositories?limit=1000",
		"v1/backupInfrastructure/managedServers?limit=1000",
		"v1/backupInfrastructure/repositories/states",
	}
	notCached := []string{
		"v1/sessions?limit=2000&orderColumn=CreationTime&orderAsc=false",
		"v1/sessions/abc/taskSessions",
		"v1/sessions/abc/logs",
		"v1/sessions?limit=1&orderColumn=CreationTime&orderAsc=true",
	}
	for _, p := range cached {
		if !cacheable(p) {
			t.Errorf("%s should be cached", p)
		}
	}
	for _, p := range notCached {
		if cacheable(p) {
			t.Errorf("%s must NOT be cached (it changes on every run)", p)
		}
	}
}

func TestCacheHitAndExpiry(t *testing.T) {
	s := &Session{}
	body := json.RawMessage(`{"data":[]}`)
	if _, ok := s.cacheGet("v1/jobs?limit=1000"); ok {
		t.Fatal("empty cache returned a hit")
	}
	s.cachePut("v1/jobs?limit=1000", body)
	got, ok := s.cacheGet("v1/jobs?limit=1000")
	if !ok || string(got) != string(body) {
		t.Fatalf("expected a hit with the stored body, got %q ok=%v", got, ok)
	}
	if _, ok := s.cacheGet("v1/jobs?limit=500"); ok {
		t.Error("a different path must not hit")
	}
	// Expired entries are misses.
	s.cacheMu.Lock()
	s.cache["v1/jobs?limit=1000"] = cacheEntry{at: time.Now().Add(-cacheTTL - time.Second), body: body}
	s.cacheMu.Unlock()
	if _, ok := s.cacheGet("v1/jobs?limit=1000"); ok {
		t.Error("an entry older than the TTL must be a miss")
	}
}
