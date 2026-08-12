package deeplog

// fetch.go: obtiene los logs del job desde el OS del VBR. Windows -> SMB2 al share
// administrativo C$ (pure-Go, binario unico). Linux appliance -> pendiente (el
// appliance hardened de prod no permite SSH; se soporta el VBR Windows). Las
// credenciales viven solo aca (server-side) y NUNCA se loguean.

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

const (
	winLogBase   = "ProgramData/Veeam/Backup" // bajo C:\
	maxLogBytes  = int64(16) << 20            // cap por archivo (Task logs pueden ser grandes)
	smbDialAfter = 20 * time.Second
)

// FetchWindows lee Job.*.log + Task.*.log de C:\ProgramData\Veeam\Backup\<job>\
// por SMB2. domain puede ir vacio (cuenta local) o "DOMINIO".
func FetchWindows(host, user, pass, domain, jobName string) (jobLog string, taskLogs map[string]string, err error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "445"), 15*time.Second)
	if err != nil {
		return "", nil, fmt.Errorf("cannot reach %s:445 (SMB) - %v", host, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(smbDialAfter))

	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: pass, Domain: domain}}
	sess, err := d.Dial(conn)
	if err != nil {
		return "", nil, fmt.Errorf("SMB auth to %s failed (check the Windows admin credentials) - %v", host, err)
	}
	defer sess.Logoff()

	share, err := sess.Mount(`C$`)
	if err != nil {
		return "", nil, fmt.Errorf("cannot mount \\\\%s\\C$ (need local admin) - %v", host, err)
	}
	defer share.Umount()

	folder, err := findJobFolder(share, jobName)
	if err != nil {
		return "", nil, err
	}
	dir := winLogBase + "/" + folder

	names, err := listDir(share, dir)
	if err != nil {
		return "", nil, fmt.Errorf("cannot list %s - %v", dir, err)
	}
	taskLogs = map[string]string{}
	var jobCandidate string
	for _, name := range names {
		low := strings.ToLower(name)
		if !strings.HasSuffix(low, ".log") {
			continue
		}
		switch {
		case strings.HasPrefix(low, "job."):
			// preferir el ".backup.log" si hay varios
			if jobCandidate == "" || strings.Contains(low, ".backup.log") {
				jobCandidate = name
			}
		case strings.HasPrefix(low, "task."):
			if b, e := readFileCapped(share, dir+"/"+name); e == nil {
				taskLogs[vmFromTaskName(name)] = b
			}
		}
	}
	if jobCandidate == "" {
		return "", nil, fmt.Errorf("no Job.*.log found in %s", dir)
	}
	jobLog, err = readFileCapped(share, dir+"/"+jobCandidate)
	if err != nil {
		return "", nil, fmt.Errorf("cannot read %s - %v", jobCandidate, err)
	}
	return jobLog, taskLogs, nil
}

// findJobFolder busca la carpeta del job (Veeam la nombra como el job, a veces
// sanitizada). Match exacto case-insensitive; si no, la que contenga el nombre.
func findJobFolder(share *smb2.Share, jobName string) (string, error) {
	names, err := listDir(share, winLogBase)
	if err != nil {
		return "", fmt.Errorf("cannot list %s (is this the VBR server?) - %v", winLogBase, err)
	}
	// Normalizamos AMBOS lados por el mismo saneo (espacios/chars -> "_") para que
	// "AWS SOBR" matchee la carpeta "AWS_SOBR".
	norm := func(x string) string { return strings.ToLower(sanitize(strings.TrimSpace(x))) }
	want := norm(jobName)
	var contains string
	for _, n := range names {
		nn := norm(n)
		if nn == want {
			return n, nil
		}
		if contains == "" && (strings.Contains(nn, want) || strings.Contains(want, nn)) {
			contains = n
		}
	}
	if contains != "" {
		return contains, nil
	}
	return "", fmt.Errorf("job folder for %q (looked for %q) not found under C:\\%s", jobName, want, strings.ReplaceAll(winLogBase, "/", "\\"))
}

func listDir(share *smb2.Share, dir string) ([]string, error) {
	f, err := share.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	infos, err := f.Readdir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, fi := range infos {
		out = append(out, fi.Name())
	}
	return out, nil
}

func readFileCapped(share *smb2.Share, path string) (string, error) {
	f, err := share.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLogBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// vmFromTaskName: "Task.<vm>.<N>.log" -> "<vm>" (saca el sufijo numerico).
func vmFromTaskName(name string) string {
	b := strings.TrimSuffix(strings.TrimPrefix(name, "Task."), ".log")
	b = strings.TrimSuffix(strings.TrimPrefix(b, "task."), ".log")
	if i := strings.LastIndex(b, "."); i >= 0 {
		if _, err := strconv.Atoi(b[i+1:]); err == nil {
			b = b[:i]
		}
	}
	return b
}

// sanitize replica el saneo de nombres de carpeta de Veeam: espacios y los chars
// invalidos de path (/ \ : * ? " < > |) se reemplazan por "_". Ej: "AWS SOBR" ->
// "AWS_SOBR"; "Hyper-V - Windows/Linux" -> "Hyper-V_-_Windows_Linux".
func sanitize(s string) string {
	r := strings.NewReplacer(" ", "_", "/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return r.Replace(s)
}
