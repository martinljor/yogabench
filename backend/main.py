"""
Prototipo - Capa de orquestacion (Capa 2) + API de benchmark (Capa 3).
----------------------------------------------------------------------
Este backend NUNCA expone la password de VBR al frontend: la recibe una vez,
la cambia por un token de la REST API de Veeam, y solo devuelve un session_id.

Modulos:
    vbr.py        -> sesiones, cliente REST de VBR, topologia dinamica, modo demo
    executors.py  -> donde corre el benchmark (hoy: MockExecutor)
    benchmark.py  -> jobs de benchmark + parseo de resultados

Correr con:
    pip install -r requirements.txt
    uvicorn main:app --reload --port 8000
"""

import os
from typing import Optional

from fastapi import FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel

import baselines
import benchmark
import proxyconn
import vbr

# Carpeta del frontend (servido por este mismo backend -> un solo puerto).
FRONTEND_DIR = os.path.join(os.path.dirname(__file__), "..", "frontend")

app = FastAPI(title="Veeam Proxy Benchmark - Prototipo")

# CORS abierto solo para el prototipo local. En produccion, restringir a la URL
# exacta del frontend.
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)


# --- Conexion ----------------------------------------------------------------
class ConnectRequest(BaseModel):
    host: str
    port: int = 9419
    username: str
    password: str
    api_version: str = "1.2-rev1"
    verify_ssl: bool = False


class ConnectResponse(BaseModel):
    session_id: str
    expires_in: int


@app.post("/api/connect", response_model=ConnectResponse)
async def connect(req: ConnectRequest):
    result = await vbr.connect(
        req.host, req.port, req.username, req.password,
        req.api_version, req.verify_ssl,
    )
    return ConnectResponse(**result)


@app.post("/api/connect-demo", response_model=ConnectResponse)
async def connect_demo():
    """DEV/TEST ONLY. Sesion con topologia de ejemplo, sin VBR. No esta enlazado
    en la consola web (el 'modo demo' se saco de la UI); queda como utilidad para
    desarrollo y pruebas automatizadas sin depender de un VBR real."""
    return ConnectResponse(**vbr.connect_demo())


@app.post("/api/{session_id}/disconnect")
async def disconnect(session_id: str):
    """Cierra la sesion: descarta el token de VBR y las conexiones a proxies."""
    vbr.SESSIONS.pop(session_id, None)
    proxyconn.clear_session(session_id)
    return {"ok": True}


# --- Objetivo 1: arquitectura -----------------------------------------------
@app.get("/api/{session_id}/proxies")
async def get_proxies(session_id: str):
    session = vbr.get_session(session_id)
    return await vbr.vbr_get(session, "v1/backupInfrastructure/proxies")


@app.get("/api/{session_id}/repositories")
async def get_repositories(session_id: str):
    session = vbr.get_session(session_id)
    return await vbr.vbr_get(session, "v1/backupInfrastructure/repositories")


@app.get("/api/{session_id}/managed-servers")
async def get_managed_servers(session_id: str):
    session = vbr.get_session(session_id)
    return await vbr.vbr_get(session, "v1/backupInfrastructure/managedServers")


@app.get("/api/{session_id}/flow")
async def get_flow(session_id: str):
    session = vbr.get_session(session_id)
    return await vbr.build_flow(session)


# --- Carril B: telemetria del propio Veeam (compatible con appliances) -------
@app.get("/api/{session_id}/sessions")
async def get_sessions(session_id: str):
    """Sesiones de jobs (backup/restore) segun las reporta el VBR."""
    session = vbr.get_session(session_id)
    return await vbr.vbr_get(
        session, "v1/sessions?limit=50&orderColumn=CreationTime&orderAsc=false")


@app.get("/api/{session_id}/analysis-range")
async def get_analysis_range(session_id: str):
    """Rango de dias con info disponible (para el selector, barato)."""
    session = vbr.get_session(session_id)
    return await vbr.analysis_range(session)


@app.get("/api/{session_id}/analysis")
async def get_analysis(session_id: str, days: Optional[int] = None):
    """Estadistica de bottleneck agregada por repositorio y proxy sobre una
    ventana de dias (lee la telemetria de Veeam; sin tocar nada)."""
    session = vbr.get_session(session_id)
    return await vbr.build_analysis(session, days=days)


@app.get("/api/{session_id}/raw/{vbr_path:path}")
async def raw_get(session_id: str, vbr_path: str, request: Request):
    """DEV/EXPLORE (read-only): passthrough GET a cualquier ruta de la REST API
    de VBR, para inspeccionar el schema real. Quitar antes de exponer."""
    session = vbr.get_session(session_id)
    qs = request.url.query
    return await vbr.vbr_get(session, vbr_path + (("?" + qs) if qs else ""))


# --- Objetivo 2: benchmark ---------------------------------------------------
@app.get("/api/baselines")
async def get_baselines():
    """Catalogos de 'lo esperado' (disco por tier, red por enlace)."""
    return {
        "disk": {"data": baselines.disk_catalog(), "default": baselines.DEFAULT_DISK},
        "net": {"data": baselines.net_catalog(), "default": baselines.DEFAULT_NET},
    }


@app.get("/api/{session_id}/benchmark-options")
async def get_benchmark_options(session_id: str):
    """Repositorios (destino, con SO del host y mount server) + proxies (para
    atar manualmente en backup). El SO define fio vs diskspd."""
    session = vbr.get_session(session_id)
    repos = vbr._items(await vbr.vbr_get(session, "v1/backupInfrastructure/repositories"))
    proxies = vbr._items(await vbr.vbr_get(session, "v1/backupInfrastructure/proxies"))
    managed = vbr._items(await vbr.vbr_get(session, "v1/backupInfrastructure/managedServers"))
    return {
        "repositories": [vbr.resolve_repo(r, managed) for r in repos],
        "proxies": [{"id": p.get("id"), "name": p.get("name", "proxy"),
                     "os": vbr.resolve_proxy_os(p, managed)} for p in proxies],
    }


