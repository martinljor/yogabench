package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yogabench/internal/analysis"
	"yogabench/internal/benchmark"
	"yogabench/internal/deeplog"
	"yogabench/internal/topology"
	"yogabench/internal/vbr"
)

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// writeErr traduce un APIError a su status; el resto es 500.
func writeErr(w http.ResponseWriter, err error) {
	var ae *vbr.APIError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, map[string]string{"detail": ae.Message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
}

// session resuelve la sesion del path o responde 404.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*vbr.Session, bool) {
	sess, ok := s.store.Get(r.PathValue("session"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Session not found. Reconnect to VBR."})
		return nil, false
	}
	return sess, true
}

// proxy reenvia el JSON crudo de una ruta de la REST API de VBR.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, path string) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	raw, err := vbr.Get(r.Context(), sess, path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, raw)
}

// --- handlers ---------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active_sessions": s.store.Count(), "version": s.version})
}

type connectRequest struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	APIVersion string `json:"api_version"`
	VerifySSL  bool   `json:"verify_ssl"`
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	var req connectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	if req.Port == 0 {
		req.Port = 9419
	}
	if req.APIVersion == "" {
		req.APIVersion = "1.3-rev0" // v13 (trae todos los tipos de proxy)
	}
	access, refresh, expiresIn, err := vbr.Authenticate(
		r.Context(), req.Host, req.Port, req.Username, req.Password, req.APIVersion, req.VerifySSL)
	if err != nil {
		log.Printf("VBR connection failed: host=%s port=%d apiVersion=%s: %v", req.Host, req.Port, req.APIVersion, err) // no password
		writeErr(w, err)
		return
	}
	sess := &vbr.Session{
		Host: req.Host, Port: req.Port, APIVersion: req.APIVersion, VerifySSL: req.VerifySSL,
		CreatedAt: time.Now(),
	}
	sess.SetTokens(access, refresh, expiresIn) // se renuevan solos (ver vbr.Get)
	id := s.store.New(sess)
	log.Printf("VBR connected: host=%s port=%d apiVersion=%s", req.Host, req.Port, req.APIVersion) // no password/token
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "expires_in": expiresIn})
}

func (s *Server) connectDemo(w http.ResponseWriter, r *http.Request) {
	id := s.store.New(&vbr.Session{Demo: true, Host: "demo-vbr"})
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "expires_in": 3600})
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("session")
	s.store.Delete(id)
	s.bench.ClearSession(id) // descarta jobs y las passwords de proxies
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) proxies(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "v1/backupInfrastructure/proxies")
}

// repositories devuelve repos normales + scale-out (SOBR) unificados.
func (s *Server) repositories(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	repos, err := topology.AllRepositories(r.Context(), sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": repos})
}

func (s *Server) managedServers(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "v1/backupInfrastructure/managedServers")
}

func (s *Server) flow(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	g, err := topology.Build(r.Context(), sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// --- Red (puertos + iperf) --------------------------------------------------

// ports: catalogo de escenarios de conectividad (por proposito) + la matriz de
// referencia de puertos (que el WebUI muestra oculta, solo a demanda).
func (s *Server) ports(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios": benchmark.Scenarios(),
		"reference": benchmark.VeeamPorts,
	})
}

type portsCheckInput struct {
	Scenario   string `json:"scenario"`
	SrcHost    string `json:"srcHost"`
	TargetHost string `json:"targetHost"`
	Username   string `json:"username"` // credenciales del ORIGEN (SSH desde ahi)
	Password   string `json:"password"` // no password
	Port       int    `json:"port"`
}

// portsCheck: para el proposito elegido, prueba (por SSH desde srcHost, con las
// credenciales del origen) el alcance TCP a targetHost en solo los puertos que
// ese proposito necesita.
func (s *Server) portsCheck(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if sess.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"detail": "Port test does not run in demo mode (needs SSH)."})
		return
	}
	var in portsCheckInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	sc, ok := benchmark.ScenarioByID(in.Scenario)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Unknown connectivity scenario."})
		return
	}
	res, err := benchmark.CheckConnectivity(in.SrcHost, in.Port, in.Username, in.Password, in.TargetHost, sc.Ports)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": "Could not SSH to " + in.SrcHost + ": " + err.Error()})
		return
	}
	log.Printf("ports-check [%s] %s -> %s: %d ports", in.Scenario, in.SrcHost, in.TargetHost, len(res))
	writeJSON(w, http.StatusOK, map[string]any{"data": res})
}

