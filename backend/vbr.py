"""
Capa 2 - Cliente VBR + gestion de sesiones + armado de topologia.
------------------------------------------------------------------
Este modulo concentra TODO lo que habla con la REST API de Veeam. El resto
del backend (main.py, benchmark.py) nunca ve una password ni un token: solo
manejan un session_id opaco.

Modo demo: una sesion puede marcarse como demo=True. En ese caso NO se hace
ninguna llamada HTTP; las lecturas devuelven una topologia de ejemplo. Sirve
para probar todo el prototipo (arquitectura + benchmark) sin un VBR real.
"""

import re
import uuid
from datetime import datetime, timedelta
from typing import Optional

import httpx
from fastapi import HTTPException

# --- "Vault" en memoria para el prototipo -----------------------------------
# ADVERTENCIA: solo para probar. En una version real esto se reemplaza por
# Vault/Key Vault y el token no vive en memoria de un proceso sin cifrado.
SESSIONS: dict[str, dict] = {}


def _base_url(host: str, port: int) -> str:
    return f"https://{host}:{port}/api"


def get_session(session_id: str) -> dict:
    session = SESSIONS.get(session_id)
    if not session:
        raise HTTPException(status_code=404, detail="Sesion no encontrada. Reconectate a VBR.")
    return session


async def connect(host: str, port: int, username: str, password: str,
                  api_version: str, verify_ssl: bool) -> dict:
    """Autentica contra VBR (OAuth2 password grant) y guarda la sesion."""
    url = f"{_base_url(host, port)}/oauth2/token"
    headers = {
        "Content-Type": "application/x-www-form-urlencoded",
        "x-api-version": api_version,
    }
    data = {"grant_type": "password", "username": username, "password": password}

    async with httpx.AsyncClient(verify=verify_ssl, timeout=15) as client:
        try:
            resp = await client.post(url, headers=headers, data=data)
        except httpx.ConnectError as e:
            raise HTTPException(status_code=502, detail=f"No se pudo conectar a {host}:{port} - {e}")

    if resp.status_code != 200:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)

    payload = resp.json()
    session_id = str(uuid.uuid4())
    SESSIONS[session_id] = {
        "demo": False,
        "host": host,
        "port": port,
        "api_version": api_version,
        "verify_ssl": verify_ssl,
        "access_token": payload["access_token"],
        "refresh_token": payload.get("refresh_token"),
    }
    return {"session_id": session_id, "expires_in": payload.get("expires_in", 0)}


def connect_demo() -> dict:
    """Crea una sesion demo respaldada por topologia de ejemplo (sin VBR)."""
    session_id = str(uuid.uuid4())
    SESSIONS[session_id] = {"demo": True, "host": "demo-vbr", "port": 0}
    return {"session_id": session_id, "expires_in": 3600}


async def vbr_get(session: dict, path: str) -> dict:
    """GET contra VBR, o topologia de ejemplo si la sesion es demo."""
    if session.get("demo"):
        return _demo_response(path)

    url = f"{_base_url(session['host'], session['port'])}/{path}"
    headers = {
        "Authorization": f"Bearer {session['access_token']}",
        "x-api-version": session["api_version"],
    }
    async with httpx.AsyncClient(verify=session["verify_ssl"], timeout=20) as client:
        resp = await client.get(url, headers=headers)
    if resp.status_code == 401:
        raise HTTPException(status_code=401, detail="Token expirado. Reconectate.")
    if resp.status_code != 200:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    return resp.json()


def _items(payload) -> list:
    """La REST API a veces envuelve en {data: [...]} y a veces devuelve lista."""
    if isinstance(payload, dict):
        return payload.get("data", [])
    if isinstance(payload, list):
        return payload
    return []


# --- Armado de topologia dinamica (Objetivo 1) ------------------------------
# En VBR los roles relevantes para el benchmark de backup/restore son:
#   proxy       -> mueve los datos
#   repository  -> target donde se escribe
#   mount-server-> monta backups en restore / repos que lo requieren
#   gateway     -> puerta de enlace para repos SMB/dedup
# Mount server y gateway NO son objetos de infraestructura propios: son
# referencias DENTRO del repositorio (mountServer / gateway server). Por eso
# los descubrimos leyendo esos sub-objetos, sin asumir un unico nombre de
# campo (el schema varia entre builds de VBR).

