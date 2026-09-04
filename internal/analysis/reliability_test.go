package analysis

// Field case (small lab, 2026-08-28): "Backup Job 2" failed at 03:00 and on its
// three retries, all with "No route to host", because the host of its target
// repository was down. The verdict reported "Source is the recurring bottleneck"
// from data three weeks old and never mentioned the failures.

import (
	"fmt"
	"strings"
	"testing"
)

// sess: a session as the REST API returns it (newest first in the slice).
func sess(jobID, name, day, hhmm, result, msg string) map[string]any {
	return map[string]any{
		"jobId": jobID, "name": name, "sessionType": "BackupJob",
		"creationTime": day + "T" + hhmm + ":00",
		"endTime":      day + "T" + hhmm + ":30",
		"result":       map[string]any{"result": result, "message": msg},
	}
}

func TestFailuresGroupedAndFlaggedAsNow(t *testing.T) {
	// Newest first, exactly as the analysis reads them.
	in := []map[string]any{
		sess("j2", "Backup Job 2", "2026-08-28", "03:36", "Failed", "Error: No route to host"),
		sess("j2", "Backup Job 2", "2026-08-28", "03:24", "Failed", "Error: No route to host"),
		sess("j2", "Backup Job 2", "2026-08-28", "03:12", "Failed", "Error: No route to host"),
		sess("j2", "Backup Job 2", "2026-08-28", "03:00", "Failed", "Error: No route to host"),
		sess("j1", "Backup Job 1 (Full)", "2026-08-05", "12:58", "Success", "Success"),
	}
	rel := FailuresOf(in)
	if rel.FailedRuns != 4 {
		t.Fatalf("failed runs: got %d, want 4", rel.FailedRuns)
	}
	if len(rel.Failures) != 1 {
		t.Fatalf("the four failures share a message, so they group into one: got %d", len(rel.Failures))
	}
	f := rel.Failures[0]
	if f.Count != 4 || f.Message != "Error: No route to host" {
		t.Errorf("group: got %d × %q", f.Count, f.Message)
	}
	if f.JobName != "Backup Job 2" {
		t.Errorf("job name: got %q", f.JobName)
	}
	if !f.Now {
		t.Error("the job's most recent run is one of these failures, so Now must be set")
	}
}

// A job that failed earlier but whose latest run succeeded is NOT failing now.
func TestFailureNotNowWhenLatestRunSucceeded(t *testing.T) {
	in := []map[string]any{
		sess("j1", "Daily VMs", "2026-08-28", "02:00", "Success", "Success"),
		sess("j1", "Daily VMs", "2026-08-27", "02:00", "Failed", "Error: timeout"),
	}
	rel := FailuresOf(in)
	if rel.FailedRuns != 1 {
		t.Fatalf("failed runs: got %d, want 1", rel.FailedRuns)
	}
	if rel.Failures[0].Now {
		t.Error("the latest run succeeded, so the job is not failing now")
	}
}

// Runs failing now outrank the bottleneck: they take over the headline and the
// first action.
func TestReliabilityOutranksBottleneck(t *testing.T) {
	var recs []Record
	for d := 5; d <= 10; d++ {
		recs = append(recs, rec("j1", fmt.Sprintf("2026-08-%02d", d), "12:58", "13:30", 16*gib, "Source", []string{"r1"}, nil))
	}
	a := BuildAssessment(recs, 12, names, names)
	if a.HeadlineCode != "env.bound" {
		t.Fatalf("precondition: got %s, want env.bound", a.HeadlineCode)
	}
	before := len(a.Actions)

	a.AddReliability(Reliability{
		FailedRuns: 4,
		Failures: []Failure{{JobID: "j2", JobName: "Backup Job 2", Message: "Error: No route to host",
			Count: 4, Now: true}},
		DownHosts: []DownHost{{Name: "172.16.0.102", Status: "Unavailable", Repos: []string{"NAS repo iscsi"}}},
	})

	if a.Severity != "critical" {
		t.Errorf("severity: got %s, want critical", a.Severity)
	}
	if a.HeadlineCode != "env.failing" {
		t.Fatalf("the headline must be about the failure, got %s", a.HeadlineCode)
	}
	if a.Actions[0].Code != "act.envFailing" {
		t.Fatalf("first action: got %s, want act.envFailing", a.Actions[0].Code)
	}
	if !hasEnvAction(a, "act.envHostDown") {
		t.Errorf("expected the down host that explains it; actions: %v", codes(a))
	}
	// The performance advice is kept, just below.
	if !hasEnvAction(a, "act.envSource") {
		t.Errorf("the bottleneck advice must not be dropped; actions: %v", codes(a))
	}
	if len(a.Actions) <= before {
		t.Errorf("actions: got %d, want more than the %d it had", len(a.Actions), before)
	}
}

