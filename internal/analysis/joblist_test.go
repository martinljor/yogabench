package analysis

// Veeam names a session "<job> (Incremental)". Using that as the job name made the
// analysis look like it only covered that one run — reported from the field.

import "testing"

func TestJobNameOf(t *testing.T) {
	cases := map[string]string{
		"VMware - Malware (Incremental)":           "VMware - Malware",
		"VMware - Domain Controller (Incremental)": "VMware - Domain Controller",
		"Backup Job 2 (Full)":                      "Backup Job 2",
		"Backup Job 1 (Increment)":                 "Backup Job 1",
		"Hyper-V - Windows/Linux (Synthetic Full)": "Hyper-V - Windows/Linux",
		"Daily VMs (Retry 2)":                      "Daily VMs",
		"VBR Managed Agents - Windows":             "VBR Managed Agents - Windows",
		// A job whose real name ends in parentheses must survive untouched: only the
		// known run-type suffixes are stripped.
		"Backup (Prod)":                  "Backup (Prod)",
		"Copy to DR (Site B)":            "Copy to DR (Site B)",
		"VMware - Malware (incremental)": "VMware - Malware",
	}
	for in, want := range cases {
		if got := JobNameOf(in); got != want {
			t.Errorf("JobNameOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The run is a Full if any of its tasks ran as Full: a job can mix modes per VM.
func TestRunAlgorithm(t *testing.T) {
	cases := []struct {
		name  string
		tasks []Task
		want  string
	}{
		{"all increment", []Task{{Algorithm: "Increment"}, {Algorithm: "Increment"}}, "Increment"},
		{"one full", []Task{{Algorithm: "Increment"}, {Algorithm: "Full"}}, "Full"},
		{"empty", []Task{{}, {}}, ""},
		{"no tasks", nil, ""},
	}
	for _, c := range cases {
		if got := runAlgorithm(c.tasks); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// SOBR system sessions (offload/tiering) flooded the job selector and inflated
// the assessment to "52 jobs" on a field lab: their names contain "Backup", so
// the job-hint match let them in.
func TestIsDataJobSkipsSobrSystemSessions(t *testing.T) {
	skip := []map[string]any{
		{"sessionType": "SobrOffload", "name": "Hardened Scale-Out Backup Repository Offload"},
		// Field case (v1/jobs listed 12 jobs, the assessment counted 46): the
		// offload sessions arrive with a sessionType that does not say "offload" —
		// the word only appears in the name. They must still be excluded.
		{"sessionType": "BackupJob", "name": "Hardened Scale-Out Backup Repository Offload"},
		// Field case: the offload sessions arrived with a sessionType that does not
		// say "offload" — only the name does. They must still be excluded.
		{"sessionType": "BackupJob", "name": "Hardened Scale-Out Backup Repository Offload"},
		{"sessionType": "BackupOffload", "name": "Repo Offload"},
		{"sessionType": "SobrTiering", "name": "Capacity Tiering Backup"},
		{"sessionType": "RepositoryRescan", "name": "Backup Repository Rescan"},
	}
	keep := []map[string]any{
		{"sessionType": "BackupJob", "name": "VMware - File Servers (Incremental)"},
		{"sessionType": "BackupCopy", "name": "Copy Backup"},
		{"sessionType": "ReplicaJob", "name": "VMware - Replicas"},
	}
	for _, x := range skip {
		if isDataJob(x) {
			t.Errorf("%v must be skipped", x["sessionType"])
		}
	}
	for _, x := range keep {
		if !isDataJob(x) {
			t.Errorf("%v must be kept", x["sessionType"])
		}
	}
}
