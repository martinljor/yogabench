package analysis

// reliability.go — the environment verdict used to be about performance only:
// failed sessions were dropped before the records were built, so a lab whose job
// had failed four times that morning still got "Source is the recurring
// bottleneck" computed from data three weeks old. A verdict that does not mention
// that the backups are failing is worse than no verdict.
//
// Nothing here costs extra REST calls: the failed sessions come in the same
// response the analysis already reads, and the host status comes from the cached
// infrastructure endpoints.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"yogabench/internal/vbr"
)

// Failure: failed runs of a job in the window, grouped by error message.
type Failure struct {
	JobID   string `json:"jobId"`
	JobName string `json:"jobName"`
	Message string `json:"message"`
	Count   int    `json:"count"`
	LastAt  string `json:"lastAt"`
	// Now: the job's most recent run in the window is one of these failures, so
	// the job is failing right now rather than having failed at some point.
	Now bool `json:"now"`
}

// DownHost: a managed server that is not available, and the repositories it
// hosts — that pairing is usually the cause of the failures.
type DownHost struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Repos  []string `json:"repos,omitempty"`
}

// Reliability: what the window says about runs failing, independent of speed.
type Reliability struct {
	FailedRuns int        `json:"failedRuns"`
	Failures   []Failure  `json:"failures,omitempty"`
	DownHosts  []DownHost `json:"downHosts,omitempty"`
}

// FailuresOf groups the failed sessions of the window. sess must be ordered from
// newest to oldest, so the first session seen for a job tells whether that job is
// failing right now.
func FailuresOf(sess []map[string]any) Reliability {
	var rel Reliability
	group := map[string]*Failure{}
	newest := map[string]bool{}
	var order []string
	for _, x := range sess {
		jobID := firstNonEmpty(str(x["jobId"]), str(x["name"]))
		failed := resultOf(x) == "Failed"
		first := !newest[jobID]
		newest[jobID] = true
		if !failed {
			continue
		}
		rel.FailedRuns++
		msg := strings.TrimSpace(messageOf(x))
		if msg == "" {
			msg = "Failed"
		} else if !looksLikeError(msg) {
			// Veeam sometimes puts the last task title ("Processing File02") in
			// result.message instead of the error. Presenting that as the reason
			// is misleading; say what it actually is.
			msg = fmt.Sprintf("failed at %q — the session summary does not carry the error text", msg)
		}
		// Agrupamos por NOMBRE + mensaje: hay sesiones (restores de Entra ID, por
		// ejemplo) que traen un jobId distinto en cada corrida y duplicaban la fila.
		key := JobNameOf(str(x["name"])) + "|" + msg
		f := group[key]
		if f == nil {
			f = &Failure{JobID: jobID, JobName: JobNameOf(str(x["name"])), Message: msg,
				LastAt: str(x["creationTime"])}
			group[key] = f
			order = append(order, key)
		}
		f.Count++
		if first {
			f.Now = true // the job's latest run in the window is this failure
		}
	}
	for _, k := range order {
		rel.Failures = append(rel.Failures, *group[k])
	}
	// Currently-failing first, then by how many times it failed.
	sort.SliceStable(rel.Failures, func(i, j int) bool {
		if rel.Failures[i].Now != rel.Failures[j].Now {
			return rel.Failures[i].Now
		}
		return rel.Failures[i].Count > rel.Failures[j].Count
	})
	return rel
}

// looksLikeError: whether the session message reads as an error, as opposed to a
// task title Veeam sometimes leaves there.
func looksLikeError(msg string) bool {
	m := strings.ToLower(msg)
	for _, w := range []string{"error", "fail", "unable", "cannot", "can't", "timeout", "timed out", "no route", "denied", "refused", "no space", "not enough", "unavailable", "warning"} {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

// DownHostsOf: managed servers that are not available, with the repositories they
// host. Both endpoints are cached, so this adds no request.
func DownHostsOf(ctx context.Context, s *vbr.Session) []DownHost {
	repos := map[string][]string{} // hostId -> repository names
	for _, r := range allRepositories(ctx, s) {
		if h := str(r["hostId"]); h != "" {
			repos[h] = append(repos[h], strOr(r["name"], "repository"))
		}
	}
	var out []DownHost
	for _, m := range getItems(ctx, s, "v1/backupInfrastructure/managedServers?limit=1000") {
		st := str(m["status"])
		if st == "" || strings.EqualFold(st, "Available") {
			continue
		}
		out = append(out, DownHost{Name: strOr(m["name"], "server"), Status: st, Repos: repos[str(m["id"])]})
	}
	return out
}

// AddReliability folds the reliability findings into the environment verdict.
// Runs failing NOW outrank any bottleneck: a slow backup is a problem, a backup
// that does not complete is a different kind of problem.
func (a *Assessment) AddReliability(rel Reliability) {
	if a == nil {
		return
	}
	a.Reliability = rel
	// Success rate: the records only hold completed runs; the failures arrive
	// here. attempts = completed + failed.
	if att := a.Runs + rel.FailedRuns; att > 0 {
		a.SuccessPct = int(float64(a.Runs)/float64(att)*100 + 0.5)
	}
	var failingNow []Failure
	for _, f := range rel.Failures {
		if f.Now {
			failingNow = append(failingNow, f)
		}
	}

	var acts []Action
	add := func(impact, code, text string, params map[string]any) {
		acts = append(acts, Action{Impact: impact, Code: code, Text: text, Params: params, Source: "observed"})
	}

	if len(failingNow) > 0 {
		top := failingNow[0]
		names := failingNow[0].JobName
		if len(failingNow) > 1 {
			names = fmt.Sprintf("%s +%d", names, len(failingNow)-1)
		}
		a.Severity = "critical"
		a.HeadlineCode = "env.failing"
		a.Headline = fmt.Sprintf("%s: the most recent run failed — %s", names, top.Message)
		a.HeadlineParams = map[string]any{"jobs": len(failingNow), "job": names, "msg": top.Message}
		add("high", "act.envFailing",
			fmt.Sprintf("%s failed %d time(s) in the window and its latest run is one of them: %s. Fix this before looking at throughput — the bottleneck below is measured on the runs that did complete.",
				top.JobName, top.Count, top.Message),
			map[string]any{"job": top.JobName, "count": top.Count, "msg": top.Message, "jobs": len(failingNow)})
	} else if rel.FailedRuns > 0 {
		add("medium", "act.envPastFailures",
			fmt.Sprintf("%d run(s) failed in the window, though the latest run of every job completed. Worth a look if it repeats.", rel.FailedRuns),
			map[string]any{"runs": rel.FailedRuns})
	}

	// A host that is down is usually the cause, so it goes right next to it.
	for _, h := range rel.DownHosts {
		if len(h.Repos) > 0 {
			add("high", "act.envHostDown",
				fmt.Sprintf("%s is %s and hosts %s: any job writing there cannot complete.", h.Name, h.Status, strings.Join(h.Repos, ", ")),
				map[string]any{"host": h.Name, "status": h.Status, "repos": strings.Join(h.Repos, ", ")})
		} else {
			add("medium", "act.envHostDownBare",
				fmt.Sprintf("%s is %s.", h.Name, h.Status),
				map[string]any{"host": h.Name, "status": h.Status})
		}
	}
	if len(acts) == 0 {
		return
	}
	a.Actions = rank(append(acts, a.Actions...))
}
