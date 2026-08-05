package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"yogabench/internal/vbr"
)

const emptyGUID = "00000000-0000-0000-0000-000000000000"

// Nombres candidatos donde puede venir el host de un proxy / el mount server,
// segun el build de VBR. Y sub-claves para ids anidados.
var (
	mountKeys    = []string{"mountServerId", "mountHostId", "mountServer"}
	nestedIDKeys = []string{"mountServerId", "hostId", "serverId", "id", "name"}
)

// --- acceso a la API + decode ----------------------------------------------

func getItems(ctx context.Context, s *vbr.Session, path string) ([]map[string]any, error) {
	raw, err := vbr.Get(ctx, s, path)
	if err != nil {
		return nil, err
	}
	return decodeItems(raw), nil
}

func decodeItems(raw json.RawMessage) []map[string]any {
	var wrap struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.Data != nil {
		return wrap.Data
	}
	var arr []map[string]any
	_ = json.Unmarshal(raw, &arr)
	return arr
}

// AllRepositories: repos normales + scale-out (SOBR), unificados.
func AllRepositories(ctx context.Context, s *vbr.Session) ([]map[string]any, error) {
	repos, err := getItems(ctx, s, "v1/backupInfrastructure/repositories?limit=1000")
	if err != nil {
		return nil, err
	}
	sobrs, _ := getItems(ctx, s, "v1/backupInfrastructure/scaleOutRepositories?limit=1000") // opcional
	for _, so := range sobrs {
		if str(so["type"]) == "" {
			so["type"] = "ScaleOut"
		}
	}
	return append(repos, sobrs...), nil
}

// --- helpers de datos (schema variable de VBR) ------------------------------

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func strOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

// findKey busca (recursivo, case-insensitive) el primer valor de una clave.
func findKey(obj any, key string) any {
	switch v := obj.(type) {
	case map[string]any:
		for k, val := range v {
			if strings.EqualFold(k, key) {
				return val
			}
		}
		for _, val := range v {
			if r := findKey(val, key); r != nil {
				return r
			}
		}
	case []any:
		for _, val := range v {
			if r := findKey(val, key); r != nil {
				return r
			}
		}
	}
	return nil
}

// extractID prueba las claves dadas; si el valor es un objeto anidado, busca
// dentro por las sub-claves id-esas.
func extractID(obj map[string]any, keys []string) string {
	for _, k := range keys {
		switch v := obj[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			for _, sk := range nestedIDKeys {
				if s := str(v[sk]); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// findProxyIDs junta (recursivo) los arrays "proxyIds" de la config del job.
func findProxyIDs(obj any) []string {
	var out []string
	switch v := obj.(type) {
	case map[string]any:
		for k, val := range v {
			if strings.EqualFold(k, "proxyids") {
				if arr, ok := val.([]any); ok {
					for _, x := range arr {
						if s := str(x); s != "" {
							out = append(out, s)
						}
					}
				}
			} else {
				out = append(out, findProxyIDs(val)...)
			}
		}
	case []any:
		for _, val := range v {
			out = append(out, findProxyIDs(val)...)
		}
	}
	return out
}

// mountServerID resuelve el mount server de un repo, tolerante al schema (v12/v13):
//  1. clave explicita (mountServerId / mountServer anidado)
//  2. mountServerId en cualquier nivel (recursivo)
//  3. fallback: el propio host del repo (Veeam usa el host del repo como mount
//     server cuando no se especifica otro)
func mountServerID(repo map[string]any) string {
	if mid := extractID(repo, mountKeys); mid != "" {
		return mid
	}
	if mid := str(findKey(repo, "mountServerId")); mid != "" {
		return mid
	}
	return str(findKey(repo, "hostId"))
}

func normalizeRole(role string) string {
	r := strings.ToLower(role)
	switch {
	case strings.Contains(r, "proxy"):
		return "proxy"
	case strings.Contains(r, "repo"):
		return "repository"
	case strings.Contains(r, "mount"):
		return "mount-server"
	case strings.Contains(r, "gateway"):
		return "gateway"
	case strings.Contains(r, "vbr"), strings.Contains(r, "backup"):
		return "backup-server"
	default:
		return "managed-server"
	}
}

func labelFor(managed []map[string]any, id, fallback string) string {
	for _, m := range managed {
		if str(m["id"]) == id {
			return strOr(m["name"], fallback)
		}
	}
	return fallback
}

// tareas simultaneas del proxy (server.maxTaskCount).
func proxyTasksInfo(p map[string]any) string {
	if f, ok := findKey(p, "maxTaskCount").(float64); ok && f > 0 {
		return fmt.Sprintf("%d tareas simult.", int(f))
	}
	return ""
}

// tareas del repo: maxTaskCount si taskLimitEnabled; si esta deshabilitado, "sin limite".
func repoTasksInfo(r map[string]any) string {
	if b, ok := findKey(r, "taskLimitEnabled").(bool); ok && !b {
		return "tareas sin limite"
	}
	if f, ok := findKey(r, "maxTaskCount").(float64); ok && f > 0 {
		return fmt.Sprintf("%d tareas simult.", int(f))
	}
	return ""
}
