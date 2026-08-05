package benchmark

import (
	"math"
	"time"
)

// Executor: donde corre el benchmark. El orquestador no sabe COMO se llega al
// host; pide fases (conexion -> preflight -> deploy -> correr).
//
//	MockExecutor  -> simula todo, sin tocar ningun host (demo / desarrollo)
//	winRMExecutor -> REAL (Windows/diskspd): todavia NO portado a Go (stub honesto)
type Executor interface {
	Tool() string
	TestConnection() ConnResult
	CheckTools() ToolsResult
	DeployTools() DeployResult
	RunDisk(spec Spec, onProgress func(int)) ([]DiskRow, error)
}

type Spec struct {
	TargetID string
	Tests    []string
	Duration int
}

type ConnResult struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	Hostname string `json:"hostname"`
}

type ToolsResult struct {
	Installed bool   `json:"installed"`
	Detail    string `json:"detail"`
}

type DeployResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

var defaultTests = []string{"seqread", "seqwrite", "randread", "randwrite"}

// ---------------------------------------------------------------------------
// MockExecutor: simula fio/diskspd con numeros plausibles derivados del
// target_id (semilla estable) para que cada host tenga un perfil repetible.
type MockExecutor struct {
	osType       string
	targetID     string
	toolsPresent bool
	tool         string
}

func NewMockExecutor(osType, targetID string, toolsPresent bool) *MockExecutor {
	tool := "fio"
	if osType == "windows" {
		tool = "diskspd"
	}
	return &MockExecutor{osType: osType, targetID: targetID, toolsPresent: toolsPresent, tool: tool}
}

func (e *MockExecutor) Tool() string { return e.tool }

func (e *MockExecutor) TestConnection() ConnResult {
	time.Sleep(200 * time.Millisecond)
	return ConnResult{OK: true, Message: "Conexion simulada OK (mock).", Hostname: "mock-" + e.targetID}
}

func (e *MockExecutor) CheckTools() ToolsResult {
	if e.toolsPresent {
		return ToolsResult{Installed: true, Detail: e.tool + " presente (mock)."}
	}
	return ToolsResult{Installed: false, Detail: e.tool + " no encontrado (mock)."}
}

func (e *MockExecutor) DeployTools() DeployResult {
	time.Sleep(300 * time.Millisecond)
	return DeployResult{OK: true, Message: e.tool + " desplegado (mock)."}
}

func (e *MockExecutor) RunDisk(spec Spec, onProgress func(int)) ([]DiskRow, error) {
	tests := spec.Tests
	if len(tests) == 0 {
		tests = defaultTests
	}
	rng := seededRand(spec.TargetID)
	factor := uni(rng, 0.6, 1.4)

	total := spec.Duration
	if total > 10 {
		total = 10
	}
	if total < 1 {
		total = 1
	}
	steps := len(tests) * 4
	if steps < 1 {
		steps = 1
	}
	for i := 0; i < steps; i++ {
		time.Sleep(time.Duration(total) * time.Second / time.Duration(steps))
		if onProgress != nil {
			onProgress(int(float64(i+1) / float64(steps) * 100))
		}
	}

	jobs := make([]fioJobJSON, 0, len(tests))
	for _, name := range tests {
		jobs = append(jobs, mockFioJob(name, factor, uni(rng, 0.9, 1.1)))
	}
	return normalizeFio(fioReport{Jobs: jobs}), nil
}

// ---------------------------------------------------------------------------
// winRMExecutor: canal REAL a Windows via WinRM + diskspd. WinRM es SOAP con
// autenticacion NTLM, que la stdlib de Go no implementa; queda como stub honesto
// hasta decidir si se agrega una libreria WinRM pura-Go.
type winRMExecutor struct{ host string }

const winRMNotReady = "El benchmark REAL por WinRM (Windows/diskspd) todavia no esta portado a esta version (Go). " +
	"Por ahora usa un host sin credenciales (modo demo/simulado) o la version Python para el canal WinRM real."

func (e *winRMExecutor) Tool() string { return "diskspd" }

func (e *winRMExecutor) TestConnection() ConnResult {
	return ConnResult{OK: false, Message: winRMNotReady}
}

func (e *winRMExecutor) CheckTools() ToolsResult {
	return ToolsResult{Installed: false, Detail: winRMNotReady}
}

func (e *winRMExecutor) DeployTools() DeployResult {
	return DeployResult{OK: false, Message: winRMNotReady}
}

func (e *winRMExecutor) RunDisk(spec Spec, onProgress func(int)) ([]DiskRow, error) {
	return nil, errWinRMNotReady
}

// mockFioJob arma una entrada fio-JSON simulada (misma forma que la real) para
// un sub-test, con numeros plausibles derivados del perfil del host.
func mockFioJob(name string, factor, jitter float64) fioJobJSON {
	empty := fioDir{}
	switch name {
	case "seqread":
		bw := 900 * factor * jitter
		return fioJobJSON{Jobname: name, Read: mockDir(bw, bw, 4.0/factor), Write: empty}
	case "seqwrite":
		bw := 720 * factor * jitter
		return fioJobJSON{Jobname: name, Read: empty, Write: mockDir(bw, bw, 5.5/factor)}
	case "randread":
		iops := 90000 * factor * jitter
		return fioJobJSON{Jobname: name, Read: mockDir(iops*4/1024, iops, 0.25/factor), Write: empty}
	default: // randwrite
		iops := 62000 * factor * jitter
		return fioJobJSON{Jobname: name, Read: empty, Write: mockDir(iops*4/1024, iops, 0.45/factor)}
	}
}

// mockDir arma una seccion fio (KiB/s, IOPS, latencia_ns) desde (MB/s, IOPS, ms).
func mockDir(bwMbps, iopsVal, latMs float64) fioDir {
	d := fioDir{Bw: math.Round(bwMbps * 1024), Iops: roundN(iopsVal, 1)}
	d.LatNs.Mean = math.Round(latMs * 1_000_000)
	return d
}
