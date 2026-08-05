package benchmark

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"yogabench/internal/vbr"
)

// Operacion emulada -> que tests de disco corre (perspectiva del repositorio,
// donde suele estar el bottleneck): backup=escritura, restore=lectura.
var operationTests = map[string][]string{
	"backup":  {"seqwrite", "randwrite"},
	"restore": {"seqread", "randread"},
	"both":    {"seqread", "seqwrite", "randread", "randwrite"},
}

func testsForOperation(op string) []string {
	if t, ok := operationTests[op]; ok {
		return t
	}
	return operationTests["both"]
}

func resourcesFor(resource string) []string {
	if resource == "all" {
		return []string{"disk", "net", "compute"}
	}
	return []string{resource}
}

// Conn: como llegar al host del repo para correr el benchmark. La password vive
// SOLO aca (server-side), como el token de VBR; el frontend nunca la ve de vuelta.
type Conn struct {
	Mode        string // mock | ssh | winrm
	OSType      string
	TargetID    string
	TargetLabel string
	Host        string
	Port        int
	Username    string
	Password    string
	Transport   string
	Path        string // dir de prueba en el volumen del repo (ssh)
	Deployed    bool
}

// Manager: estado en memoria de jobs y conexiones (por sesion). Todo el acceso
// se serializa con mu (los jobs se mutan desde goroutines de fondo).
type Manager struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	conns map[string]*Conn // clave "session:target"
}

func NewManager() *Manager {
	return &Manager{jobs: map[string]*Job{}, conns: map[string]*Conn{}}
}

// --- entradas (decodificadas del JSON del frontend) -------------------------