# Nombres candidatos donde puede venir el host de un proxy segun el build.
_PROXY_HOST_KEYS = ["hostId", "serverId", "hostName", "server"]
# Nombres candidatos para la referencia al mount server dentro de un repo.
_MOUNT_KEYS = ["mountServerId", "mountHostId", "mountServer"]


# Sub-claves donde puede venir el id real cuando el valor es un objeto anidado.
# Ej (schema real de VBR v12): repo.mountServer = {"mountServerId": "..."};
#                              proxy.server    = {"hostId": "..."}.
_NESTED_ID_KEYS = ["mountServerId", "hostId", "serverId", "id", "name"]


def _extract_id(obj, keys) -> Optional[str]:
    for k in keys:
        val = obj.get(k) if isinstance(obj, dict) else None
        if isinstance(val, dict):
            val = next((val[sk] for sk in _NESTED_ID_KEYS if val.get(sk)), None)
        if val:
            return val
    return None


async def build_flow(session: dict) -> dict:
    """
    Devuelve {nodes, edges} representando el camino de datos del backup:
        proxy -> repository -> mount-server
    Es defensivo frente al schema real: si un campo no existe, simplemente no
    dibuja esa relacion en lugar de romper.
    """
    proxies = _items(await vbr_get(session, "v1/backupInfrastructure/proxies"))
    repos = _items(await vbr_get(session, "v1/backupInfrastructure/repositories"))
    managed = _items(await vbr_get(session, "v1/backupInfrastructure/managedServers"))

    nodes = []
    edges = []
    by_id = {}

    def add_node(node_id, label, role, raw=None):
        if not node_id:
            return
        if node_id in by_id:
            return
        node = {"id": node_id, "label": label, "role": role, "raw": raw or {}}
        by_id[node_id] = node
        nodes.append(node)

    def promote_role(node_id, role):
        """Un host descubierto como managed-server puede cumplir un rol
        funcional (ej: ser el mount server de un repo). Ese rol tiene
        prioridad para la vista de arquitectura del benchmark."""
        node = by_id.get(node_id)
        if node and node["role"] == "managed-server":
            node["role"] = role

    # Managed servers = hosts fisicos base (contexto). Clasificamos por type/role.
    for m in managed:
        role = m.get("type") or m.get("role") or "managed-server"
        add_node(m.get("id"), m.get("name", "server"), _normalize_role(role), m)

    for p in proxies:
        pid = p.get("id")
        add_node(pid, p.get("name", "proxy"), "proxy", p)
        host = _extract_id(p, _PROXY_HOST_KEYS)
        if host and host in by_id:
            edges.append({"from": host, "to": pid, "kind": "runs-on"})

    for r in repos:
        rid = r.get("id")
        add_node(rid, r.get("name", "repository"), "repository", r)

        # proxy -> repo: camino de escritura del backup. Sin un campo fiable
        # que lo declare, conectamos todos los proxies al repo (topologia
        # "cualquier proxy puede escribir a cualquier repo", que es lo comun).
        for p in proxies:
            edges.append({"from": p.get("id"), "to": rid, "kind": "writes-to"})

        # repo -> mount server (rol relevante en restore).
        mount_id = _extract_id(r, _MOUNT_KEYS)
        if mount_id:
            add_node(mount_id, _label_for(managed, mount_id, "mount server"),
                     "mount-server")
            promote_role(mount_id, "mount-server")
            edges.append({"from": rid, "to": mount_id, "kind": "mount"})

    return {"nodes": nodes, "edges": edges}


def host_os(host_id: str, managed_items: list, default: str = "linux") -> str:
    """SO (windows/linux) de un managed server por su id, segun su 'type'."""
    for m in managed_items:
        if m.get("id") == host_id:
            t = (m.get("type") or "").lower()
            if "windows" in t or "win" in t:
                return "windows"
            if "linux" in t:
                return "linux"
    return default


def host_name(host_id: str, managed_items: list, fallback: str = "host") -> str:
    for m in managed_items:
        if m.get("id") == host_id:
            return m.get("name", fallback)
    return fallback


