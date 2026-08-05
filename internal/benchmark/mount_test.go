package benchmark

import "testing"

// Reproduce el schema real de v13 (appliance): el mount server viene anidado en
// mountServer.linux.mountServerId. Antes se resolvia null; ahora debe encontrarlo.
func TestResolveRepoMountNestedV13(t *testing.T) {
	repo := map[string]any{
		"id": "r1", "name": "Default Backup Repository", "hostId": "vbr-id",
		"repository": map[string]any{"path": "/var/lib/veeam/backup"},
		"mountServer": map[string]any{
			"mountServerSettingsType": "linux",
			"linux": map[string]any{
				"mountServerId":    "vbr-id",
				"writeCacheFolder": "/var/lib/veeamdata/veeam/IRCache/",
			},
		},
	}
	managed := []map[string]any{{"id": "vbr-id", "name": "vbr", "type": "LinuxHost"}}

	ro := resolveRepo(repo, managed)
	if ro.Mount == nil {
		t.Fatal("mount server no resuelto (esperaba vbr-id)")
	}
	if ro.Mount.ID != "vbr-id" || ro.Mount.Name != "vbr" {
		t.Errorf("mount = %+v (esperaba id=vbr-id name=vbr)", ro.Mount)
	}
	if ro.Path != "/var/lib/veeam/backup" {
		t.Errorf("path = %q", ro.Path)
	}
}