type iperfInput struct {
	ServerHost string `json:"serverHost"`
	ServerUser string `json:"serverUser"` // credenciales SSH del servidor
	ServerPass string `json:"serverPass"` // no password
	ClientHost string `json:"clientHost"`
	ClientUser string `json:"clientUser"` // credenciales SSH del cliente (pueden diferir)
	ClientPass string `json:"clientPass"` // no password
	LinkSpeed  string `json:"linkSpeed"`  // enlace esperado: 1gbe/10gbe/25gbe/40gbe
	Port       int    `json:"port"`       // puerto de iperf (default 5201; configurable por firewall)
	Duration   int    `json:"duration"`
}

// iperf: benchmark de red iperf3 entre dos hosts (por SSH), con credenciales
// propias para cada host.
func (s *Server) iperf(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if sess.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"error": "Network benchmark does not run in demo mode (needs SSH + iperf3)."})
		return
	}
	var in iperfInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	log.Printf("iperf %s -> %s started", in.ClientHost, in.ServerHost)
	res := benchmark.RunIperf(in.ServerHost, in.ServerUser, in.ServerPass, in.ClientHost, in.ClientUser, in.ClientPass, in.LinkSpeed, in.Port, in.Duration)
	log.Printf("iperf %s -> %s: send=%.0fMbps recv=%.0fMbps link=%s expected=%.0fMbps pct=%d%% verdict=%s err=%q",
		in.ClientHost, in.ServerHost, res.SendMbps, res.RecvMbps, in.LinkSpeed, res.ExpectedMbps, res.Pct, res.Status, res.Error)
	s.bench.SetIperf(r.PathValue("session"), res) // queda como senal MEDIDA del analisis
	writeJSON(w, http.StatusOK, res)
}

// recommendations: sugerencias de asignacion de proxies (VMware) para balancear.
func (s *Server) recommendations(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	recs, err := topology.Recommend(r.Context(), sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": recs})
}

// analysis: estadistica de bottleneck agregada por repo y proxy. `days` opcional
// acota la ventana (sin days = todo el historico disponible, con cota interna).
func (s *Server) analysis(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var days *int
	if q := r.URL.Query().Get("days"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			days = &n
		}
	}
	res, err := analysis.Build(r.Context(), sess, days)
	if err != nil {
		writeErr(w, err)
		return
	}
	if a := res.Assessment; a != nil {
		sess.SetAnalyzed("assessment", a) // queda para el diagnostico
		top := "-"
		if len(a.Actions) > 0 {
			top = a.Actions[0].Code
		}
		log.Printf("assessment: %d job(s)/%d run(s) in %dd · peak=%.0fMB/s at %s · bottleneck=%s(%d%%) · busiest=%02dh(%d jobs, %d%%) · top=%s | %s",
			a.Jobs, a.Runs, a.Days, a.PeakMBps, a.PeakAt, a.TopStage, a.TopStagePct, a.BusiestHour, a.BusiestJobs, a.BusiestPct, top, a.Headline)
	}
	writeJSON(w, http.StatusOK, res)
}

// analysisRange: rango real de dias con datos (sesion mas vieja y mas nueva).
func (s *Server) analysisRange(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, analysis.Range(r.Context(), sess))
}

// analysisJobs: lista de jobs para el listbox del drill-down (modo "Un job").
func (s *Server) analysisJobs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": analysis.JobList(r.Context(), sess)})
}

// analysisJob: modelo de capacidad de UN job (su ultima corrida con datos).
func (s *Server) analysisJob(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	jobID := r.URL.Query().Get("jobId")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "jobId required"})
		return
	}
	days := 7
	if q := r.URL.Query().Get("days"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			days = n
		}
	}
	res, err := analysis.JobCapacity(r.Context(), sess, jobID, days)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"detail": err.Error()})
		return
	}
	// Si ya medimos el techo real (fio/iperf) del repo/enlace de este job, el
	// veredicto se rehace con esa senal: cambia el consejo de raiz.
	if m := s.measuredFor(r.PathValue("session"), repoIDsOf(res)); m != nil {
		res.Verdict = analysis.BuildVerdict(res, nil, m)
	}
	proj := "-"
	if res.Projection != nil {
		proj = fmt.Sprintf("%s ~%d%%", res.Projection.NextStage, res.Projection.ImprovementPct)
	}
	log.Printf("analysis job %s: session=%s primary=%s conf=%s read=%.0fMB/s write=%.0fMB/s proj=%s",
		jobID, res.SessionID, res.Primary, res.Confidence, res.ReadMBps, res.WriteMBps, proj)
	logVerdict(jobID, res.Verdict)
	sess.SetAnalyzed("job:"+jobID, res) // queda para el diagnostico
	writeJSON(w, http.StatusOK, res)
}

