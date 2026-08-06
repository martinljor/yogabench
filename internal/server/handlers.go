package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yogabench/internal/analysis"
	"yogabench/internal/benchmark"
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
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Sesion no encontrada. Reconectate a VBR."})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "cuerpo invalido"})
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
		writeErr(w, err)
		return
	}
	id := s.store.New(&vbr.Session{
		Host: req.Host, Port: req.Port, APIVersion: req.APIVersion, VerifySSL: req.VerifySSL,
		AccessToken: access, RefreshToken: refresh, CreatedAt: time.Now(),
	})
	log.Printf("VBR conectado: host=%s puerto=%d apiVersion=%s", req.Host, req.Port, req.APIVersion) // sin password/token
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
			"jobs":                 rawOrErr("v1/jobs?limit=500"),                                            // relaciones proxy->repo
			"sessions":             rawOrErr("v1/sessions?limit=10&orderColumn=CreationTime&orderAsc=false"), // analisis
		},
	}
	if g, err := topology.Build(ctx, sess); err != nil {
		report["flowError"] = err.Error()
	} else {
		report["flow"] = g
	}
	// Muestra de UNA sesion (taskSessions + logs): para diagnosticar por que el
	// analisis no encuentra bottleneck / repo / proxy en este ambiente.
	if b, err := vbr.Get(ctx, sess, "v1/sessions?limit=25&orderColumn=CreationTime&orderAsc=false"); err == nil {
		var wrap struct {
			Data []struct {
				ID          string `json:"id"`
				Type        string `json:"type"`
				SessionType string `json:"sessionType"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &wrap) == nil && len(wrap.Data) > 0 {
			id := wrap.Data[0].ID
			// Preferir una sesion de datos (backup/replica/restore/copy) — las de
			// descubrimiento/retention no traen taskSessions ni bottleneck.
			for _, x := range wrap.Data {
				tp := strings.ToLower(x.Type + x.SessionType)
				if strings.Contains(tp, "backup") || strings.Contains(tp, "replica") ||
					strings.Contains(tp, "restore") || strings.Contains(tp, "copy") {
					id = x.ID
					break
				}
			}
			report["sampleSession"] = map[string]any{
				"id":           id,
				"taskSessions": rawOrErr("v1/sessions/" + id + "/taskSessions"),
				"logs":         rawOrErr("v1/sessions/" + id + "/logs"),
			}
		}
	}
	log.Printf("diagnostico generado: %d proxies, %d repos", len(proxies), len(repos))
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "cuerpo invalido"})
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
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "No hay conexion al host. Valida la conexion primero."})
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
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "No hay conexion al host. Valida la conexion primero."})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "cuerpo invalido"})
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
