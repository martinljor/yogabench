package analysis

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"yogabench/internal/vbr"
)

// --- acceso a la API (best-effort: si falla, devuelve vacio) ----------------

func getItems(ctx context.Context, s *vbr.Session, path string) []map[string]any {
	raw, err := vbr.Get(ctx, s, path)
	if err != nil {
		return nil
	}
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

// allRepositories: repos normales + scale-out (SOBR), unificados.
func allRepositories(ctx context.Context, s *vbr.Session) []map[string]any {
	repos := getItems(ctx, s, "v1/backupInfrastructure/repositories?limit=1000")
	sobrs := getItems(ctx, s, "v1/backupInfrastructure/scaleOutRepositories?limit=1000")
	return append(repos, sobrs...)
}

// jobProxyMap: por jobId, los proxies asignados (config del job). Auto = vacio.
func jobProxyMap(ctx context.Context, s *vbr.Session) map[string][]string {
	m := map[string][]string{}
	for _, j := range getItems(ctx, s, "v1/jobs?limit=1000") {
		if id := str(j["id"]); id != "" {
			m[id] = cleanIDs(findProxyIDs(j))
		}
	}
	return m
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
						if id := str(x); id != "" {
							out = append(out, id)
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

// --- clasificacion de sesiones ---------------------------------------------

func isDataJob(sess map[string]any) bool {
	t := strings.ToLower(strOr(sess["type"], str(sess["sessionType"])))
	for _, bad := range skipHints {
		if strings.Contains(t, bad) {
			return false
		}
	}
	for _, ok := range jobHints {
		if strings.Contains(t, ok) {
			return true
		}
	}
	return false
}

func resultOf(sess map[string]any) string {
	if res, ok := sess["result"].(map[string]any); ok {
		return str(res["result"])
	}
	return ""
}

func nameMap(items []map[string]any) map[string]string {
	m := make(map[string]string, len(items))
	for _, it := range items {
		if id := str(it["id"]); id != "" {
			m[id] = str(it["name"])
		}
	}
	return m
}

// --- helpers genericos ------------------------------------------------------

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

func first(items []map[string]any) map[string]any {
	if len(items) > 0 {
		return items[0]
	}
	return map[string]any{}
}

func num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func parseDT(v any) (time.Time, bool) {
	s := str(v)
	if s == "" {
		return time.Time{}, false
	}
	s = strings.SplitN(s, "+", 2)[0]
	s = strings.SplitN(s, ".", 2)[0]
	s = strings.TrimSuffix(s, "Z")
	t, err := time.Parse("2006-01-02T15:04:05", s)
	return t, err == nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// cleanIDs quita vacios, el GUID nulo y duplicados (preservando orden).
func cleanIDs(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id == "" || id == emptyGUID || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
