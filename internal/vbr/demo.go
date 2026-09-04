package vbr

// demo.go — synthetic VBR for exercising the UI without a live server. The data
// is built relative to time.Now() so the 7-day window always has runs, and it
// deliberately includes every case the UI must handle:
//   - a week of nightly runs with the Load: line (per-stage %),
//   - a job failing NOW with retries (reliability headline),
//   - an SOBR offload session (must be filtered out of the analysis/selector),
//   - a job pointing at a repository the REST does not list (placeholder node),
//   - a managed server that is Unavailable, hosting a repository.
// Names are generic; nothing here comes from a real environment.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func demoResponse(path string) json.RawMessage {
	path = strings.SplitN(path, "?", 2)[0]
	switch {
	case strings.HasSuffix(path, "/taskSessions"):
		return demoTaskSessions(path)
	case strings.HasSuffix(path, "/logs"):
		return demoLogs(path)
	case strings.HasSuffix(path, "/proxies"):
		return json.RawMessage(demoProxies)
	case strings.HasSuffix(path, "/repositories"):
		return json.RawMessage(demoRepos)
	case strings.HasSuffix(path, "/scaleOutRepositories"):
		return json.RawMessage(demoSobrs)
	case strings.HasSuffix(path, "/repositories/states"):
		return json.RawMessage(demoRepoStates)
	case strings.HasSuffix(path, "/managedServers"):
		return json.RawMessage(demoManaged)
	case strings.HasSuffix(path, "/jobs"):
		return json.RawMessage(demoJobs)
	case strings.HasSuffix(path, "/sessions"):
		return demoSessions()
	default:
		return json.RawMessage(`{"data":[]}`)
	}
}

const demoManaged = `{"data":[
	{"id":"srv-vbr","name":"vbr01.demo.local","type":"WindowsHost","isBackupServer":true,"isDefaultMountServer":true,"status":"Available"},
	{"id":"srv-win","name":"proxy01.demo.local","type":"WindowsHost","status":"Available"},
	{"id":"srv-lin","name":"proxy02.demo.local","type":"LinuxHost","status":"Available"},
	{"id":"srv-repo-lin","name":"hardened01.demo.local","type":"LinuxHost","status":"Unavailable"},
	{"id":"srv-mount","name":"mount01.demo.local","type":"WindowsHost","status":"Available"}
]}`

const demoProxies = `{"data":[
	{"id":"prx-win","name":"proxy01.demo.local","type":"ViProxy","hostId":"srv-win","server":{"hostId":"srv-win","maxTaskCount":4,"transportMode":"auto"}},
	{"id":"prx-lin","name":"proxy02.demo.local","type":"ViProxy","hostId":"srv-lin","server":{"hostId":"srv-lin","maxTaskCount":16,"transportMode":"auto"}}
]}`

const demoRepos = `{"data":[
	{"id":"repo-refs","name":"Main Repository","type":"WinLocal","hostId":"srv-win","repository":{"path":"E:\\Backups","taskLimitEnabled":true,"maxTaskCount":4},"mountServer":{"windows":{"mountServerId":"srv-mount"}}},
	{"id":"repo-xfs","name":"Hardened Repository","type":"LinuxHardened","hostId":"srv-repo-lin","repository":{"path":"/mnt/backups","taskLimitEnabled":true,"maxTaskCount":8},"mountServer":{"linux":{"mountServerId":"srv-mount"}}},
	{"id":"repo-s3","name":"Object Storage","type":"AmazonS3","bucket":{"immutability":{"isEnabled":true}},"mountServer":{"windows":{"mountServerId":"srv-vbr"}}}
]}`

const demoSobrs = `{"data":[
	{"id":"repo-sobr","name":"Scale-Out Repository","type":"ScaleOut"}
]}`

const demoRepoStates = `{"data":[
	{"id":"repo-refs","capacityGB":2000,"freeGB":80},
	{"id":"repo-xfs","capacityGB":4000,"freeGB":2600}
]}`

// demoJobs: job-vms writes to repo-refs pinned to prx-win; job-sql points at a
// repository the REST does not list (placeholder node in the topology); job-copy
// goes to the SOBR with automatic proxy.
const demoJobs = `{"data":[
	{"id":"job-vms","name":"VMware - Demo VMs","type":"VSphereBackup","isDisabled":false,
	 "storage":{"backupRepositoryId":"repo-refs","backupProxies":{"autoSelectEnabled":false,"proxyIds":["prx-win"]}}},
	{"id":"job-sql","name":"VMware - SQL","type":"VSphereBackup","isDisabled":false,
	 "storage":{"backupRepositoryId":"repo-ghost","backupProxies":{"autoSelectEnabled":true,"proxyIds":[]}}},
	{"id":"job-copy","name":"Copy to Scale-Out","type":"BackupCopy","isDisabled":false,
	 "storage":{"backupRepositoryId":"repo-sobr","backupProxies":{"autoSelectEnabled":true,"proxyIds":[]}}}
]}`

// day returns a timestamp d days back at hh:mm, in the local zone RFC3339-ish
// format the REST uses.
func day(d, hh, mm int) string {
	t := time.Now().AddDate(0, 0, -d)
	// The fractional seconds matter: the analysis date parser relies on them to
	// strip the timezone offset, exactly like the real REST payloads carry them.
	return time.Date(t.Year(), t.Month(), t.Day(), hh, mm, 0, 0, time.Local).Format("2006-01-02T15:04:05.000000-07:00")
}

