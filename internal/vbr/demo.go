package vbr

import (
	"encoding/json"
	"strings"
)

// demoResponse devuelve una topologia de ejemplo (sin VBR real) para probar la
// consola. Ignora el query string y matchea por el final de la ruta.
func demoResponse(path string) json.RawMessage {
	path = strings.SplitN(path, "?", 2)[0]
	switch {
	case strings.HasSuffix(path, "/proxies"):
		return json.RawMessage(demoProxies)
	case strings.HasSuffix(path, "/repositories"):
		return json.RawMessage(demoRepos)
	case strings.HasSuffix(path, "/managedServers"):
		return json.RawMessage(demoManaged)
	default:
		return json.RawMessage(`{"data":[]}`)
	}
}

const demoProxies = `{"data":[
	{"id":"prx-win","name":"VMware Proxy 01","type":"vmware","os":"windows","hostId":"srv-win","server":{"maxTaskCount":4},"description":"Windows proxy - hot-add"},
	{"id":"prx-lin","name":"Linux Proxy 01","type":"vmware","os":"linux","hostId":"srv-lin","server":{"maxTaskCount":8},"description":"Linux proxy - NBD/hot-add"}
]}`

const demoRepos = `{"data":[
	{"id":"repo-refs","name":"Local ReFS Repo","type":"WinLocal","hostId":"srv-win","mountServerId":"srv-mount","repository":{"path":"E:\\Backups","taskLimitEnabled":true,"maxTaskCount":4}},
	{"id":"repo-xfs","name":"Linux Hardened Repo","type":"LinuxHardened","hostId":"srv-repo-lin","mountServerId":"srv-mount","repository":{"path":"/mnt/backups","taskLimitEnabled":false}}
]}`

const demoManaged = `{"data":[
	{"id":"srv-vbr","name":"VBR01 (Backup Server)","type":"VbrServer"},
	{"id":"srv-win","name":"WIN-PROXY01","type":"WindowsHost"},
	{"id":"srv-lin","name":"LIN-PROXY01","type":"LinuxHost"},
	{"id":"srv-mount","name":"WIN-MOUNT01","type":"WindowsHost"},
	{"id":"srv-repo-lin","name":"HARDENED-REPO01","type":"LinuxHost"}
]}`