// measuredFor arma la senal MEDIDA del job: el techo real que ya medimos con
// fio (disco del repo) y con iperf (enlace) en esta sesion. Sin esto, un
// "Target 98%" no distingue "el disco no da mas" de "el disco esta ocioso y el
// job no lo alimenta". Devuelve nil si no se midio nada.
// repoIDs: los repos que usa el job (de las tareas de la corrida representativa).
func (s *Server) measuredFor(sessionID string, repoIDs []string) *analysis.Measured {
	m := &analysis.Measured{}
	for _, id := range repoIDs {
		if mbps, tool, label := s.bench.DiskWriteMBps(sessionID, id); mbps > m.RepoWriteMBps {
			m.RepoWriteMBps, m.RepoTool, m.RepoName = mbps, tool, label
		}
	}
	if ip := s.bench.Iperf(sessionID); ip != nil {
		m.LinkMbps = ip.SendMbps // el sentido que importa: proxy -> repo
		if ip.RecvMbps > m.LinkMbps {
			m.LinkMbps = ip.RecvMbps
		}
		m.LinkExpected, m.LinkLabel = ip.ExpectedMbps, ip.ExpectedLabel
	}
	if m.RepoWriteMBps == 0 && m.LinkMbps == 0 {
		return nil
	}
	return m
}

// repoIDsOf: repos que toco el job (de la corrida representativa).
func repoIDsOf(res *analysis.JobCapacityResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range res.Resources {
		if x.Kind == "repository" && x.ID != "" && !seen[x.ID] {
			seen[x.ID] = true
			out = append(out, x.ID)
		}
	}
	return out
}

// logVerdict deja el veredicto en el log (en ingles) para poder calibrar el
// motor con lo que pasa en campo.
func logVerdict(jobID string, v *analysis.Verdict) {
	if v == nil {
		return
	}
	top := "-"
	if len(v.Actions) > 0 {
		top = v.Actions[0].Code
		if g := v.Actions[0].GainPct; g != nil {
			top = fmt.Sprintf("%s(-%d%%)", top, *g)
		}
	}
	log.Printf("verdict job %s: sev=%s stage=%s cause=%s(known=%v) gain=%d%% deep=%v actions=%d top=%s | %s",
		jobID, v.Severity, v.Stage, v.CauseCode, v.CauseKnown, v.GainPct, v.HasDeep, len(v.Actions), top, v.Headline)
}

type deepInput struct {
	JobID    string `json:"jobId"`
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"` // no password
	Domain   string `json:"domain"`
	Days     int    `json:"days"`
}

// analysisJobDeep: "doble-click" — entra al OS del VBR (Windows: SMB2 a C$) con las
// credenciales del usuario, baja Job/Task logs del job y devuelve el analisis deep
// (transporte+motivo, 4-stage por VM, duraciones, opciones). Read-only; sin secretos al log.
func (s *Server) analysisJobDeep(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if sess.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"detail": "Deep mode does not run in demo (needs host access to the VBR)."})
		return
	}
	var in deepInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	if in.JobID == "" || strings.TrimSpace(in.Username) == "" || in.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "jobId, username and password are required"})
		return
	}
	name, osKind, err := analysis.JobDeepTarget(r.Context(), sess, in.JobID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"detail": err.Error()})
		return
	}
	host := strings.TrimSpace(in.Host)
	if host == "" {
		host = sess.Host
	}
	if osKind == "linux" {
		writeJSON(w, http.StatusOK, map[string]string{"detail": "Deep mode for a Linux appliance needs SSH, which the hardened v13 appliance does not allow. It is supported on a Windows VBR (SMB to C$)."})
		return
	}
	jobLog, taskLogs, err := deeplog.FetchWindows(host, in.Username, in.Password, in.Domain, name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"detail": err.Error()})
		return
	}
	res := deeplog.Parse(jobLog, taskLogs)
	res.JobName = name
	log.Printf("deep analysis: job=%q host=%s runAt=%q transport=%s vms=%d disks=%d load=%v notes=%d",
		name, host, res.RunAt, res.Transport, len(res.VMs), deepDiskCount(res), res.Aggregate != nil, len(res.Notes)) // no password

	// Con los logs en mano recalculamos el VEREDICTO: ahora la causa se confirma
	// (transporte, 4-stage por VM, opciones). Si la capacidad falla, igual
	// devolvemos el deep.
	out := map[string]any{"deep": res}
	days := in.Days
	if days <= 0 {
		days = 7
	}
	if capRes, err := analysis.JobCapacity(r.Context(), sess, in.JobID, days); err == nil {
		v := analysis.BuildVerdict(capRes, &res, s.measuredFor(r.PathValue("session"), repoIDsOf(capRes)))
		out["verdict"], out["capacity"] = v, capRes
		logVerdict(in.JobID, v)
		sess.SetAnalyzed("job:"+in.JobID, out) // el deep reemplaza al REST-only
	} else {
		log.Printf("deep analysis: no capacity context for job %s: %v", in.JobID, err)
	}
	writeJSON(w, http.StatusOK, out)
}