func sessJSON(id, name, jobID, typ, start, end, result, msg string) string {
	endField := ""
	if end != "" {
		endField = fmt.Sprintf(`"endTime":%q,`, end)
	}
	return fmt.Sprintf(`{"id":%q,"name":%q,"jobId":%q,"sessionType":%q,"creationTime":%q,%s"result":{"result":%q,"message":%q}}`,
		id, name, jobID, typ, start, endField, result, msg)
}

// demoSessions: newest first, like the REST returns them.
func demoSessions() json.RawMessage {
	var out []string
	// job-sql failing NOW: three runs today with the same error.
	for i, hm := range [][2]int{{3, 36}, {3, 24}, {3, 12}} {
		out = append(out, sessJSON(fmt.Sprintf("s-sqlf-%d", i), "VMware - SQL", "job-sql", "BackupJob",
			day(0, hm[0], hm[1]), day(0, hm[0], hm[1]+8), "Failed", "Error: No route to host"))
	}
	// An offload session that must NOT show up anywhere.
	out = append(out, sessJSON("s-off-0", "Scale-Out Repository Offload", "job-off", "SobrOffload",
		day(0, 1, 0), day(0, 1, 20), "Success", "Success"))
	// job-vms: nightly successful runs at 22:00 for a week (newest first).
	for d := 0; d < 7; d++ {
		out = append(out, sessJSON(fmt.Sprintf("s-vms-%d", d), "VMware - Demo VMs (Incremental)", "job-vms", "BackupJob",
			day(d+1, 22, 0), day(d+1, 22, 21), "Success", "Success"))
	}
	// job-copy: two runs, one older failure whose message is a task title.
	out = append(out, sessJSON("s-copy-0", "Copy to Scale-Out", "job-copy", "BackupCopy",
		day(2, 4, 0), day(2, 4, 45), "Success", "Success"))
	out = append(out, sessJSON("s-copy-f", "Copy to Scale-Out", "job-copy", "BackupCopy",
		day(4, 4, 0), day(4, 4, 5), "Failed", "Processing FileServer01"))
	return json.RawMessage(`{"data":[` + strings.Join(out, ",") + `]}`)
}

// demoTaskSessions: two VMs per successful vms run, one per copy run.
func demoTaskSessions(path string) json.RawMessage {
	id := demoSessionID(path)
	switch {
	case strings.HasPrefix(id, "s-vms-"):
		return json.RawMessage(`{"data":[
			{"name":"vm-app01","repositoryId":"repo-refs","algorithm":"Increment","result":{"result":"Success"},
			 "progress":{"bottleneck":"Target","duration":"00:20:15","processingRate":"180 MB","processedSize":214748364800,"readSize":32212254720,"transferredSize":11811160064}},
			{"name":"vm-db01","repositoryId":"repo-refs","algorithm":"Increment","result":{"result":"Success"},
			 "progress":{"bottleneck":"Target","duration":"00:18:40","processingRate":"150 MB","processedSize":107374182400,"readSize":21474836480,"transferredSize":8589934592}}
		]}`)
	case strings.HasPrefix(id, "s-copy-0"):
		return json.RawMessage(`{"data":[
			{"name":"FileServer01","repositoryId":"repo-sobr","algorithm":"Full","result":{"result":"Success"},
			 "progress":{"bottleneck":"Network","duration":"00:45:00","processingRate":"95 MB","processedSize":322122547200,"readSize":257698037760,"transferredSize":214748364800}}
		]}`)
	default:
		return json.RawMessage(`{"data":[]}`)
	}
}

// demoLogs: the real endpoint wraps records, and the Load: line carries the
// per-stage percentages the analysis parses.
func demoLogs(path string) json.RawMessage {
	id := demoSessionID(path)
	switch {
	case strings.HasPrefix(id, "s-vms-"):
		// d de "s-vms-<d>": la corrida fue hace d+1 dias a las 22:00. El registro
		// de espera lleva su intervalo real (9 min en cola por slots).
		d := 0
		fmt.Sscanf(id, "s-vms-%d", &d)
		return json.RawMessage(fmt.Sprintf(`{"totalRecords":3,"records":[
			{"title":"Resource not ready: backup repository","startTime":%q,"updateTime":%q},
			{"title":"Load: Source 34%% > Proxy 41%% > Network 22%% > Target 96%%","description":""},
			{"title":"Primary bottleneck: Target","description":""}
		]}`, day(d+1, 22, 0), day(d+1, 22, 9)))
	case strings.HasPrefix(id, "s-copy-0"):
		return json.RawMessage(`{"totalRecords":2,"records":[
			{"title":"Load: Source 25% > Proxy 30% > Network 88% > Target 45%","description":""},
			{"title":"Primary bottleneck: Network","description":""}
		]}`)
	default:
		return json.RawMessage(`{"totalRecords":0,"records":[]}`)
	}
}

// demoSessionID: "v1/sessions/<id>/taskSessions" -> "<id>".
func demoSessionID(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 3 {
		return parts[len(parts)-2]
	}
	return ""
}
