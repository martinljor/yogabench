// Package dbg: logging de depuracion global, gateado por un unico flag.
// En alpha viene activado (--debug), para que el log capture el detalle
// necesario para diagnosticar (salida cruda de iperf, puertos, llamadas REST).
// NUNCA se loguean passwords ni tokens.
package dbg

import (
	"log"
	"strings"
)

// On habilita el logging de depuracion. Lo setea main desde el flag --debug.
var On bool

// Logf escribe una linea "[debug] ..." solo si el modo debug esta activo.
func Logf(format string, a ...any) {
	if On {
		log.Printf("[debug] "+format, a...)
	}
}

// Clip recorta un texto largo (ej: salida cruda de un comando) para el log,
// colapsando saltos de linea, con un maximo de n caracteres.
func Clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " ⏎ "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
