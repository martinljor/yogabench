package benchmark

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshExecutor: canal REAL a hosts Linux (proxy/repo, incluido el appliance v13)
// por SSH + fio. Reusa el parser fio-JSON compartido. La password vive solo
// server-side, como el token de VBR.
type sshExecutor struct {
	host     string
	port     int
	user     string
	password string
	dir      string // directorio de prueba (en el volumen del repo)
}

// Ruta por defecto del repositorio en el appliance v13 (validada en campo).
const defaultRepoDir = "/var/lib/veeam/backup"

func newSSHExecutor(host string, port int, user, password, dir string) *sshExecutor {
	if port == 0 {
		port = 22
	}
	if dir == "" {
		dir = defaultRepoDir
	}
	return &sshExecutor{host: host, port: port, user: user, password: password, dir: dir}
}

func (e *sshExecutor) Tool() string { return "fio" }

func (e *sshExecutor) connect() (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            e.user,
		Auth:            []ssh.AuthMethod{ssh.Password(e.password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // lab: host key self-signed, igual que el TLS del VBR
		Timeout:         15 * time.Second,
	}
	return ssh.Dial("tcp", net.JoinHostPort(e.host, strconv.Itoa(e.port)), cfg)
}

// run ejecuta un comando y devuelve (stdout, stderr, err).
func (e *sshExecutor) run(client *ssh.Client, cmd string) (string, string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", "", err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	err = sess.Run(cmd)
	return out.String(), errb.String(), err
}

func (e *sshExecutor) TestConnection() ConnResult {
	client, err := e.connect()
	if err != nil {
		return ConnResult{OK: false, Message: "Could not connect via SSH: " + err.Error()}
	}
	defer client.Close()
	out, errs, err := e.run(client, "hostname")
	if err != nil {
		return ConnResult{OK: false, Message: firstNonEmpty(strings.TrimSpace(errs), err.Error())}
	}
	return ConnResult{OK: true, Message: "SSH connection OK.", Hostname: strings.TrimSpace(out)}
}

func (e *sshExecutor) CheckTools() ToolsResult {
	client, err := e.connect()
	if err != nil {
		return ToolsResult{Installed: false, Detail: "Could not connect via SSH: " + err.Error()}
	}
	defer client.Close()
	out, _, _ := e.run(client, "command -v fio || which fio")
	if path := strings.TrimSpace(out); path != "" {
		return ToolsResult{Installed: true, Detail: "fio found at " + path + "."}
	}
	return ToolsResult{Installed: false, Detail: "fio is not installed on the repository host."}
}

func (e *sshExecutor) DeployTools() DeployResult {
	client, err := e.connect()
	if err != nil {
		return DeployResult{OK: false, Message: "Could not connect via SSH: " + err.Error()}
	}
	defer client.Close()
	if out, _, _ := e.run(client, "command -v fio || which fio"); strings.TrimSpace(out) != "" {
		return DeployResult{OK: true, Message: "fio is already present."}
	}
	// En un appliance hardened no hay gestor de paquetes ni sudo libre.
	return DeployResult{OK: false, Message: "Cannot auto-install fio on the host (appliance without sudo/package manager). Install it manually or ask to enable it."}
}

func (e *sshExecutor) RunDisk(spec Spec, onProgress func(int)) ([]DiskRow, error) {
	tests := spec.Tests
	if len(tests) == 0 {
		tests = defaultTests
	}
	duration := spec.Duration
	if duration <= 0 {
		duration = 8
	}
	client, err := e.connect()
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	defer client.Close()

	engine := "libaio"
	var rows []DiskRow
	for i, name := range tests {
		out, errs, err := e.run(client, fioCommand(e.dir, name, engine, duration))
		// Fallback: si el host no tiene el modulo async, reintentar con psync (QD1).
		if err != nil && engine == "libaio" && mentionsEngine(errs+out) {
			engine = "psync"
			out, errs, err = e.run(client, fioCommand(e.dir, name, engine, duration))
		}
		if err != nil {
			return nil, fmt.Errorf("fio %s failed: %s", name, strings.TrimSpace(firstNonEmpty(errs, err.Error())))
		}
		rep, perr := parseFioReport([]byte(extractJSON(out)))
		if perr != nil {
			return nil, fmt.Errorf("could not parse fio output (%s): %v", name, perr)
		}
		rows = append(rows, normalizeFio(rep)...)
		if onProgress != nil {
			onProgress((i + 1) * 100 / len(tests))
		}
	}
	return rows, nil
}

// --- helpers ----------------------------------------------------------------

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func mentionsEngine(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "ioengine") || strings.Contains(l, "libaio") || strings.Contains(l, "engine")
}

// extractJSON recorta cualquier ruido previo al primer '{' (fio a veces imprime
// notas antes del JSON).
func extractJSON(s string) string {
	if i := strings.IndexByte(s, '{'); i >= 0 {
		return s[i:]
	}
	return s
}
