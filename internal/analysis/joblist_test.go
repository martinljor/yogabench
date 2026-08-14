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
