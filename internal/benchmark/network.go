package benchmark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
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
	{"Proxy", "Repository / Proxy", "2500-3300", "Transferencia de datos (un puerto por tarea)"},
	{"VBR / Mount server", "Repository", "6170", "Veeam Mount Service"},
	{"VBR server", "vCenter / ESXi", "443", "API de vSphere"},
	{"VBR / Proxy", "ESXi", "902", "Transporte NBD (VMware)"},
	{"Cliente / navegador", "VBR", "9419", "REST API de VBR"},
	{"Herramienta", "Host del repo (Linux)", "22", "SSH (benchmark de disco / red)"},
}

// PortsToTest: puertos concretos que probamos en el test de conectividad (los
// rangos se representan con un puerto muestra).
var PortsToTest = []int{22, 443, 902, 2500, 6160, 6162, 6170, 9419}

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

// --- Test de conectividad de puertos ----------------------------------------

type PortResult struct {
	Target string `json:"target"`
	Port   int    `json:"port"`
	Open   bool   `json:"open"`
}

// CheckConnectivity: desde srcHost (por SSH) prueba el alcance TCP a
// targetHost:port de cada puerto (usa el builtin /dev/tcp de bash).
func CheckConnectivity(srcHost string, srcPort int, user, pass, targetHost string, ports []int) ([]PortResult, error) {
	c, err := dialSSH(srcHost, srcPort, user, pass)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := make([]PortResult, 0, len(ports))
	for _, p := range ports {
		cmd := fmt.Sprintf("timeout 3 bash -c 'echo > /dev/tcp/%s/%d' 2>/dev/null && echo OPEN || echo CLOSED", targetHost, p)
		r := runSSH(c, cmd)
		out = append(out, PortResult{Target: targetHost, Port: p, Open: strings.Contains(r, "OPEN")})
	}
	return out, nil
}

// --- Benchmark de red (iperf3) ----------------------------------------------

type IperfResult struct {
	SendMbps float64 `json:"sendMbps"`
	RecvMbps float64 `json:"recvMbps"`
	Error    string  `json:"error"`
}

// RunIperf corre iperf3 entre dos hosts por SSH: server (-1 = una prueba y sale)
// en serverHost, client en clientHost. Requiere iperf3 en ambos y el puerto
// abierto entre ellos.
func RunIperf(serverHost, clientHost string, port int, user, pass string, dur int) IperfResult {
	if port == 0 {
		port = 5201
	}
	if dur <= 0 {
		dur = 10
	}
	sc, err := dialSSH(serverHost, 0, user, pass)
	if err != nil {
		return IperfResult{Error: "SSH al server (" + serverHost + "): " + err.Error()}
	}
	defer sc.Close()
	// Server efímero: sirve una prueba y termina. Corre en su propia sesión.
	go runSSH(sc, fmt.Sprintf("iperf3 -s -1 -p %d", port))
	time.Sleep(1500 * time.Millisecond) // que el server ligue antes del cliente

	cc, err := dialSSH(clientHost, 0, user, pass)
	if err != nil {
		return IperfResult{Error: "SSH al client (" + clientHost + "): " + err.Error()}
	}
	defer cc.Close()
	out := runSSH(cc, fmt.Sprintf("iperf3 -c %s -p %d -t %d -J", serverHost, port, dur))

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
		return IperfResult{Error: "no pude parsear iperf3 (¿está instalado en ambos hosts?): " + strings.TrimSpace(firstNonEmpty(out, err.Error()))}
	}
	if rep.Error != "" {
		return IperfResult{Error: rep.Error}
	}
	return IperfResult{
		SendMbps: round1(rep.End.SumSent.BitsPerSecond / 1e6),
		RecvMbps: round1(rep.End.SumReceived.BitsPerSecond / 1e6),
	}
}
