"""
Capa 3 - Orquestacion de jobs de benchmark + parseo de resultados.
------------------------------------------------------------------
Maneja el ciclo de vida de un job (queued -> running -> completed/failed),
lo corre en segundo plano con asyncio, y normaliza la salida cruda del
executor (fio-JSON) a metricas simples para la UI.

El parser parse_fio_json es COMPARTIDO entre el mock y un futuro executor
real por SSH: es el punto donde "salida de fio" se vuelve "metricas".
"""

import asyncio
import hashlib
import random
import time
import uuid
from typing import Optional

import baselines
from executors import Executor

# job_id -> job dict. En memoria, como el resto del prototipo.
JOBS: dict[str, dict] = {}

DEFAULT_TESTS = ["seqread", "seqwrite", "randread", "randwrite"]

# Operacion emulada -> que tests de disco corre.
# Modelo (perspectiva del repositorio, que es donde suele estar el bottleneck):
#   backup  = escritura al repositorio  -> tests de write
#   restore = lectura desde el repositorio -> tests de read
#   both    = las dos acciones
OPERATION_TESTS = {
    "backup": ["seqwrite", "randwrite"],
    "restore": ["seqread", "randread"],
    "both": ["seqread", "seqwrite", "randread", "randwrite"],
}


def tests_for_operation(operation: str) -> list:
    return OPERATION_TESTS.get(operation, OPERATION_TESTS["both"])


def list_jobs(session_id: str) -> list:
    return [j for j in JOBS.values() if j["session_id"] == session_id]


def get_job(job_id: str) -> Optional[dict]:
    return JOBS.get(job_id)


def resources_for(resource: str) -> list:
    """'all' -> los tres recursos; si no, el elegido."""
    if resource == "all":
        return ["disk", "net", "compute"]
    return [resource]


def create_job(session_id: str, *, repository: dict, operation: str, resource: str,
               proxy_label: str, disk_baseline: str, net_baseline: str,
               duration: int, executor: Executor) -> dict:
    """Crea el job y lo lanza en segundo plano. Devuelve el job (estado queued).

    repository: {id, name, host_os, mount:{id,name,os}|None}
    """
    mount = repository.get("mount") or {}
    job = {
        "id": str(uuid.uuid4()),
        "session_id": session_id,
        "repository_id": repository.get("id"),
        "repository_label": repository.get("name", "repository"),
        "operation": operation,        # backup | restore
        "resource": resource,          # disk | net | compute | all
        "proxy_label": proxy_label,    # nodo asociado en backup
        "mount_label": mount.get("name"),  # nodo asociado en restore
        "os_type": repository.get("host_os", "linux"),
        "tool": executor.tool,
        "disk_baseline": disk_baseline,
        "net_baseline": net_baseline,
        "status": "queued",
        "progress": 0,
        "created_at": time.time(),
        "started_at": None,
        "finished_at": None,
        "results": {"disk": None, "net": None, "compute": None},
        "error": None,
    }
    JOBS[job["id"]] = job
    asyncio.create_task(_run(job, executor, duration))
    return job


async def _run(job: dict, executor: Executor, duration: int):
    job["status"] = "running"
    job["started_at"] = time.time()
    resources = resources_for(job["resource"])
    seed = job["repository_id"] or "seed"

    try:
        done = 0
        for res in resources:
            if res == "disk":
                async def on_progress(pct, _base=done, _n=len(resources)):
                    job["progress"] = int((_base + pct / 100) / _n * 100)
                tests = tests_for_operation(job["operation"])
                raw = await executor.run_disk_benchmark(
                    {"target_id": seed, "tests": tests, "duration": duration},
                    on_progress=on_progress)
                # Guardamos la salida cruda para poder inspeccionar/depurar el
                # formato real (ej: diskspd por version). Se ve en el GET del job.
                job["disk_raw"] = raw
                job["results"]["disk"] = baselines.annotate_disk(_parse(raw), job["disk_baseline"])
            elif res == "net":
                await asyncio.sleep(min(duration, 3))
                job["results"]["net"] = baselines.annotate_net(_mock_net(seed), job["net_baseline"])
            elif res == "compute":
                await asyncio.sleep(min(duration, 3))
                job["results"]["compute"] = baselines.annotate_compute(_mock_compute(seed))
            done += 1
            job["progress"] = int(done / len(resources) * 100)
        job["progress"] = 100
        job["status"] = "completed"
    except Exception as e:  # noqa: BLE001 - prototipo: cualquier fallo marca el job
        job["status"] = "failed"
        job["error"] = str(e)
    finally:
        job["finished_at"] = time.time()


