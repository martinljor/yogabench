// Package server arma el HTTP server: rutea la API y sirve el frontend embebido
// en el mismo puerto.
package server

import (
	"io/fs"
	"net/http"

	"yogabench/internal/vbr"
)

type Server struct {
	store *vbr.Store
	mux   *http.ServeMux
}

// New devuelve el handler HTTP listo (con CORS). `web` es el frontend embebido.
func New(store *vbr.Store, web fs.FS) http.Handler {
	s := &Server{store: store, mux: http.NewServeMux()}
	s.routes(web)
	return cors(s.mux)
}

func (s *Server) routes(web fs.FS) {
	m := s.mux

	// Salud
	m.HandleFunc("GET /health", s.health)

	// Conexion / sesion
	m.HandleFunc("POST /api/connect", s.connect)
	m.HandleFunc("POST /api/connect-demo", s.connectDemo)
	m.HandleFunc("POST /api/{session}/disconnect", s.disconnect)

	// Infraestructura (lecturas)
	m.HandleFunc("GET /api/{session}/proxies", s.proxies)
	m.HandleFunc("GET /api/{session}/repositories", s.repositories)
	m.HandleFunc("GET /api/{session}/managed-servers", s.managedServers)

	// Frontend embebido (catch-all; las rutas /api y /health tienen prioridad).
	m.Handle("GET /", http.FileServer(http.FS(web)))
}

// cors permite cualquier origen (util para el HTML suelto en desarrollo). Como
// el backend sirve el frontend en el mismo origen, en produccion se puede cerrar.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