// Past failures with everything green now: reported, but they do not hijack the
// headline.
func TestPastFailuresDoNotHijackHeadline(t *testing.T) {
	var recs []Record
	for d := 5; d <= 10; d++ {
		recs = append(recs, rec("j1", fmt.Sprintf("2026-08-%02d", d), "12:58", "13:30", 16*gib, "Source", []string{"r1"}, nil))
	}
	a := BuildAssessment(recs, 12, names, names)
	a.AddReliability(Reliability{FailedRuns: 2,
		Failures: []Failure{{JobName: "Daily VMs", Message: "Error: timeout", Count: 2}}})
	if a.HeadlineCode != "env.bound" {
		t.Errorf("headline: got %s, want env.bound (nothing is failing now)", a.HeadlineCode)
	}
	if a.Severity == "critical" {
		t.Error("severity must not be critical for failures that already recovered")
	}
	if !hasEnvAction(a, "act.envPastFailures") {
		t.Errorf("expected act.envPastFailures; actions: %v", codes(a))
	}
}

// Nothing failed: the verdict is untouched.
func TestNoFailuresLeavesVerdictAlone(t *testing.T) {
	var recs []Record
	for d := 5; d <= 10; d++ {
		recs = append(recs, rec("j1", fmt.Sprintf("2026-08-%02d", d), "12:58", "13:30", 16*gib, "Source", []string{"r1"}, nil))
	}
	a := BuildAssessment(recs, 12, names, names)
	head, sev, n := a.HeadlineCode, a.Severity, len(a.Actions)
	a.AddReliability(Reliability{})
	if a.HeadlineCode != head || a.Severity != sev || len(a.Actions) != n {
		t.Errorf("verdict changed with no failures: %s/%s/%d", a.HeadlineCode, a.Severity, len(a.Actions))
	}
}

// Veeam sometimes leaves the last task title ("Processing File02") in
// result.message instead of the error. Presenting that as the reason is
// misleading — field case from the large lab.
func TestFailureMessageThatIsNotAnError(t *testing.T) {
	in := []map[string]any{
		sess("j1", "VMware - File Servers", "2026-08-26", "22:32", "Failed", "Processing File02"),
		sess("j2", "NAS Backup", "2026-08-27", "06:57", "Failed", "Error: No route to host"),
	}
	rel := FailuresOf(in)
	if len(rel.Failures) != 2 {
		t.Fatalf("groups: got %d, want 2", len(rel.Failures))
	}
	for _, f := range rel.Failures {
		switch f.JobID {
		case "j1":
			if f.Message == "Processing File02" {
				t.Error("a task title must not be presented as the failure reason")
			}
			if want := `failed at "Processing File02"`; !containsStr(f.Message, want) {
				t.Errorf("message: got %q, want it to contain %q", f.Message, want)
			}
		case "j2":
			if f.Message != "Error: No route to host" {
				t.Errorf("a real error must pass through untouched: %q", f.Message)
			}
		}
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && strings.Contains(s, sub)
}

// Entra ID restore sessions carry a fresh jobId per run: grouping by jobId
// duplicated the row. Grouping is by job NAME + message.
func TestFailuresGroupByNameNotJobID(t *testing.T) {
	in := []map[string]any{
		sess("id-1", "Entra ID Tenant Backup", "2026-09-03", "10:00", "Failed", "Error: restore failed"),
		sess("id-2", "Entra ID Tenant Backup", "2026-09-02", "10:00", "Failed", "Error: restore failed"),
	}
	rel := FailuresOf(in)
	if len(rel.Failures) != 1 {
		t.Fatalf("same name+message must group into one row: got %d", len(rel.Failures))
	}
	if rel.Failures[0].Count != 2 {
		t.Errorf("count: got %d, want 2", rel.Failures[0].Count)
	}
}