def resolve_proxy_os(proxy: dict, managed_items: list) -> str:
    """SO del proxy: su host (`server.hostId`) no trae 'os', se deduce del host."""
    host_id = _extract_id(proxy, _PROXY_HOST_KEYS)
    return host_os(host_id, managed_items, default=(proxy.get("os") or "linux").lower())


def resolve_repo(repo: dict, managed_items: list) -> dict:
    """Datos del repo relevantes para el benchmark: host (donde esta el disco)
    y mount server (rol de restore), con sus SO resueltos."""
    host_id = _extract_id(repo, _PROXY_HOST_KEYS)  # repos usan 'hostId' top-level
    mount_id = _extract_id(repo, _MOUNT_KEYS)
    mount = None
    if mount_id:
        mount = {"id": mount_id, "name": host_name(mount_id, managed_items, "mount server"),
                 "os": host_os(mount_id, managed_items)}
    return {
        "id": repo.get("id"),
        "name": repo.get("name", "repository"),
        "host_os": host_os(host_id, managed_items),
        "mount": mount,
    }


def _normalize_role(role: str) -> str:
    r = (role or "").lower()
    if "proxy" in r:
        return "proxy"
    if "repo" in r:
        return "repository"
    if "mount" in r:
        return "mount-server"
    if "gateway" in r:
        return "gateway"
    if "vbr" in r or "backup" in r:
        return "backup-server"
    return "managed-server"


def _label_for(managed, node_id, fallback) -> str:
    for m in managed:
        if m.get("id") == node_id:
            return m.get("name", fallback)
    return fallback


# --- Carril B: analisis de bottleneck a partir de la telemetria de Veeam -----
# El desglose viene en el log de la sesion como:
#   "Load: Source 99% > Proxy 36% > Network 2% > Target 0%"
#   "Primary bottleneck: Source"
_LOAD_RE = re.compile(
    r"Source\s+(\d+)%\s*>\s*Proxy\s+(\d+)%\s*>\s*Network\s+(\d+)%\s*>\s*Target\s+(\d+)%", re.I)
_PRIMARY_RE = re.compile(r"Primary bottleneck:\s*(\w+)", re.I)

# Tipos de sesion que mueven datos (tienen bottleneck). El resto se ignora.
_JOB_TYPE_HINTS = ("backup", "replica", "restore", "copy")
# ...pero descartamos los que dicen "backup" y NO mueven datos de VMs.
_SKIP_TYPE_HINTS = ("configuration", "malware", "compliance", "infrastructure")
# GUID vacio que devuelve VBR cuando no hay recurso asignado (ej: job fallido).
_EMPTY_GUID = "00000000-0000-0000-0000-000000000000"


def _clean_ids(ids: list) -> list:
    return [i for i in ids if i and i != _EMPTY_GUID]


def _is_data_job(s: dict) -> bool:
    t = (s.get("sessionType") or "").lower()
    return any(h in t for h in _JOB_TYPE_HINTS) and not any(h in t for h in _SKIP_TYPE_HINTS)


def _parse_dt(s: Optional[str]) -> Optional[datetime]:
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "").split("+")[0].split(".")[0])
    except ValueError:
        return None


def _range_of(sessions: list) -> dict:
    dts = [d for d in (_parse_dt(s.get("creationTime")) for s in sessions if _is_data_job(s)) if d]
    if not dts:
        return {"from": None, "to": None, "days_available": 0}
    lo, hi = min(dts), max(dts)
    return {"from": lo.date().isoformat(), "to": hi.date().isoformat(),
            "days_available": (hi.date() - lo.date()).days + 1}


async def analysis_range(session: dict) -> dict:
    """Rango de dias con info disponible (barato: una sola llamada)."""
    sess = _items(await vbr_get(
        session, "v1/sessions?limit=200&orderColumn=CreationTime&orderAsc=false"))
    return _range_of(sess)


