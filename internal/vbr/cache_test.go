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

// Single-flight: several parallel misses on the same path must produce ONE fetch.
// In the field two parallel v1/jobs fetches took 19 s each on the same payload.
func TestSingleFlightJoinsOneCall(t *testing.T) {
	s := &Session{}
	const path = "v1/jobs?limit=1000"
	call, lead := s.joinOrLead(path)
	if !lead {
		t.Fatal("the first caller must lead the fetch")
	}
	// Two more callers arrive while it is in flight: both wait on the same call.
	same, lead2 := s.joinOrLead(path)
	if lead2 || same != call {
		t.Fatalf("a second caller must join, not lead: lead=%v same=%v", lead2, same == call)
	}
	// A different path is independent.
	if _, leadOther := s.joinOrLead("v1/backupInfrastructure/proxies?limit=1000"); !leadOther {
		t.Error("another path must lead its own fetch")
	}

	got := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-call.done
			got <- string(call.body)
		}()
	}
	s.leadDone(path, call, json.RawMessage(`{"data":[1]}`), nil)
	for i := 0; i < 2; i++ {
		select {
		case v := <-got:
			if v != `{"data":[1]}` {
				t.Errorf("waiter got %q", v)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a waiter was never released")
		}
	}
	// Once finished the path is free again, so the next miss fetches fresh.
	if _, leadAgain := s.joinOrLead(path); !leadAgain {
		t.Error("after finishing, the next caller must lead")
	}
}
