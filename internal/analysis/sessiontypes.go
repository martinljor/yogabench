package analysis

// sessiontypes.go — session classification against the OFFICIAL ESessionType
// enumeration of the VBR 13 REST API reference (1.3-rev2), instead of guessing
// from whatever each lab happens to run. Three classes:
//
//   backupRunTypes — data-mover runs the performance analysis is about: they
//     move production data through proxy/repository and populate the selector,
//     the assessment and the per-job verdict. Plugin platforms (Nutanix AHV,
//     Proxmox, HPE Morpheus, ...) arrive as PlatformBackupJob/PlatformSnapshot*.
//   systemRunTypes — VBR housekeeping: never analyzed, never a reliability row.
//   restore-ish (see isReliabilityRun) — restores, failovers and SureBackup are
//     not capacity subjects, but a FAILED one is a reliability finding.
//
// Types absent from all sets (new or renamed values) fall back to the old
// keyword hints, and the "analysis input" log line reports them in skipped:
// counts so the sets can be extended with evidence.

import "strings"

// backupRunTypes: ESessionType values that move backup data.
var backupRunTypes = map[string]bool{
	"backupjob":               true, // VMware / Hyper-V VM backup
	"replicajob":              true,
	"backupcopyjob":           true,
	"backupcopyjobparent":     true,
	"copybackup":              true, // VM copy
	"agentbackup":             true,
	"endpointbackup":          true,
	"platformbackupjob":       true, // plugin platforms: AHV, Proxmox, Morpheus, ...
	"platformsnapshotjob":     true,
	"platformsnapshotcopyjob": true,
	"unstructureddatabackup":  true, // NAS / object storage (not in the enum dump; kept for the hint fallback parity)
	"nasbackup":               true,
	"filebackup":              true,
}

// systemRunTypes: housekeeping ESessionType values — noise for every analysis.
var systemRunTypes = map[string]bool{
	"infrastructure": true, "automation": true, "configurationbackup": true,
	"configurationresynchronize": true, "repositorymaintenance": true,
	"repositoryevacuate": true, "infrastructureitemdeletion": true,
	"malwaredetection": true, "securitycomplianceanalyzer": true,
	"logsexport": true, "testcredentials": true, "volumesdiscover": true,
	"deletebackup": true, "backgroundoperation": true, "agentdiscovery": true,
	"agentpolicy": true, "agentoperationpurgecache": true,
	"applianceupdatesinstall": true, "unstructureddatabrowse": true,
	"unstructureddownloadmeta": true, "entraidrescanrepository": true,
	"hostcomponentsupdate": true, "filebackuphealthcheck": true,
	"surebackupcontentscan": true, "archivebackup": true, "archiverehydration": true,
	"archivesync": true, "archivecopy": true, "archivefreezing": true,
	"retrievebackup": true, "archivetierdownload": true, "directbackupsync": true,
	"checkpointremoval": true, "retention": true, "platformservicejob": true,
	"agentmanagement": true, "encryptionanalysis": true,
	"veeamupdatersettingssync": true, "storagediscovery": true,
	"storagemonitoring": true, "backupcachesync": true, "pervmmovebackup": true,
	"pervmcopybackup": true, "filebackuparchiveretrievalprolonging": true,
	"haswitchover": true, "haclusteredit": true, "indexcollection": true,
	"publishbackupcontentviamount": true, "publishbackupcontentvianfs": true,
	"sqllogbackup": true, "oraclelogbackup": true, "postgresqllogbackup": true,
	"iriscbackup": true, "irisbackup": true,
}

// restoreHints: session types whose FAILURE matters (reliability) but that are
// not capacity subjects: restores, failovers, migrations, SureBackup checks.
var restoreHints = []string{"restore", "recovery", "failover", "failback", "migration", "surebackup", "quickmigration"}

func sessionTypeOf(sess map[string]any) string {
	t := str(sess["sessionType"])
	if t == "" {
		t = str(sess["type"])
	}
	return strings.ToLower(t)
}

// isRestoreRun: not a capacity subject, but a failed one is a reliability finding.
func isRestoreRun(sess map[string]any) bool {
	t := sessionTypeOf(sess)
	for _, h := range restoreHints {
		if strings.Contains(t, h) {
			return true
		}
	}
	return false
}

// isReliabilityRun: sessions whose failures the environment verdict reports.
func isReliabilityRun(sess map[string]any) bool {
	return isDataJob(sess) || isRestoreRun(sess)
}