async def _resolve_repo(session: dict, repository_id: str) -> dict:
    repos = vbr._items(await vbr.vbr_get(session, "v1/backupInfrastructure/repositories"))
    managed = vbr._items(await vbr.vbr_get(session, "v1/backupInfrastructure/managedServers"))
    repo = next((r for r in repos if r.get("id") == repository_id), None)
    if not repo:
        raise HTTPException(status_code=404, detail="Repositorio no encontrado en esta sesion.")
    return vbr.resolve_repo(repo, managed)


# --- Ciclo del executor: conexion -> preflight -> deploy (host del repo) ------
class BenchConnRequest(BaseModel):
    repository_id: str
    host: Optional[str] = None       # direccion del host del repositorio
    username: Optional[str] = None
    password: Optional[str] = None
    port: int = 5985                 # WinRM HTTP
    transport: str = "ntlm"


@app.post("/api/{session_id}/bench-connection")
async def bench_connection(session_id: str, req: BenchConnRequest):
    """Fase 1: valida la conexion al host del repositorio (donde corre el disco)."""
    session = vbr.get_session(session_id)
    repo = await _resolve_repo(session, req.repository_id)

    conn = proxyconn.make_conn(
        {"id": repo["id"], "name": repo["name"]}, repo["host_os"],
        demo=session.get("demo", False),
        host=req.host, username=req.username, password=req.password,
        port=req.port, transport=req.transport,
    )
    executor = proxyconn.build_executor(conn)
    result = await executor.test_connection()
    if result.get("ok"):
        proxyconn.save(session_id, req.repository_id, conn)
    return {"mode": conn["mode"], **result}


def _require_conn(session_id: str, repository_id: str) -> dict:
    conn = proxyconn.get(session_id, repository_id)
    if not conn:
        raise HTTPException(status_code=409,
                            detail="No hay conexion al host del repositorio. Valida la conexion primero.")
    return conn


@app.get("/api/{session_id}/bench-connection/{repository_id}/tools")
async def preflight_tools(session_id: str, repository_id: str):
    """Fase 2: chequea si la herramienta de benchmark esta instalada."""
    vbr.get_session(session_id)
    conn = _require_conn(session_id, repository_id)
    return await proxyconn.build_executor(conn).check_tools()


@app.post("/api/{session_id}/bench-connection/{repository_id}/deploy")
async def deploy_tools(session_id: str, repository_id: str):
    """Fase 3: despliega la herramienta si falta."""
    vbr.get_session(session_id)
    conn = _require_conn(session_id, repository_id)
    result = await proxyconn.build_executor(conn).deploy_tools()
    if result.get("ok"):
        conn["deployed"] = True
    return result


# --- Fase 4: benchmark -------------------------------------------------------
class BenchmarkRequest(BaseModel):
    repository_id: str
    operation: str = "backup"        # backup | restore
    resource: str = "disk"           # disk | net | compute | all
    proxy_id: Optional[str] = None   # nodo atado en backup (manual)
    duration: int = 8
    disk_baseline: str = baselines.DEFAULT_DISK
    net_baseline: str = baselines.DEFAULT_NET


@app.post("/api/{session_id}/benchmark")
async def start_benchmark(session_id: str, req: BenchmarkRequest):
    session = vbr.get_session(session_id)
    repo = await _resolve_repo(session, req.repository_id)

    # Etiqueta del nodo asociado: proxy (backup) desde la seleccion manual.
    proxy_label = None
    if req.proxy_id:
        proxies = vbr._items(await vbr.vbr_get(session, "v1/backupInfrastructure/proxies"))
        p = next((x for x in proxies if x.get("id") == req.proxy_id), None)
        proxy_label = p.get("name") if p else None

    # Conexion validada al host del repo si existe; si no, mock (demo sin ciclo).
    conn = proxyconn.get(session_id, req.repository_id)
    if not conn:
        conn = proxyconn.make_conn({"id": repo["id"], "name": repo["name"]},
                                   repo["host_os"], demo=True)
    executor = proxyconn.build_executor(conn)

    job = benchmark.create_job(
        session_id, repository=repo, operation=req.operation, resource=req.resource,
        proxy_label=proxy_label, disk_baseline=req.disk_baseline,
        net_baseline=req.net_baseline, duration=req.duration, executor=executor)
    return {"job_id": job["id"], "status": job["status"]}


@app.get("/api/{session_id}/benchmark/{job_id}")
async def get_benchmark(session_id: str, job_id: str):
    vbr.get_session(session_id)
    job = benchmark.get_job(job_id)
    if not job or job["session_id"] != session_id:
        raise HTTPException(status_code=404, detail="Job no encontrado.")
    return job


@app.get("/api/{session_id}/benchmarks")
async def list_benchmarks(session_id: str):
    vbr.get_session(session_id)
    return {"data": benchmark.list_jobs(session_id)}


@app.get("/health")
async def health():
    return {"ok": True, "active_sessions": len(vbr.SESSIONS)}


# --- Frontend estatico -------------------------------------------------------
# Se monta AL FINAL para que las rutas /api y /health tengan prioridad. Sirve
# la consola web en el mismo puerto que la API (un solo servicio para desplegar).
if os.path.isdir(FRONTEND_DIR):
    app.mount("/", StaticFiles(directory=FRONTEND_DIR, html=True), name="frontend")
