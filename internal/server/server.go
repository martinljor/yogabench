// Package server arma el HTTP server: rutea la API y sirve el frontend embebido
// en el mismo puerto.
package server

import (
	"io/fs"
	"net/http"

	"yogabench/internal/benchmark"
	"yogabench/internal/vbr"
)

type Server struct {
	store   *vbr.Store
	bench   *benchmark.Manager
	mux     *http.ServeMux
	version string
}

// New devuelve el handler HTTP listo (con CORS). `web` es el frontend embebido;
// `version` se expone en /health para que el frontend la muestre en el brand.
func New(store *vbr.Store, web fs.FS, version string) http.Handler {
	s := &Server{store: store, bench: benchmark.NewManager(), mux: http.NewServeMux(), version: version}
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

	// Arquitectura
	m.HandleFunc("GET /api/{session}/flow", s.flow)
	m.HandleFunc("GET /api/{session}/recommendations", s.recommendations)

	// Passthrough read-only a la REST API de VBR (para inspeccionar el schema real).
	m.HandleFunc("GET /api/{session}/raw/{path...}", s.rawGet)

	// Diagnostico: bundle (resuelto + crudo) para validar el ambiente real.
	m.HandleFunc("GET /api/{session}/diagnostics", s.diagnostics)

	// Analisis (Carril B: bottleneck agregado por repo/proxy)
	m.HandleFunc("GET /api/{session}/sessions", s.sessions)
	m.HandleFunc("GET /api/{session}/analysis", s.analysis)
	m.HandleFunc("GET /api/{session}/analysis-range", s.analysisRange)
	m.HandleFunc("GET /api/{session}/analysis/jobs", s.analysisJobs)
	m.HandleFunc("GET /api/{session}/analysis/job", s.analysisJob)

	// Benchmark (Objetivo 2): baselines, opciones, ciclo de conexion y jobs.
	m.HandleFunc("GET /api/baselines", s.baselines)
	m.HandleFunc("GET /api/{session}/benchmark-options", s.benchmarkOptions)
	m.HandleFunc("POST /api/{session}/bench-connection", s.benchConnection)
	m.HandleFunc("GET /api/{session}/bench-tools", s.benchTools)
	m.HandleFunc("POST /api/{session}/bench-deploy", s.benchDeploy)
	m.HandleFunc("POST /api/{session}/benchmark", s.benchmarkStart)
	m.HandleFunc("GET /api/{session}/benchmark/{job}", s.benchmarkGet)
	m.HandleFunc("GET /api/{session}/benchmarks", s.benchmarkList)

	// Red: referencia de puertos, test de conectividad y benchmark iperf.
	m.HandleFunc("GET /api/{session}/ports", s.ports)
	m.HandleFunc("POST /api/{session}/ports-check", s.portsCheck)
	m.HandleFunc("POST /api/{session}/iperf", s.iperf)

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