// deepDiskCount: discos totales que se pudieron leer de los Task logs (para
// verificar en el log que el parseo realmente saco datos).
func deepDiskCount(r deeplog.Result) int {
	n := 0
	for _, vm := range r.VMs {
		n += len(vm.Disks)
	}
	return n
}

type hostResInput struct {
	HostID string `json:"hostId"`
	Cores  int    `json:"cores"`
	RamGB  int    `json:"ramGB"`
}

// hostResources guarda (en la sesion) los cores/RAM de un host, para que el
// modelo de capacidad de recomendaciones firmes. cores=ram=0 borra el override.
func (s *Server) hostResources(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in hostResInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	if strings.TrimSpace(in.HostID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "hostId required"})
		return
	}
	sess.SetHostRes(in.HostID, vbr.HostRes{Cores: in.Cores, RamGB: in.RamGB})
	log.Printf("host resources set: host=%s cores=%d ramGB=%d", in.HostID, in.Cores, in.RamGB)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sessions: passthrough de las ultimas sesiones de jobs (util para explorar).
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "v1/sessions?limit=50&orderColumn=CreationTime&orderAsc=false")
}

// rawGet: passthrough read-only a cualquier ruta de la REST API de VBR, para
// inspeccionar el schema real (ej: /raw/v1/backupInfrastructure/repositories).
func (s *Server) rawGet(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	s.proxy(w, r, path)
}

// diagnostics arma un bundle para validar el ambiente real: lo que el tool
// RESOLVIO (proxies con tipo, repos con host/path/mount) + el grafo + el JSON
// CRUDO de cada endpoint. Sin passwords ni tokens (no viven en estos objetos).
func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rawOrErr := func(path string) any {
		raw, err := vbr.Get(ctx, sess, path)
		if err != nil {
			return map[string]string{"error": err.Error()}
		}
		return json.RawMessage(raw)
	}

	repos, proxies := s.bench.Options(ctx, sess)
	report := map[string]any{
		"meta": map[string]any{
			"host":        sess.Host,
			"apiVersion":  sess.APIVersion,
			"generatedAt": time.Now().Format(time.RFC3339),
		},
		"resolved": map[string]any{
			"proxies":      proxies,
			"repositories": repos,
		},
		"raw": map[string]any{
			"proxies":              rawOrErr("v1/backupInfrastructure/proxies?limit=1000"),
			"repositories":         rawOrErr("v1/backupInfrastructure/repositories?limit=1000"),
			"scaleOutRepositories": rawOrErr("v1/backupInfrastructure/scaleOutRepositories?limit=1000"),
			"managedServers":       rawOrErr("v1/backupInfrastructure/managedServers?limit=1000"),
			"jobs":                 rawOrErr(analysis.JobsPath),                                            // relaciones proxy->repo
			"sessions":             rawOrErr("v1/sessions?limit=10&orderColumn=CreationTime&orderAsc=false"), // analisis
		},
	}
	if g, err := topology.Build(ctx, sess); err != nil {
		report["flowError"] = err.Error()
	} else {
		report["flow"] = g
	}
	// Muestra de sesiones de DATOS (taskSessions + logs) para calibrar el modelo de
	// capacidad: hasta N jobs DISTINTOS, la corrida exitosa mas reciente de cada uno
	// (asi hay variedad de cuellos: Target/Source/Proxy/Network). "ConfigurationBackup"
	// contiene "backup" pero NO es data job -> se excluye, igual discovery/retention/etc.
	if b, err := vbr.Get(ctx, sess, "v1/sessions?limit=200&orderColumn=CreationTime&orderAsc=false"); err == nil {
		var wrap struct {
			Data []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				JobID       string `json:"jobId"`
				Type        string `json:"type"`
				SessionType string `json:"sessionType"`
				Result      struct {
					Result string `json:"result"`
				} `json:"result"`
			} `json:"data"`
		}
		isData := func(tp string) bool {
			for _, bad := range []string{"delete", "configuration", "discover", "retention", "malware", "agentmanagement", "security", "compliance", "filelevel", "flr"} {
				if strings.Contains(tp, bad) {
					return false
				}
			}
			return strings.Contains(tp, "backup") || strings.Contains(tp, "replica") ||
				strings.Contains(tp, "restore") || strings.Contains(tp, "copy")
		}
		if json.Unmarshal(b, &wrap) == nil && len(wrap.Data) > 0 {
			const maxSamples = 10
			seenJob := map[string]bool{}
			var samples []map[string]any
			for _, x := range wrap.Data {
				tp := strings.ToLower(x.Type + x.SessionType)
				res := strings.ToLower(x.Result.Result)
				if !isData(tp) || (res != "success" && res != "warning") {
					continue
				}
				if x.JobID != "" && seenJob[x.JobID] { // una por job (la mas reciente)
					continue
				}
				seenJob[x.JobID] = true
				samples = append(samples, map[string]any{
					"id": x.ID, "name": x.Name, "sessionType": x.SessionType, "result": x.Result.Result,
					"taskSessions": rawOrErr("v1/sessions/" + x.ID + "/taskSessions"),
					"logs":         rawOrErr("v1/sessions/" + x.ID + "/logs"),
				})
				if len(samples) >= maxSamples {
					break
				}
			}
			if len(samples) > 0 {
				report["sampleSessions"] = samples
				report["sampleSession"] = samples[0] // back-compat
			} else {
				// Ninguna sesion de datos exitosa: al menos mostrar la mas reciente.
				id := wrap.Data[0].ID
				report["sampleSession"] = map[string]any{
					"id":           id,
					"taskSessions": rawOrErr("v1/sessions/" + id + "/taskSessions"),
					"logs":         rawOrErr("v1/sessions/" + id + "/logs"),
				}
			}
		}
	}
	// Lo que el usuario YA analizo en esta sesion, con su veredicto calculado: con
	// esto se reproduce offline lo que vio en pantalla (calibrar reglas) sin gastar
	// mas llamadas REST. Vacio si solo genero el diagnostico.
	analyzed := sess.AnalyzedAll()
	if len(analyzed) > 0 {
		report["analyzed"] = analyzed
	}
	log.Printf("diagnostics generated: %d proxies, %d repos, %d analyzed", len(proxies), len(repos), len(analyzed))
	writeJSON(w, http.StatusOK, report)
}

