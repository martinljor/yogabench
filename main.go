// Yoga Benchmark - consola web para diagnosticar cuellos de botella de Veeam.
//
// Un solo binario, sin dependencias externas (solo la stdlib de Go). Sirve la
// consola web y la API en el mismo puerto. El usuario final solo ejecuta el
// binario; no compila ni instala nada.
package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"

	"yogabench/internal/server"
	"yogabench/internal/vbr"
)

func main() {
	port := flag.String("port", "8000", "puerto HTTP")
	flag.Parse()

	// La FS embebida tiene todo bajo "frontend/"; la reraizamos ahi.
	web, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("frontend embebido: %v", err)
	}

	handler := server.New(vbr.NewStore(), web)

	addr := ":" + *port
	log.Printf("Yoga Benchmark escuchando en http://localhost%s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}
