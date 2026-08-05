package benchmark

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

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

// Conn: como llegar al host (SSH) para correr el benchmark. Una conexion activa
// por sesion: el wizard conecta primero al servidor y despues elige el repo. La
// password vive SOLO aca (server-side); el frontend nunca la ve de vuelta.
type Conn struct {
	Mode     string // ssh (winrm es stub)
	Host     string
	Port     int
	Username string
	Password string
	Hostname string // hostname que devolvio el host al conectar
	Deployed bool
}

// Manager: estado en memoria de jobs y conexiones (por sesion). Todo el acceso
// se serializa con mu (los jobs se mutan desde goroutines de fondo).
type Manager struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	conns map[string]*Conn // clave: sessionID (una conexion activa por sesion)
}

func NewManager() *Manager {
	return &Manager{jobs: map[string]*Job{}, conns: map[string]*Conn{}}
}

// --- entradas (decodificadas del JSON del frontend) -------------------------

type BenchConnInput struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	Port     int    `json:"port"`
}

type BenchmarkInput struct {
	RepositoryID string `json:"repository_id"`
	Operation    string `json:"operation"` // backup (escritura) | restore (lectura)
	Duration     int    `json:"duration"`
	DiskBaseline string `json:"disk_baseline"`
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

func buildExecutor(c *Conn, path string) Executor {
	if c.Mode == "winrm" {
		return &winRMExecutor{host: c.Host}
	}
	return newSSHExecutor(c.Host, c.Port, c.Username, c.Password, path)
}

// TestConnection valida el canal SSH al host indicado (sin repo: el wizard
// conecta primero al servidor y despues elige el repo). Si OK, guarda la
// conexion activa de la sesion. Devuelve (mode, resultado).
func (m *Manager) TestConnection(ctx context.Context, s *vbr.Session, sessionID string, in BenchConnInput) (string, ConnResult) {
	if s.Demo {
		return "demo", ConnResult{OK: false, Message: "El benchmark real no corre en modo demo (necesita SSH al host)."}
	}
	if strings.TrimSpace(in.Host) == "" || strings.TrimSpace(in.Username) == "" || strings.TrimSpace(in.Password) == "" {
		return "", ConnResult{OK: false, Message: "Completá host, usuario y password."}
	}
	port := in.Port
	if port == 0 {
		port = 22
	}
	c := &Conn{Mode: "ssh", Host: in.Host, Port: port, Username: in.Username, Password: in.Password}
	res := buildExecutor(c, "").TestConnection()
	if res.OK {
		c.Hostname = res.Hostname
		m.mu.Lock()
		m.conns[sessionID] = c
		m.mu.Unlock()
	}
	return c.Mode, res
}

func (m *Manager) getConn(sessionID string) *Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[sessionID]
}

// CheckTools (paso Herramienta). Segundo retorno = existe conexion validada.
func (m *Manager) CheckTools(sessionID string) (ToolsResult, bool) {
	c := m.getConn(sessionID)
	if c == nil {
		return ToolsResult{}, false
	}
	return buildExecutor(c, "").CheckTools(), true
}

func (m *Manager) DeployTools(sessionID string) (DeployResult, bool) {
	c := m.getConn(sessionID)
	if c == nil {
		return DeployResult{}, false
	}
	res := buildExecutor(c, "").DeployTools()
	if res.OK {
		m.mu.Lock()
		c.Deployed = true
		m.mu.Unlock()
	}
	return res, true
}

// --- fase 4: correr el benchmark -------------------------------------------

// Start crea el job de disco y lo lanza en segundo plano. Exige una conexion
// validada al host (no hay modo simulado). Devuelve (jobID, status, code, detail):
// code 0 = ok; 404 = repo inexistente; 409 = falta validar la conexion.
func (m *Manager) Start(ctx context.Context, s *vbr.Session, sessionID string, in BenchmarkInput) (string, string, int, string) {
	repo, ok := m.repoByID(ctx, s, in.RepositoryID)
	if !ok {
		return "", "", 404, "Repositorio no encontrado en esta sesion."
	}
	// Requiere la conexion validada en el primer paso del wizard.
	c := m.getConn(sessionID)
	if c == nil {
		return "", "", 409, "Validá la conexión al host antes de correr (paso Conexión)."
	}
	ex := buildExecutor(c, repo.Path)

	if in.Duration <= 0 {
		in.Duration = 8
	}
	if in.Operation == "" {
		in.Operation = "backup"
	}
	if in.DiskBaseline == "" {
		in.DiskBaseline = defaultDisk
	}
	mountLabel := ""
	if repo.Mount != nil {
		mountLabel = repo.Mount.Name
	}
	job := &Job{
		ID: newID(), SessionID: sessionID,
		RepositoryID: repo.ID, RepositoryLabel: repo.Name,
		Operation: in.Operation, Resource: "disk",
		MountLabel: mountLabel,
		Host:       c.Host, Hostname: c.Hostname,
		OSType: repo.HostOS, Tool: ex.Tool(),
		DiskBaseline: in.DiskBaseline,
		Status:       "queued", Progress: 0,
	}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()

	log.Printf("benchmark %s iniciado: repo=%q host=%s path=%s operacion=%s modo=%s tool=%s",
		job.ID, repo.Name, c.Host, repo.Path, in.Operation, c.Mode, ex.Tool())
	go m.run(job, ex, in.Duration)
	return job.ID, job.Status, 0, ""
}

// run corre el benchmark de disco (fio) y anota contra el baseline.
func (m *Manager) run(job *Job, ex Executor, duration int) {
	m.set(func() { job.Status = "running" })
	seed := job.RepositoryID
	if seed == "" {
		seed = "seed"
	}

	rows, err := ex.RunDisk(
		Spec{TargetID: seed, Tests: testsForOperation(job.Operation), Duration: duration},
		func(pct int) { m.set(func() { job.Progress = pct }) })
	if err != nil {
		m.set(func() { job.Status, job.Error = "failed", err.Error() })
		log.Printf("benchmark %s FALLO: %v", job.ID, err)
		return
	}
	annotated := annotateDisk(rows, job.DiskBaseline)

	var summary string
	m.set(func() {
		job.Results.Disk = annotated
		job.Progress, job.Status = 100, "completed"
		summary = summarize(job.Results)
	})
	log.Printf("benchmark %s completado: %s", job.ID, summary)
}

// summarize arma una linea compacta de resultados para el log (sin datos sensibles).
func summarize(r Results) string {
	var parts []string
	for _, d := range r.Disk {
		parts = append(parts, fmt.Sprintf("%s=%.0fMB/s/%.0fIOPS/%.2fms(%s)", d.Name, d.BwMbps, d.Iops, d.LatMs, d.Status))
	}
	return strings.Join(parts, " ")
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
	delete(m.conns, sessionID)
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
