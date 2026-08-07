package benchmark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"yogabench/internal/dbg"
)

// --- Puertos requeridos entre componentes de Veeam (referencia) -------------

type PortRule struct {
	From string `json:"from"`
	To   string `json:"to"`
	Port string `json:"port"`
	Desc string `json:"desc"`
}

// VeeamPorts: matriz de referencia de los puertos clave (ver doc oficial de Veeam
// para el detalle completo por rol/versión).
var VeeamPorts = []PortRule{
	{"VBR server", "Managed servers (proxy/repo)", "6160", "Veeam Installer Service"},
	{"VBR server", "Managed servers", "6162", "Veeam Data Mover"},
	{"Proxy", "Repository / Proxy", "2500-3300", "Data transfer (one port per task)"},
	{"VBR / Mount server", "Repository", "6170", "Veeam Mount Service"},
	{"VBR server", "vCenter / ESXi", "443", "vSphere API"},
	{"VBR / Proxy", "ESXi", "902", "NBD transport (VMware)"},
	{"Client / browser", "VBR", "9419", "REST API de VBR"},
	{"Tool", "Repo host (Linux)", "22", "SSH (disk/network benchmark)"},
}

// --- Escenarios de conectividad (por proposito) -----------------------------
//
// En vez de escanear una lista plana de puertos, probamos SOLO los puertos que
// un proposito concreto necesita (ej: "agregar un proxy"), en el sentido
// correcto origen->destino. Asi el resultado tiene sentido operativo.

// PortCheck: un puerto concreto a probar, con su etiqueta (puede ser un rango
// como "2500-3300", del que probamos un puerto muestra) y para que sirve.
type PortCheck struct {
	Port    int    `json:"port"`    // puerto concreto que se prueba
	Label   string `json:"label"`   // etiqueta mostrada (ej: "2500-3300")
	Purpose string `json:"purpose"` // para que sirve
}

// ConnScenario: un proposito de conectividad Veeam y los puertos que valida.
type ConnScenario struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Source string      `json:"source"` // rol origen (hint para el usuario)
	Target string      `json:"target"` // rol destino (hint)
	Note   string      `json:"note"`
	Ports  []PortCheck `json:"ports"`
}

// Scenarios: catalogo de propositos de conectividad de un entorno VMware + repo
// Linux (los puertos siguen la doc oficial de Veeam). Descripciones en ingles
// (base del backend); el WebUI traduce las etiquetas estaticas.
func Scenarios() []ConnScenario {
	return []ConnScenario{
		{
			ID: "add-proxy", Name: "Add / validate a backup proxy",
			Source: "VBR server", Target: "Proxy host",
			Note:  "Ports the VBR server needs to deploy and drive a backup proxy.",
			Ports: []PortCheck{{6160, "6160", "Veeam Installer Service (deploy components)"}, {6162, "6162", "Veeam Data Mover"}},
		},
		{
			ID: "add-repo", Name: "Add / validate a backup repository",
			Source: "VBR / Mount server", Target: "Repository host",
			Note:  "Ports to deploy the repository and to mount backups for restores.",
			Ports: []PortCheck{{6160, "6160", "Veeam Installer Service (deploy components)"}, {6162, "6162", "Veeam Data Mover"}, {6170, "6170", "Veeam Mount Service"}},
		},
		{
			ID: "backup-path", Name: "Backup data path (proxy -> repository)",
			Source: "Proxy", Target: "Repository / gateway",
			Note:  "Transport ports used to move backup data (one port per parallel task).",
			Ports: []PortCheck{{2500, "2500-3300", "Data transfer (one port per task)"}},
		},
		{
			ID: "connect-vmware", Name: "Connect VMware (vCenter / ESXi)",
			Source: "VBR / Proxy", Target: "vCenter / ESXi",
			Note:  "Management API and NBD data transport for VMware backups.",
			Ports: []PortCheck{{443, "443", "vSphere API (management)"}, {902, "902", "NBD transport (VM data)"}},
		},
		{
			ID: "rest-api", Name: "REST API access",
			Source: "Client / browser / this tool", Target: "VBR server",
			Note:  "The VBR REST API this tool talks to.",
			Ports: []PortCheck{{9419, "9419", "Veeam REST API"}},
		},
		{
			ID: "ssh-linux", Name: "SSH to a Linux host (management / benchmark)",
			Source: "This tool / VBR", Target: "Linux host (proxy / repo / appliance)",
			Note:  "SSH used by this tool for the disk and network benchmarks.",
			Ports: []PortCheck{{22, "22", "SSH"}},
		},
	}
}

