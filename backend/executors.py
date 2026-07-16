"""
Capa 3 - Executors (donde corre el benchmark).
----------------------------------------------
El orquestador (benchmark.py) no sabe COMO se llega al proxy. Pide, en fases:
    test_connection()      -> valida el canal (SSH/WinRM) contra el host del proxy
    check_tools()          -> preflight: esta instalada la herramienta?
    deploy_tools()         -> si falta, la instala/copia
    run_disk_benchmark()   -> corre el test y devuelve salida cruda

Executors disponibles:
    MockExecutor   -> simula todo, sin tocar ningun proxy (demo / desarrollo)
    WinRmExecutor  -> REAL, Windows: WinRM + diskspd

El SshExecutor (Linux/fio) implementaria la misma interfaz.

El parseo de la salida cruda vive en benchmark.py (parse_fio_json / parse_diskspd),
elegido por el campo "format" que devuelve run_disk_benchmark.
"""

import abc
import asyncio
import hashlib
import random
from typing import Awaitable, Callable, Optional

# callback(progreso_0_a_100) -> awaitable, para reportar avance del job.
ProgressCb = Optional[Callable[[int], Awaitable[None]]]

DEFAULT_TESTS = ["seqread", "seqwrite", "randread", "randwrite"]


def test_params(name: str):
    """(block_size, es_random, porcentaje_write) por sub-test."""
    return {
        "seqread":   ("1M", False, 0),
        "seqwrite":  ("1M", False, 100),
        "randread":  ("4K", True, 0),
        "randwrite": ("4K", True, 100),
    }[name]


class Executor(abc.ABC):
    tool = "fio"

    @abc.abstractmethod
    async def test_connection(self) -> dict:
        """{'ok': bool, 'message': str, 'hostname': str|None}"""
        raise NotImplementedError

    @abc.abstractmethod
    async def check_tools(self) -> dict:
        """{'installed': bool, 'detail': str}"""
        raise NotImplementedError

    @abc.abstractmethod
    async def deploy_tools(self) -> dict:
        """{'ok': bool, 'message': str}"""
        raise NotImplementedError

    @abc.abstractmethod
    async def run_disk_benchmark(self, spec: dict, on_progress: ProgressCb = None) -> dict:
        raise NotImplementedError


# ---------------------------------------------------------------------------
class MockExecutor(Executor):
    """Simula fio/diskspd con numeros plausibles. No toca ningun proxy.

    Los numeros se derivan del target_id (semilla estable) para que cada proxy
    tenga un 'perfil' de disco repetible.
    """

    def __init__(self, os_type: str = "linux", target_id: str = "mock", tools_present: bool = False):
        self.os_type = (os_type or "linux").lower()
        self.target_id = target_id
        self.tools_present = tools_present
        self.tool = "diskspd" if self.os_type == "windows" else "fio"

    async def test_connection(self) -> dict:
        await asyncio.sleep(0.3)
        return {"ok": True, "message": "Conexion simulada OK (mock).",
                "hostname": f"mock-{self.target_id}"}

    async def check_tools(self) -> dict:
        await asyncio.sleep(0.3)
        if self.tools_present:
            return {"installed": True, "detail": f"{self.tool} presente (mock)."}
        return {"installed": False, "detail": f"{self.tool} no encontrado (mock)."}

    async def deploy_tools(self) -> dict:
        await asyncio.sleep(0.6)
        return {"ok": True, "message": f"{self.tool} desplegado (mock)."}

    async def run_disk_benchmark(self, spec: dict, on_progress: ProgressCb = None) -> dict:
        duration = int(spec.get("duration", 8))
        tests = spec.get("tests") or DEFAULT_TESTS
        factor, rnd = self._profile(self.target_id)

        total = min(duration, 10)
        steps = max(len(tests) * 4, 1)
        for i in range(steps):
            await asyncio.sleep(total / steps)
            if on_progress:
                await on_progress(int((i + 1) / steps * 100))

        jobs = [self._fio_job(name, factor, rnd) for name in tests]
        return {"format": "fio-json", "fio version": "fio-3.28 (mock)", "jobs": jobs}

    @staticmethod
    def _profile(target_id: str):
        h = int(hashlib.md5(target_id.encode()).hexdigest(), 16)
        rnd = random.Random(h)
        return rnd.uniform(0.6, 1.4), rnd

    def _fio_job(self, name: str, factor: float, rnd: random.Random) -> dict:
        jitter = rnd.uniform(0.9, 1.1)
        empty = {"bw": 0, "iops": 0.0, "lat_ns": {"mean": 0}}
        if name == "seqread":
            bw = 900 * factor * jitter
            return {"jobname": name, "read": self._sec(bw, bw, 4.0 / factor), "write": dict(empty)}
        if name == "seqwrite":
            bw = 720 * factor * jitter
            return {"jobname": name, "read": dict(empty), "write": self._sec(bw, bw, 5.5 / factor)}
        if name == "randread":
            iops = 90000 * factor * jitter
            return {"jobname": name, "read": self._sec(iops * 4 / 1024, iops, 0.25 / factor), "write": dict(empty)}
        iops = 62000 * factor * jitter
        return {"jobname": name, "read": dict(empty), "write": self._sec(iops * 4 / 1024, iops, 0.45 / factor)}

    @staticmethod
    def _sec(bw_mbps: float, iops_val: float, lat_ms: float) -> dict:
        return {"bw": round(bw_mbps * 1024), "iops": round(iops_val, 1),
                "lat_ns": {"mean": round(lat_ms * 1_000_000)}}