async def build_analysis(session: dict, days: Optional[int] = None,
                         max_sessions: int = 40) -> dict:
    """Estadistica agregada por REPOSITORIO y por PROXY sobre una ventana de dias.
    Solo lectura (compatible con appliances). Cada grupo trae los jobs para
    poder expandir el detalle."""
    sess = _items(await vbr_get(
        session, "v1/sessions?limit=200&orderColumn=CreationTime&orderAsc=false"))
    rng = _range_of(sess)
    data_sess = [s for s in sess if _is_data_job(s)]
    if days:
        cutoff = datetime.now() - timedelta(days=int(days))
        data_sess = [s for s in data_sess
                     if (_parse_dt(s.get("creationTime")) or datetime.min) >= cutoff]
    data_sess = data_sess[:max_sessions]

    repos = {r.get("id"): r.get("name")
             for r in _items(await vbr_get(session, "v1/backupInfrastructure/repositories"))}
    proxies = {p.get("id"): p.get("name")
               for p in _items(await vbr_get(session, "v1/backupInfrastructure/proxies"))}
    job_proxies = await _job_proxy_map(session)

    records = []
    for s in data_sess:
        sid = s.get("id")
        res = s.get("result") or {}
        # Omitimos los fallidos: solo analizamos runs que ejecutaron bien.
        if (res.get("result") or "") == "Failed":
            continue
        tasks = _analysis_tasks(await _safe_get(session, f"v1/sessions/{sid}/taskSessions"))
        stype = s.get("sessionType") or ""
        records.append({
            "id": sid,
            "name": s.get("name"),
            "type": stype,
            "operation": "restore" if "restore" in stype.lower() else "backup",
            "result": res.get("result"),
            "message": res.get("message"),
            "creationTime": s.get("creationTime"),
            "endTime": s.get("endTime"),
            "bottleneck": _analysis_bottleneck(
                (await _safe_get(session, f"v1/sessions/{sid}/logs")).get("records", [])),
            "tasks": tasks,
            "processedSize": sum(t["processedSize"] for t in tasks),
            "transferredSize": sum(t["transferredSize"] for t in tasks),
            "repoIds": _clean_ids(sorted({t.get("repositoryId") for t in tasks})),
            "proxyIds": _clean_ids(job_proxies.get(s.get("jobId"), [])),
        })

    return {
        "range": rng,
        "days": days,
        "byRepository": _aggregate(records, lambda r: r["repoIds"], repos, "(sin repositorio)"),
        "byProxy": _aggregate(records, lambda r: r["proxyIds"], proxies, "(sin proxy identificado)"),
    }


async def _job_proxy_map(session: dict) -> dict:
    """jobId -> lista de proxyIds configurados (leido de /jobs)."""
    jobs = _items(await _safe_get(session, "v1/jobs"))
    return {j.get("id"): _find_proxyids(j) for j in jobs}


def _find_proxyids(obj) -> list:
    """Busca recursivamente arrays 'proxyIds' en la config del job."""
    found = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            if k.lower() == "proxyids" and isinstance(v, list):
                found += v
            else:
                found += _find_proxyids(v)
    elif isinstance(obj, list):
        for x in obj:
            found += _find_proxyids(x)
    return found


def _aggregate(records: list, ids_fn, name_map: dict, unknown: str) -> list:
    groups = {}
    for r in records:
        for gid in (ids_fn(r) or [unknown]):
            groups.setdefault(gid, []).append(r)
    out = [_group_stats(gid, name_map.get(gid, unknown if gid == unknown else gid), recs)
           for gid, recs in groups.items()]
    out.sort(key=lambda g: -g["runs"])
    return out


def _group_stats(gid, name, recs: list) -> dict:
    counts = {"Success": 0, "Warning": 0, "Failed": 0}
    for r in recs:
        counts[r["result"]] = counts.get(r["result"], 0) + 1
    bns = [r["bottleneck"] for r in recs
           if r.get("bottleneck") and r["bottleneck"].get("source") is not None]
    bavg = None
    if bns:
        bavg = {k: round(sum(b[k] for b in bns) / len(bns))
                for k in ("source", "proxy", "network", "target")}
        bavg["primary"] = max(("source", "proxy", "network", "target"),
                              key=lambda k: bavg[k]).capitalize()
    primary_counts = {}
    for r in recs:
        p = (r.get("bottleneck") or {}).get("primary")
        if p:
            primary_counts[p] = primary_counts.get(p, 0) + 1
    return {
        "id": gid, "name": name or "(sin nombre)", "runs": len(recs),
        "results": counts,
        "processedSize": sum(r["processedSize"] for r in recs),
        "transferredSize": sum(r["transferredSize"] for r in recs),
        "bottleneckAvg": bavg, "primaryCounts": primary_counts,
        "jobs": recs,
    }