# --- Mock de red y computo (real todavia no; ver README) --------------------
def _seeded(seed: str):
    return random.Random(int(hashlib.md5(seed.encode()).hexdigest(), 16))


def _mock_net(seed: str) -> float:
    """MB/s de red simulados. Base ~10GbE * factor del target."""
    rnd = _seeded("net" + seed)
    return 1150 * rnd.uniform(0.55, 1.15) * rnd.uniform(0.95, 1.05)


def _mock_compute(seed: str) -> float:
    """MB/s de compresion simulados."""
    rnd = _seeded("cpu" + seed)
    return 560 * rnd.uniform(0.6, 1.3) * rnd.uniform(0.95, 1.05)


def _parse(raw: dict) -> list:
    """Elige el parser segun el formato que devolvio el executor."""
    if raw.get("format") == "diskspd-text":
        return parse_diskspd(raw)
    return parse_fio_json(raw)


def parse_diskspd(raw: dict) -> list:
    """Salida de texto de diskspd -> metricas normalizadas (misma forma que fio).

    diskspd imprime una seccion 'Read IO' y otra 'Write IO', cada una con una
    linea 'total:' con columnas: bytes | I/Os | MiB/s | I/O per s | AvgLat(ms).
    Tomamos la seccion segun el modo del test.
    """
    out = []
    for t in raw.get("tests", []):
        out.append({"name": t["name"], "mode": t["mode"],
                    **_diskspd_section(t.get("output", ""), t["mode"])})
    return out


def _diskspd_section(output: str, mode: str) -> dict:
    header = "Write IO" if mode == "write" else "Read IO"
    lines = output.splitlines()
    idx = next((i for i, l in enumerate(lines) if header in l), None)
    total_line = None
    if idx is not None:
        for l in lines[idx + 1:]:
            if l.strip().lower().startswith("total:"):
                total_line = l
                break
    empty = {"bw_mbps": 0, "iops": 0, "lat_ms": 0}
    if not total_line:
        return empty
    parts = [p.strip() for p in total_line.split("|")]
    try:
        mibps, iops, lat = float(parts[2]), float(parts[3]), float(parts[4])
    except (IndexError, ValueError):
        return empty
    return {"bw_mbps": round(mibps * 1.048576, 1), "iops": round(iops), "lat_ms": round(lat, 3)}


def parse_fio_json(raw: dict) -> list:
    """fio-JSON -> lista de metricas normalizadas por sub-test.

    fio reporta bw en KiB/s y latencia en ns; una corrida tiene read y write,
    solo uno con datos segun el modo. Devolvemos el lado que tiene actividad.
    """
    out = []
    for job in raw.get("jobs", []):
        read = job.get("read", {})
        write = job.get("write", {})
        is_write = (write.get("iops") or 0) > 0 and (read.get("iops") or 0) == 0
        sec = write if is_write else read
        out.append({
            "name": job.get("jobname", "test"),
            "mode": "write" if is_write else "read",
            "bw_mbps": round((sec.get("bw", 0) or 0) / 1024, 1),   # KiB/s -> MB/s
            "iops": round(sec.get("iops", 0) or 0),
            "lat_ms": round((sec.get("lat_ns", {}).get("mean", 0) or 0) / 1_000_000, 3),
        })
    return out