# ---------------------------------------------------------------------------
class WinRmExecutor(Executor):
    """REAL - Windows via WinRM + diskspd.

    NO probado desde este entorno (necesita un proxy Windows con WinRM habilitado
    y credenciales). Los errores se propagan con su mensaje para diagnosticar.

    Requiere el paquete `pywinrm` (ver requirements.txt) y en el proxy:
      - WinRM habilitado (winrm quickconfig)
      - permiso del usuario para ejecutar PowerShell remoto
    diskspd: si no esta en PATH, deploy_tools lo baja de la URL oficial y lo deja
    en deploy_dir. Requiere que el proxy tenga salida a internet, o pre-ubicar
    diskspd.exe en deploy_dir a mano.
    """

    tool = "diskspd"

    # URL oficial de diskspd (Microsoft). Ajustable.
    DISKSPD_URL = "https://github.com/microsoft/diskspd/releases/latest/download/DiskSpd.ZIP"

    def __init__(self, host: str, username: str, password: str, port: int = 5985,
                 transport: str = "ntlm", deploy_dir: str = r"C:\Windows\Temp\diskspd"):
        self.host = host
        self.username = username
        self.password = password
        self.port = port
        self.transport = transport
        self.deploy_dir = deploy_dir
        self.exe = deploy_dir + r"\diskspd.exe"
        self.testfile = deploy_dir + r"\benchfile.dat"

    def _session(self):
        try:
            import winrm  # import perezoso: no romper el backend si falta el paquete
        except ImportError as e:
            raise RuntimeError(
                "Falta el paquete 'pywinrm'. Instalalo: pip install pywinrm") from e
        endpoint = f"http://{self.host}:{self.port}/wsman"
        return winrm.Session(endpoint, auth=(self.username, self.password),
                             transport=self.transport)

    async def _run_ps(self, script: str):
        """Corre PowerShell en el proxy (en thread, pywinrm es sincrono)."""
        def _blocking():
            r = self._session().run_ps(script)
            return r.status_code, r.std_out.decode(errors="replace"), r.std_err.decode(errors="replace")
        return await asyncio.to_thread(_blocking)

    async def test_connection(self) -> dict:
        try:
            code, out, err = await self._run_ps("hostname")
        except Exception as e:  # noqa: BLE001
            return {"ok": False, "message": f"No se pudo conectar por WinRM: {e}", "hostname": None}
        if code != 0:
            return {"ok": False, "message": err.strip() or "Error al ejecutar comando.", "hostname": None}
        return {"ok": True, "message": "Conexion WinRM OK.", "hostname": out.strip()}

    async def check_tools(self) -> dict:
        script = (
            f"if (Get-Command diskspd.exe -ErrorAction SilentlyContinue) {{ 'PATH' }}"
            f" elseif (Test-Path '{self.exe}') {{ 'DEPLOYED' }} else {{ 'MISSING' }}"
        )
        code, out, err = await self._run_ps(script)
        state = out.strip()
        if state == "PATH":
            return {"installed": True, "detail": "diskspd encontrado en PATH."}
        if state == "DEPLOYED":
            return {"installed": True, "detail": f"diskspd encontrado en {self.exe}."}
        return {"installed": False, "detail": "diskspd no esta instalado en el proxy."}

    async def deploy_tools(self) -> dict:
        script = f"""
$ErrorActionPreference='Stop'
New-Item -ItemType Directory -Force -Path '{self.deploy_dir}' | Out-Null
$zip = '{self.deploy_dir}\\diskspd.zip'
[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12
Invoke-WebRequest -Uri '{self.DISKSPD_URL}' -OutFile $zip
Expand-Archive -Path $zip -DestinationPath '{self.deploy_dir}' -Force
$exe = Get-ChildItem -Path '{self.deploy_dir}' -Recurse -Filter diskspd.exe | Where-Object {{ $_.FullName -match 'amd64' }} | Select-Object -First 1
if (-not $exe) {{ $exe = Get-ChildItem -Path '{self.deploy_dir}' -Recurse -Filter diskspd.exe | Select-Object -First 1 }}
Copy-Item $exe.FullName '{self.exe}' -Force
'OK'
"""
        try:
            code, out, err = await self._run_ps(script)
        except Exception as e:  # noqa: BLE001
            return {"ok": False, "message": f"Fallo el despliegue: {e}"}
        if code != 0 or "OK" not in out:
            return {"ok": False, "message": err.strip() or "Fallo el despliegue de diskspd."}
        return {"ok": True, "message": f"diskspd desplegado en {self.exe}."}

    async def run_disk_benchmark(self, spec: dict, on_progress: ProgressCb = None) -> dict:
        duration = int(spec.get("duration", 8))
        tests = spec.get("tests") or DEFAULT_TESTS
        results = []
        for i, name in enumerate(tests):
            bs, is_rand, wpct = test_params(name)
            rand = "-r" if is_rand else ""
            # -L mide latencia (agrega columnas AvgLat/LatStdDev que parseamos).
            # OJO: -D (IOPS std dev) NO mide latencia y corre las columnas.
            # El archivo de prueba va como argumento POSICIONAL, al final.
            cmd = (f'& "{self.exe}" -c1G -d{duration} -b{bs} {rand} -w{wpct} '
                   f'-o8 -t2 -W2 -Sh -L "{self.testfile}"')
            code, out, err = await self._run_ps(cmd)
            if code != 0:
                raise RuntimeError(f"diskspd fallo en {name}: {err.strip() or out.strip()}")
            mode = "write" if wpct == 100 else "read"
            results.append({"name": name, "mode": mode, "output": out})
            if on_progress:
                await on_progress(int((i + 1) / len(tests) * 100))
        # limpiar el archivo de prueba
        await self._run_ps(f"Remove-Item -Force '{self.testfile}' -ErrorAction SilentlyContinue")
        return {"format": "diskspd-text", "tool": "diskspd", "tests": results}