// ScenarioByID busca un escenario por id.
func ScenarioByID(id string) (ConnScenario, bool) {
	for _, s := range Scenarios() {
		if s.ID == id {
			return s, true
		}
	}
	return ConnScenario{}, false
}

// --- SSH helpers compartidos ------------------------------------------------

func dialSSH(host string, port int, user, pass string) (*ssh.Client, error) {
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	return ssh.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), cfg)
}

func runSSH(c *ssh.Client, cmd string) string {
	sess, err := c.NewSession()
	if err != nil {
		return ""
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &out
	_ = sess.Run(cmd)
	return out.String()
}

// runSSHOut: como runSSH pero devuelve stdout y stderr POR SEPARADO. iperf3 -J
// escribe el JSON en stdout y los errores en stderr; mezclarlos corrompe el JSON.
func runSSHOut(c *ssh.Client, cmd string) (stdout, stderr string) {
	sess, err := c.NewSession()
	if err != nil {
		return "", err.Error()
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	_ = sess.Run(cmd)
	return out.String(), errb.String()
}

// --- Test de conectividad de puertos ----------------------------------------

type PortResult struct {
	Target  string `json:"target"`
	Port    int    `json:"port"`
	Label   string `json:"label"`
	Purpose string `json:"purpose"`
	Open    bool   `json:"open"`
}

// CheckConnectivity: desde srcHost (por SSH, con las credenciales del ORIGEN)
// prueba el alcance TCP a targetHost en los puertos del escenario elegido (usa
// el builtin /dev/tcp de bash). Solo necesita credenciales del origen.
func CheckConnectivity(srcHost string, srcPort int, user, pass, targetHost string, ports []PortCheck) ([]PortResult, error) {
	c, err := dialSSH(srcHost, srcPort, user, pass)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := make([]PortResult, 0, len(ports))
	for _, p := range ports {
		cmd := fmt.Sprintf("timeout 3 bash -c 'echo > /dev/tcp/%s/%d' 2>/dev/null && echo OPEN || echo CLOSED", targetHost, p.Port)
		r := runSSH(c, cmd)
		open := strings.Contains(r, "OPEN")
		dbg.Logf("port-check %s -> %s:%d = %v (%s)", srcHost, targetHost, p.Port, open, p.Purpose)
		out = append(out, PortResult{Target: targetHost, Port: p.Port, Label: p.Label, Purpose: p.Purpose, Open: open})
	}
	return out, nil
}

// --- Benchmark de red (iperf3) ----------------------------------------------

type IperfResult struct {
	SendMbps      float64 `json:"sendMbps"`
	RecvMbps      float64 `json:"recvMbps"`
	Port          int     `json:"port"`          // puerto que finalmente funciono
	ExpectedMbps  float64 `json:"expectedMbps"`  // capacidad esperada del enlace elegido
	ExpectedLabel string  `json:"expectedLabel"` // ej: "10 GbE"
	Pct           int     `json:"pct"`           // % del enlace alcanzado (mejor de send/recv)
	Status        string  `json:"status"`        // ok | warn | low
	Error         string  `json:"error"`
}

// iperfCandidatePorts: puertos a probar en modo AUTO. Son del rango de transporte
// de Veeam (2500-3300), que el firewall del appliance abre por defecto y que estan
// libres cuando no corre un backup. Como no hay sudo para mirar el firewall,
// probamos empiricamente cual funciona entre los dos hosts.
var iperfCandidatePorts = []int{2500, 2600, 3000, 3200, 3300}

// RunIperf corre iperf3 entre dos hosts por SSH, cada uno con sus PROPIAS
// credenciales SSH. linkKey es el enlace esperado (1gbe/10gbe/...) para anotar el
// resultado. Si port<=0 => modo AUTO: prueba los puertos de Veeam abiertos por
// defecto y usa el primero que conecte. Requiere iperf3 en ambos hosts.
func RunIperf(serverHost, serverUser, serverPass, clientHost, clientUser, clientPass, linkKey string, port, dur int) IperfResult {
	if dur <= 0 {
		dur = 10
	}
	sc, err := dialSSH(serverHost, 0, serverUser, serverPass)
	if err != nil {
		return IperfResult{Error: "SSH to server (" + serverHost + "): " + err.Error()}
	}
	defer sc.Close()
	cc, err := dialSSH(clientHost, 0, clientUser, clientPass)
	if err != nil {
		return IperfResult{Error: "SSH to client (" + clientHost + "): " + err.Error()}
	}
	defer cc.Close()

	ports := []int{port}
	auto := port <= 0
	if auto {
		ports = iperfCandidatePorts
	}

	var res IperfResult
	for _, p := range ports {
		res = runIperfOnce(sc, cc, serverHost, clientHost, p, dur)
		if res.Error == "" {
			res.Port = p
			break
		}
		if auto {
			dbg.Logf("iperf auto: port %d not usable -> %s", p, dbg.Clip(res.Error, 120))
		}
	}
	if res.Error != "" {
		if auto {
			res.Error = fmt.Sprintf("no usable port found in the Veeam transport range %v — the firewall may open these only during jobs. Last: %s", iperfCandidatePorts, res.Error)
		}
		return res
	}
	// Anotar contra la capacidad del enlace elegido (mejor de send/recv).
	exp, label := NetExpected(linkKey)
	res.ExpectedMbps, res.ExpectedLabel = exp, label
	best := res.RecvMbps
	if res.SendMbps > best {
		best = res.SendMbps
	}
	if exp > 0 {
		res.Pct = int(math.Round(best / exp * 100))
		res.Status = verdict(best / exp)
	}
	return res
}

// runIperfOnce corre una prueba en un puerto concreto. El server se autolimita con
// `timeout` (no deja procesos colgados) y el cliente usa --connect-timeout para
// fallar rapido si el puerto esta filtrado (evita el cuelgue de ~2 min).
func runIperfOnce(sc, cc *ssh.Client, serverHost, clientHost string, port, dur int) IperfResult {
	go runSSH(sc, fmt.Sprintf("timeout %d iperf3 -s -1 -p %d", dur+12, port))
	time.Sleep(1200 * time.Millisecond) // que el server ligue antes del cliente
	clientCmd := fmt.Sprintf("timeout %d iperf3 --connect-timeout 5000 -c %s -p %d -t %d -J", dur+8, serverHost, port, dur)
	out, errOut := runSSHOut(cc, clientCmd)
	dbg.Logf("iperf try port=%d cmd=%q", port, clientCmd)
	dbg.Logf("iperf stdout: %s", dbg.Clip(out, 500))
	dbg.Logf("iperf stderr: %s", dbg.Clip(errOut, 200))

	var rep struct {
		End struct {
			SumSent struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_sent"`
			SumReceived struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &rep); err != nil {
		detail := strings.TrimSpace(firstNonEmpty(errOut, out))
		return IperfResult{Error: fmt.Sprintf("port %d: %s", port, dbg.Clip(detail, 160))}
	}
	if rep.Error != "" {
		return IperfResult{Error: fmt.Sprintf("port %d: %s", port, strings.TrimSpace(rep.Error))}
	}
	r := IperfResult{
		SendMbps: round1(rep.End.SumSent.BitsPerSecond / 1e6),
		RecvMbps: round1(rep.End.SumReceived.BitsPerSecond / 1e6),
	}
	if r.SendMbps == 0 && r.RecvMbps == 0 {
		r.Error = fmt.Sprintf("port %d: iperf3 reported 0 Mbps (connection likely blocked)", port)
	}
	return r
}
