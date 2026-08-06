package benchmark

// Executor: donde corre el benchmark real. El orquestador no sabe COMO se llega
// al host; pide fases (conexion -> preflight -> deploy -> correr).
//
//	sshExecutor   -> REAL, Linux/appliance: SSH + fio
//	winRMExecutor -> Windows/diskspd: todavia stub (NTLM/SOAP no esta en stdlib)
//
// No hay modo simulado: para correr hacen falta credenciales reales del host.
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
// winRMExecutor: canal REAL a Windows via WinRM + diskspd. WinRM es SOAP con
// autenticacion NTLM, que la stdlib de Go no implementa; queda como stub honesto
// hasta que haga falta el canal Windows (el appliance v13 es Linux -> SSH).
type winRMExecutor struct{ host string }

const winRMNotReady = "The WinRM (Windows/diskspd) benchmark is not ported to this version yet. " +
	"The v13 appliance is Linux: use a repository on a Linux host (SSH+fio channel)."

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
