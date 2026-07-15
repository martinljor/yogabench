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

import uuid
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
