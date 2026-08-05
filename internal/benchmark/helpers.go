package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"yogabench/internal/vbr"
)

var errWinRMNotReady = errors.New(winRMNotReady)

// Claves candidatas (schema variable de VBR) para el host de un proxy/repo y
// para el mount server referenciado dentro de un repo.
var (
	proxyHostKeys = []string{"hostId", "serverId", "hostName", "server"}
	mountKeys     = []string{"mountServerId", "mountHostId", "mountServer"}
	nestedIDKeys  = []string{"mountServerId", "hostId", "serverId", "id", "name"}
)

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func roundN(f float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(f*p) / p
}

// --- acceso a la API + resolucion de host/SO (schema variable) --------------

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

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

func allRepositories(ctx context.Context, s *vbr.Session) []map[string]any {
	repos := getItems(ctx, s, "v1/backupInfrastructure/repositories?limit=1000")
	sobrs := getItems(ctx, s, "v1/backupInfrastructure/scaleOutRepositories?limit=1000")
	for _, so := range sobrs {
		if str(so["type"]) == "" {
			so["type"] = "ScaleOut"
		}
	}
	return append(repos, sobrs...)
}

// extractID prueba las claves dadas; si el valor es objeto anidado, busca dentro.
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

// hostOS: SO (windows/linux) de un managed server por su id, segun su 'type'.
func hostOS(hostID string, managed []map[string]any, def string) string {
	for _, m := range managed {
		if str(m["id"]) == hostID {
			t := strings.ToLower(str(m["type"]))
			if strings.Contains(t, "windows") || strings.Contains(t, "win") {
				return "windows"
			}
			if strings.Contains(t, "linux") {
				return "linux"
			}
		}
	}
	return def
}

func hostName(hostID string, managed []map[string]any, fallback string) string {
	for _, m := range managed {
		if str(m["id"]) == hostID {
			if n := str(m["name"]); n != "" {
				return n
			}
		}
	}
	return fallback
}

// resolveRepo: datos del repo relevantes para el benchmark (host del disco +
// carpeta real + mount server, con SO resueltos).
func resolveRepo(repo map[string]any, managed []map[string]any) RepoOption {
	hostID := extractID(repo, proxyHostKeys)
	mountID := extractID(repo, mountKeys)
	var mount *MountInfo
	if mountID != "" {
		mount = &MountInfo{ID: mountID, Name: hostName(mountID, managed, "mount server"), OS: hostOS(mountID, managed, "linux")}
	}
	name := str(repo["name"])
	if name == "" {
		name = "repository"
	}
	return RepoOption{ID: str(repo["id"]), Name: name, HostOS: hostOS(hostID, managed, "linux"),
		HostName: hostName(hostID, managed, ""), Path: repoPath(repo), Mount: mount}
}

// repoPath: carpeta local donde el repo escribe los backups. Cada repo puede
// estar en un volumen/disco distinto del mismo host, asi que el benchmark debe
// medir ESTA ruta (no una fija). El schema varia entre builds de VBR.
func repoPath(repo map[string]any) string {
	if r, ok := repo["repository"].(map[string]any); ok {
		for _, k := range []string{"path", "folder", "sharePath", "share"} {
			if p := str(r[k]); p != "" {
				return p
			}
		}
	}
	for _, k := range []string{"path", "folder"} {
		if p := str(repo[k]); p != "" {
			return p
		}
	}
	return ""
}

// resolveProxyOS: SO del proxy deducido de su host (server.hostId).
func resolveProxyOS(proxy map[string]any, managed []map[string]any) string {
	hostID := extractID(proxy, proxyHostKeys)
	def := strings.ToLower(str(proxy["os"]))
	if def == "" {
		def = "linux"
	}
	return hostOS(hostID, managed, def)
}