async def _safe_get(session: dict, path: str) -> dict:
    try:
        r = await vbr_get(session, path)
        return r if isinstance(r, dict) else {"data": r}
    except Exception:  # noqa: BLE001 - un job sin detalle no debe romper el analisis
        return {}


def _analysis_tasks(payload: dict) -> list:
    out = []
    for tk in _items(payload):
        pg = tk.get("progress") or {}
        processed = pg.get("processedSize") or 0
        transferred = pg.get("transferredSize") or 0
        out.append({
            "name": tk.get("name"),
            "repositoryId": tk.get("repositoryId"),
            "result": (tk.get("result") or {}).get("result"),
            "bottleneck": pg.get("bottleneck"),
            "processingRate": pg.get("processingRate"),
            "duration": pg.get("duration"),
            "processedSize": processed,
            "readSize": pg.get("readSize") or 0,
            "transferredSize": transferred,
            # Solo tiene sentido con transferencia real (evita ratios absurdos
            # en incrementales donde se transfieren unos pocos bytes).
            "reduction": (round(processed / transferred, 1)
                          if transferred and transferred >= 1048576 else None),
        })
    return out


def _analysis_bottleneck(log_records: list) -> Optional[dict]:
    """Parsea la(s) linea(s) 'Load:' del log. Si hay varias (multi-VM), toma el
    peor caso por componente."""
    loads, primary = [], None
    for r in log_records:
        txt = f"{r.get('title', '')} {r.get('description', '')}"
        m = _LOAD_RE.search(txt)
        if m:
            loads.append(tuple(int(x) for x in m.groups()))
        pm = _PRIMARY_RE.search(txt)
        if pm:
            primary = pm.group(1)
    if not loads:
        return {"primary": primary} if primary else None
    comps = {
        "source": max(l[0] for l in loads),
        "proxy": max(l[1] for l in loads),
        "network": max(l[2] for l in loads),
        "target": max(l[3] for l in loads),
    }
    if not primary:
        primary = max(comps, key=comps.get).capitalize()
    return {**comps, "primary": primary}


# --- Topologia de ejemplo para modo demo ------------------------------------
def _demo_response(path: str) -> dict:
    if path.endswith("/proxies"):
        return {"data": _DEMO_PROXIES}
    if path.endswith("/repositories"):
        return {"data": _DEMO_REPOS}
    if path.endswith("/managedServers"):
        return {"data": _DEMO_MANAGED}
    return {"data": []}


_DEMO_MANAGED = [
    {"id": "srv-vbr", "name": "VBR01 (Backup Server)", "type": "VbrServer"},
    {"id": "srv-win", "name": "WIN-PROXY01", "type": "WindowsHost"},
    {"id": "srv-lin", "name": "LIN-PROXY01", "type": "LinuxHost"},
    {"id": "srv-mount", "name": "WIN-MOUNT01", "type": "WindowsHost"},
    {"id": "srv-repo-lin", "name": "HARDENED-REPO01", "type": "LinuxHost"},
]

_DEMO_PROXIES = [
    {"id": "prx-win", "name": "VMware Proxy 01", "type": "vmware",
     "os": "windows", "hostId": "srv-win",
     "description": "Windows proxy - hot-add"},
    {"id": "prx-lin", "name": "Linux Proxy 01", "type": "vmware",
     "os": "linux", "hostId": "srv-lin",
     "description": "Linux proxy - NBD/hot-add"},
]

_DEMO_REPOS = [
    {"id": "repo-refs", "name": "Local ReFS Repo", "type": "WinLocal",
     "hostId": "srv-win", "mountServerId": "srv-mount",
     "repository": {"path": "E:\\Backups"}},
    {"id": "repo-xfs", "name": "Linux Hardened Repo", "type": "LinuxHardened",
     "hostId": "srv-repo-lin", "mountServerId": "srv-mount",
     "repository": {"path": "/mnt/backups"}},
]
