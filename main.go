// Yoga Benchmark - consola web para diagnosticar cuellos de botella de Veeam.
//
// Un solo binario, sin dependencias externas (solo la stdlib de Go). Sirve la
// consola web y la API en el mismo puerto. El usuario final solo ejecuta el
// binario; no compila ni instala nada.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"yogabench/internal/dbg"
	"yogabench/internal/server"
	"yogabench/internal/vbr"
)

// version del binario (se muestra en el banner de arranque y se etiqueta en el release).
const version = "0.4.7-alpha"

func main() {
	port := flag.String("port", "8000", "puerto HTTP")
	noBrowser := flag.Bool("no-browser", false, "no abrir el navegador automaticamente (para server headless)")
	logPath := flag.String("log", "yogabench.log", "archivo de log (para diagnostico); vacio = solo consola")
	debug := flag.Bool("debug", true, "modo debug: loguea detalle (puertos, salida de iperf, llamadas REST); sin passwords")
	flag.Parse()
	dbg.On = *debug

	// Log a consola + archivo (para poder compartir el diagnostico). Best-effort:
	// si no se puede abrir el archivo, sigue solo por consola. NUNCA loguea
	// passwords ni tokens.
	if *logPath != "" {
		if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	// La FS embebida tiene todo bajo "frontend/"; la reraizamos ahi.
	web, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("frontend embebido: %v", err)
	}

	handler := server.New(vbr.NewStore(), web, version)

	addr := ":" + *port
	url := "http://localhost:" + *port

	// Banner de arranque (ingles): version + que esta corriendo + como abrirlo.
	fmt.Printf(`
==================================================
  Yoga Benchmark  v%s
  Veeam diagnostics & disk benchmark  (EXPERIMENTAL - lab use only)

  Server is running. Open in your browser:
      %s

  Press Ctrl+C to stop.
==================================================

`, version, url)
	log.Printf("Yoga Benchmark %s listening on %s (debug=%v)", version, url, *debug)

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
