package main

import "embed"

// El frontend (HTML/JS) se embebe dentro del binario: un solo ejecutable, sin
// archivos sueltos. Se sirve desde el mismo puerto que la API.
//
//go:embed frontend
var frontendFS embed.FS