type BenchConnInput struct {
	RepositoryID string `json:"repository_id"`
	Host         string `json:"host"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Port         int    `json:"port"`
	Transport    string `json:"transport"`
	Path         string `json:"path"` // opcional: dir de prueba (ssh); default /var/lib/veeam/backup
}

type BenchmarkInput struct {
	RepositoryID string `json:"repository_id"`
	Operation    string `json:"operation"`
	Resource     string `json:"resource"`
	ProxyID      string `json:"proxy_id"`
	Duration     int    `json:"duration"`
	DiskBaseline string `json:"disk_baseline"`
	NetBaseline  string `json:"net_baseline"`
}

// --- opciones del formulario ------------------------------------------------

// Options: repos (destino, con SO del host + mount server) + proxies (para atar
// en backup). El SO define fio vs diskspd.
func (m *Manager) Options(ctx context.Context, s *vbr.Session) ([]RepoOption, []ProxyOption) {
	managed := getItems(ctx, s, "v1/backupInfrastructure/managedServers?limit=1000")
	var repos []RepoOption
	for _, r := range allRepositories(ctx, s) {
		repos = append(repos, resolveRepo(r, managed))
	}
	var proxies []ProxyOption
	for _, p := range getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000") {
		name := str(p["name"])
		if name == "" {
			name = "proxy"
		}
		proxies = append(proxies, ProxyOption{ID: str(p["id"]), Name: name, OS: resolveProxyOS(p, managed)})
	}
	return repos, proxies
}

func (m *Manager) repoByID(ctx context.Context, s *vbr.Session, id string) (RepoOption, bool) {
	managed := getItems(ctx, s, "v1/backupInfrastructure/managedServers?limit=1000")
	for _, r := range allRepositories(ctx, s) {
		if str(r["id"]) == id {
			return resolveRepo(r, managed), true
		}
	}
	return RepoOption{}, false
}

// --- ciclo del executor: conexion -> preflight -> deploy --------------------

// makeConn decide el modo: demo/sin-password -> mock; Windows+password -> winrm
// (stub); Linux+password -> ssh (real, fio). El puerto por defecto depende del
// canal (WinRM 5985 / SSH 22).
func makeConn(id, name, osType string, demo bool, host, user, pass string, port int, transport, path string) *Conn {
	mode := "mock"
	if !demo && pass != "" {
		if osType == "windows" {
			mode = "winrm"
		} else {
			mode = "ssh"
		}
	}
	if port == 0 {
		if mode == "winrm" {
			port = 5985
		} else {
			port = 22
		}
	}
	if transport == "" {
		transport = "ntlm"
	}
	return &Conn{Mode: mode, OSType: osType, TargetID: id, TargetLabel: name,
		Host: host, Port: port, Username: user, Password: pass, Transport: transport, Path: path}
}

func buildExecutor(c *Conn) Executor {
	switch c.Mode {
	case "winrm":
		return &winRMExecutor{host: c.Host}
	case "ssh":
		return newSSHExecutor(c.Host, c.Port, c.Username, c.Password, c.Path)
	default:
		return NewMockExecutor(c.OSType, c.TargetID, c.Deployed)
	}
}

func connKey(sessionID, target string) string { return sessionID + ":" + target }

// TestConnection valida el canal al host del repo. Devuelve (mode, resultado,
// repoEncontrado). Si OK, guarda la conexion para las fases siguientes.
func (m *Manager) TestConnection(ctx context.Context, s *vbr.Session, sessionID string, in BenchConnInput) (string, ConnResult, bool) {
	repo, ok := m.repoByID(ctx, s, in.RepositoryID)
	if !ok {
		return "", ConnResult{}, false
	}
	c := makeConn(repo.ID, repo.Name, repo.HostOS, s.Demo, in.Host, in.Username, in.Password, in.Port, in.Transport, in.Path)
	res := buildExecutor(c).TestConnection()
	if res.OK {
		m.mu.Lock()
		m.conns[connKey(sessionID, in.RepositoryID)] = c
		m.mu.Unlock()
	}
	return c.Mode, res, true
}

func (m *Manager) getConn(sessionID, repoID string) *Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[connKey(sessionID, repoID)]
}

// CheckTools (fase 2). Segundo retorno = existe conexion validada.
func (m *Manager) CheckTools(sessionID, repoID string) (ToolsResult, bool) {
	c := m.getConn(sessionID, repoID)
	if c == nil {
		return ToolsResult{}, false
	}
	return buildExecutor(c).CheckTools(), true
}

// DeployTools (fase 3). Segundo retorno = existe conexion validada.
func (m *Manager) DeployTools(sessionID, repoID string) (DeployResult, bool) {
	c := m.getConn(sessionID, repoID)
	if c == nil {
		return DeployResult{}, false
	}
	res := buildExecutor(c).DeployTools()
	if res.OK {
		m.mu.Lock()
		c.Deployed = true
		m.mu.Unlock()
	}
	return res, true
}

// --- fase 4: correr el benchmark -------------------------------------------

// Start crea el job y lo lanza en segundo plano. Devuelve (jobID, status,
// repoEncontrado).
func (m *Manager) Start(ctx context.Context, s *vbr.Session, sessionID string, in BenchmarkInput) (string, string, bool) {
	repo, ok := m.repoByID(ctx, s, in.RepositoryID)
	if !ok {
		return "", "", false
	}
	proxyLabel := ""
	if in.ProxyID != "" {
		for _, p := range getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000") {
			if str(p["id"]) == in.ProxyID {
				proxyLabel = str(p["name"])
				break
			}
		}
	}
	// Conexion validada al host si existe; si no, mock (demo sin ciclo previo).
	c := m.getConn(sessionID, in.RepositoryID)
	if c == nil {
		c = makeConn(repo.ID, repo.Name, repo.HostOS, true, "", "", "", 0, "", "")
	}
	ex := buildExecutor(c)

	if in.Duration <= 0 {
		in.Duration = 8
	}
	if in.Operation == "" {
		in.Operation = "backup"
	}
	if in.Resource == "" {
		in.Resource = "disk"
	}
	if in.DiskBaseline == "" {
		in.DiskBaseline = defaultDisk
	}
	if in.NetBaseline == "" {
		in.NetBaseline = defaultNet
	}
	mountLabel := ""
	if repo.Mount != nil {
		mountLabel = repo.Mount.Name
	}
	job := &Job{
		ID: newID(), SessionID: sessionID,
		RepositoryID: repo.ID, RepositoryLabel: repo.Name,
		Operation: in.Operation, Resource: in.Resource,
		ProxyLabel: proxyLabel, MountLabel: mountLabel,
		OSType: repo.HostOS, Tool: ex.Tool(),
		DiskBaseline: in.DiskBaseline, NetBaseline: in.NetBaseline,
		Status: "queued", Progress: 0,
	}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()

	go m.run(job, ex, in.Duration)
	return job.ID, job.Status, true
}

func (m *Manager) run(job *Job, ex Executor, duration int) {
	m.set(func() { job.Status = "running" })
	resources := resourcesFor(job.Resource)
	seed := job.RepositoryID
	if seed == "" {
		seed = "seed"
	}
	n := len(resources)

	for i, res := range resources {
		var err error
		switch res {
		case "disk":
			base := i
			rows, e := ex.RunDisk(Spec{TargetID: seed, Tests: testsForOperation(job.Operation), Duration: duration},
				func(pct int) { m.set(func() { job.Progress = (base*100 + pct) / n }) })
			if e != nil {
				err = e
				break
			}
			annotated := annotateDisk(rows, job.DiskBaseline)
			m.set(func() { job.Results.Disk = annotated })
		case "net":
			time.Sleep(sleepFor(duration))
			metric := annotateNet(mockNet(seed), job.NetBaseline)
			m.set(func() { job.Results.Net = metric })
		case "compute":
			time.Sleep(sleepFor(duration))
			metric := annotateCompute(mockCompute(seed))
			m.set(func() { job.Results.Compute = metric })
		}
		if err != nil {
			m.set(func() { job.Status, job.Error = "failed", err.Error() })
			return
		}
		done := i + 1
		m.set(func() { job.Progress = done * 100 / n })
	}
	m.set(func() { job.Progress, job.Status = 100, "completed" })
}

func sleepFor(duration int) time.Duration {
	if duration > 3 {
		duration = 3
	}
	return time.Duration(duration) * time.Second
}

// --- lecturas (marshaladas bajo lock para no correr con las goroutines) -----

func (m *Manager) JobJSON(id string) ([]byte, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, "", false
	}
	b, _ := json.Marshal(j)
	return b, j.SessionID, true
}

func (m *Manager) ListJobsJSON(sessionID string) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Job{}
	for _, j := range m.jobs {
		if j.SessionID == sessionID {
			out = append(out, j)
		}
	}
	b, _ := json.Marshal(map[string]any{"data": out})
	return b
}

// ClearSession descarta jobs y conexiones (con su password) de una sesion.
func (m *Manager) ClearSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if j.SessionID == sessionID {
			delete(m.jobs, id)
		}
	}
	prefix := sessionID + ":"
	for k := range m.conns {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(m.conns, k)
		}
	}
}

// set aplica una mutacion sobre un job bajo el lock del manager.
func (m *Manager) set(fn func()) {
	m.mu.Lock()
	fn()
	m.mu.Unlock()
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
