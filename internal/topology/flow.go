// Package topology arma el grafo de arquitectura (proxy -> repositorio -> mount
// server) desde la REST API de VBR, con roles, tareas simultaneas y las
// conexiones proxy->repo derivadas de la config de los jobs.
package topology

import (
	"context"
	"strings"

	"yogabench/internal/vbr"
)

type Node struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Role  string `json:"role"`
	Info  string `json:"info,omitempty"`
	// Kind: sub-type within the role, so the diagram can tell a local repository
	// from object storage, a SOBR or an immutable one — they behave differently and
	// the advice differs. Empty when the role has no sub-types.
	Kind string `json:"kind,omitempty"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	Auto bool   `json:"auto,omitempty"`
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// builder mantiene el orden de insercion y un indice por id (para promocionar
// roles, ej: un managed-server que resulta ser el mount server de un repo).
type builder struct {
	order []*Node
	byID  map[string]*Node
	edges []Edge
}

func (b *builder) add(id, label, role, info string) {
	if id == "" || b.byID[id] != nil {
		return
	}
	n := &Node{ID: id, Label: label, Role: role, Info: info}
	b.byID[id] = n
	b.order = append(b.order, n)
}

// setKind marca el sub-tipo del nodo (ver Node.Kind).
func (b *builder) setKind(id, kind string) {
	if n := b.byID[id]; n != nil && kind != "" {
		n.Kind = kind
	}
}

// repoKind clasifica el repositorio por lo que cambia el consejo: object storage
// (sin limite de tareas, latencia distinta), SOBR (extents), inmutable, o local.
func repoKind(r map[string]any) string {
	t := strings.ToLower(str(r["type"]))
	switch {
	case strings.Contains(t, "scaleout"), strings.Contains(t, "sobr"):
		return "sobr"
	case strings.Contains(t, "s3"), strings.Contains(t, "object"), strings.Contains(t, "blob"),
		strings.Contains(t, "vault"), strings.Contains(t, "amazon"), strings.Contains(t, "azure"),
		strings.Contains(t, "google"), strings.Contains(t, "smart"):
		if repoImmutable(r) {
			return "object-immutable"
		}
		return "object"
	default:
		if repoImmutable(r) {
			return "local-immutable"
		}
		return "local"
	}
}

// repoImmutable: la inmutabilidad vive en bucket/container segun el tipo.
func repoImmutable(r map[string]any) bool {
	for _, k := range []string{"bucket", "container"} {
		if m, ok := r[k].(map[string]any); ok {
			if imm, ok := m["immutability"].(map[string]any); ok {
				if on, _ := imm["isEnabled"].(bool); on {
					return true
				}
			}
		}
	}
	return false
}

func (b *builder) promote(id, role string) {
	if n := b.byID[id]; n != nil && n.Role == "managed-server" {
		n.Role = role
	}
}

// Build arma el grafo. Solo lectura.
func Build(ctx context.Context, s *vbr.Session) (Graph, error) {
	proxies, err := getItems(ctx, s, "v1/backupInfrastructure/proxies?limit=1000")
	if err != nil {
		return Graph{}, err
	}
	repos, err := AllRepositories(ctx, s)
	if err != nil {
		return Graph{}, err
	}
	managed, err := getItems(ctx, s, "v1/backupInfrastructure/managedServers?limit=1000")
	if err != nil {
		return Graph{}, err
	}

	b := &builder{byID: map[string]*Node{}}

	// Managed servers = contexto: se agregan para permitir promocion de rol y
	// resolucion de nombres, pero se filtran del resultado final.
	for _, m := range managed {
		role := normalizeRole(strOr(m["type"], str(m["role"])))
		b.add(str(m["id"]), strOr(m["name"], "server"), role, "")
	}

	for _, p := range proxies {
		b.add(str(p["id"]), strOr(p["name"], "proxy"), "proxy", proxyTasksInfo(p))
	}

	for _, r := range repos {
		rid := str(r["id"])
		b.add(rid, strOr(r["name"], "repository"), "repository", repoTasksInfo(r))
		b.setKind(rid, repoKind(r))
		if mid := mountServerID(r); mid != "" {
			b.add(mid, labelFor(managed, mid, "mount server"), "mount-server", "")
			b.promote(mid, "mount-server")
			b.edges = append(b.edges, Edge{From: rid, To: mid, Kind: "mount"})
		}
	}

	buildJobEdges(ctx, s, proxies, b)

	// Ocultar los managed-server de contexto (hosts sueltos): quedan solo los
	// roles funcionales.
	nodes := make([]Node, 0, len(b.order))
	for _, n := range b.order {
		if n.Role != "managed-server" {
			nodes = append(nodes, *n)
		}
	}
	return Graph{Nodes: nodes, Edges: b.edges}, nil
}

// buildJobEdges deriva proxy->repo de la config de cada job:
//   - proxy asignado explicito -> arista solida (auto=false)
//   - proxy automatico ([]/None) -> a TODOS los proxies elegibles (auto=true)
func buildJobEdges(ctx context.Context, s *vbr.Session, proxies []map[string]any, b *builder) {
	var allProxyIDs []string
	for _, p := range proxies {
		if id := str(p["id"]); id != "" {
			allProxyIDs = append(allProxyIDs, id)
		}
	}

	jobs, _ := getItems(ctx, s, jobsPath) // si /jobs falla, sin edges (no rompe)

	edgeAuto := map[[2]string]bool{}
	add := func(from, to string, auto bool) {
		if from == "" || to == "" || to == emptyGUID {
			return
		}
		k := [2]string{from, to}
		if _, ok := edgeAuto[k]; !ok || !auto { // el explicito (auto=false) siempre gana
			edgeAuto[k] = auto
		}
	}

	for _, j := range jobs {
		repoID := str(findKey(j, "backupRepositoryId"))
		if repoID == "" {
			repoID = str(findKey(j, "repositoryId"))
		}
		if repoID == "" || repoID == emptyGUID {
			continue
		}
		if pids := findProxyIDs(j); len(pids) > 0 {
			for _, pid := range pids {
				add(pid, repoID, false)
			}
		} else {
			for _, pid := range allProxyIDs {
				add(pid, repoID, true)
			}
		}
	}

	for k, auto := range edgeAuto {
		b.edges = append(b.edges, Edge{From: k[0], To: k[1], Kind: "writes-to", Auto: auto})
	}
}
