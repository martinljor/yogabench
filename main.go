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
	"os/exec"
	"runtime"
	"time"

	"yogabench/internal/server"
	"yogabench/internal/vbr"
)

func main() {
	port := flag.String("port", "8000", "puerto HTTP")
	noBrowser := flag.Bool("no-browser", false, "no abrir el navegador automaticamente (para server headless)")
	flag.Parse()

	// La FS embebida tiene todo bajo "frontend/"; la reraizamos ahi.
	web, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("frontend embebido: %v", err)
	}

	handler := server.New(vbr.NewStore(), web)

	addr := ":" + *port
	url := "http://localhost:" + *port
	log.Printf("Yoga Benchmark escuchando en %s", url)

	// Abrir el navegador solo (en desktop). En un server sin GUI, --no-browser.
	if !*noBrowser {
		go func() {
			time.Sleep(600 * time.Millisecond)
			openBrowser(url)
		}()
	}

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

// openBrowser abre la URL en el navegador por defecto del sistema. Best-effort:
// si no hay GUI (server headless), falla silencioso y el server sigue igual.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default: // linux, etc.
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
