package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active_sessions": s.store.Count()})
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
		req.APIVersion = "1.2-rev1"
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
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "expires_in": expiresIn})
}

func (s *Server) connectDemo(w http.ResponseWriter, r *http.Request) {
	id := s.store.New(&vbr.Session{Demo: true, Host: "demo-vbr"})
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "expires_in": 3600})
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	s.store.Delete(r.PathValue("session"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) proxies(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "v1/backupInfrastructure/proxies")
}

func (s *Server) repositories(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "v1/backupInfrastructure/repositories")
}

func (s *Server) managedServers(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "v1/backupInfrastructure/managedServers")
}