// --- benchmark --------------------------------------------------------------

// baselines: catalogos de "lo esperado" (disco por tier, red por enlace).
func (s *Server) baselines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"disk": map[string]any{"data": benchmark.DiskCatalog(), "default": benchmark.DefaultDisk},
		"net":  map[string]any{"data": benchmark.NetCatalog(), "default": benchmark.DefaultNet},
	})
}

func (s *Server) benchmarkOptions(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	repos, proxies := s.bench.Options(r.Context(), sess)
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos, "proxies": proxies})
}

// benchConnection (fase 1): valida el canal al host del repositorio.
func (s *Server) benchConnection(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in benchmark.BenchConnInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	mode, res := s.bench.TestConnection(r.Context(), sess, r.PathValue("session"), in)
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": mode, "ok": res.OK, "message": res.Message, "hostname": res.Hostname,
	})
}

// benchTools (paso Herramienta): chequea si fio esta instalada en el host.
func (s *Server) benchTools(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	res, hasConn := s.bench.CheckTools(r.PathValue("session"))
	if !hasConn {
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "No connection to the host. Validate the connection first."})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// benchDeploy: despliega la herramienta si falta.
func (s *Server) benchDeploy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	res, hasConn := s.bench.DeployTools(r.PathValue("session"))
	if !hasConn {
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "No connection to the host. Validate the connection first."})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// benchmarkStart (fase 4): crea y lanza el job en segundo plano.
func (s *Server) benchmarkStart(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in benchmark.BenchmarkInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	jobID, status, code, detail := s.bench.Start(r.Context(), sess, r.PathValue("session"), in)
	if code != 0 {
		writeJSON(w, code, map[string]string{"detail": detail})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": jobID, "status": status})
}

func (s *Server) benchmarkGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	raw, sessID, found := s.bench.JobJSON(r.PathValue("job"))
	if !found || sessID != r.PathValue("session") {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Job no encontrado."})
		return
	}
	writeRaw(w, raw)
}

func (s *Server) benchmarkList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	writeRaw(w, s.bench.ListJobsJSON(r.PathValue("session")))
}
