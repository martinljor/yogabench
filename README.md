# Prototipo - Consola de benchmark de proxies Veeam

> [!CAUTION]
> # 🚧 FASE ALPHA — PROTOTIPO EXPERIMENTAL 🚧
> **Este proyecto está en fase _alpha_. NO es apto para producción.**
>
> - Es un prototipo en desarrollo activo: interfaces, endpoints y comportamiento
>   **pueden cambiar o romperse** sin aviso.
> - **Genera carga real** en proxies/repositorios al correr benchmarks. Usalo
>   **solo en entornos de laboratorio/prueba y con autorización.**
> - **Sin autenticación** en la consola y **credenciales en memoria** — no lo
>   expongas en una red no confiable.
> - No es un producto de Veeam ni tiene soporte oficial. **Uso bajo tu propia
>   responsabilidad.**

Prototipo con dos objetivos:

1. **Arquitectura dinamica** (`/flow`): descubre y relaciona los roles que
   importan para el benchmark de backup/restore — proxy, repositorio, mount
   server, gateway — leyendo la REST API de VBR, sin asumir nombres fijos.
2. **Prueba de rendimiento** (Capa 3). Modelo:
   - **El destino es el repositorio**, atado a otro nodo segun la accion:
     **Backup** (escritura) → `proxy → repositorio` (el proxy se elige a mano);
     **Restore** (lectura) → `repositorio → mount server` (el mount sale del repo).
   - **Recurso a medir**: **Disco** (fio/diskspd), **Red** (iperf3), **Computo**
     (CPU) o **Todo** — este ultimo es lo ideal para repos iSCSI / appliance,
     donde el acceso a disco va por la red.
   - **4 fases**: 1) validar conexion al host del repositorio, 2) verificar la
     herramienta, 3) desplegarla si falta, 4) correr.
   - **Executors** (misma interfaz, `executors.py`): `MockExecutor` (simulado) y
     `WinRmExecutor` (**real**, Windows/diskspd). Disco real anda por WinRM;
     **Red y Computo son mock** en esta version. El SO del host (fio vs diskspd)
     se resuelve solo desde su `type`.
   - Cada resultado se compara contra un **"esperado"** (disco por tier de
     almacenamiento, red por enlace GbE, computo por referencia) con veredicto
     OK / Atencion / Bajo lo esperado. Baselines en `baselines.py`, APROXIMADOS.

> **Nota:** el "modo demo" se saco de la consola web. El endpoint
> `/api/connect-demo` sigue existiendo pero **solo como utilidad de desarrollo
> y testing** (no esta enlazado en la UI); levanta una topologia de ejemplo
> para probar sin un VBR real.

## Estructura

```
Yoga_Benchmark/
├── backend/          # Capa 2+3 - API (FastAPI)
│   ├── main.py       # ruteo
│   ├── vbr.py        # sesiones, cliente REST de VBR, topologia, demo (dev)
│   ├── executors.py  # Mock + WinRm (conexion/preflight/deploy/run)
│   ├── proxyconn.py  # conexiones al proxy + factory de executor
│   ├── benchmark.py  # jobs + parseo (fio JSON / diskspd texto)
│   ├── baselines.py  # "lo esperado" por tipo de disco + veredicto
│   └── requirements.txt
└── frontend/         # Capa 1 - consola web (HTML/JS)
    └── index.html    # tabs: Infraestructura / Arquitectura / Benchmark
```

## Requisitos

- Python 3.9+ (probado en 3.9.6; el codigo usa solo type hints compatibles)
- VBR v12+ con el servicio REST API activo (puerto 9419 por defecto,
  Settings > REST API en la consola de VBR o verificar que el servicio
  "Veeam RestAPI Service" este corriendo)
- Un usuario de VBR con rol minimo "Backup Viewer" (alcanza para leer
  proxies/repos/managedServers)

## Como correr

```bash
cd backend
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --reload --port 8000
```

Para verificar que levanto bien, en otra terminal:
`curl http://localhost:8000/health` debe devolver `{"ok":true,...}`.

Despues, abri `frontend/index.html` directamente en el navegador (doble
click, o `python3 -m http.server 8080` desde la carpeta `frontend` y entrar a
`http://localhost:8080`).

**Para probar sin VBR** (dev/test): la sesion demo ya no esta en la UI, pero
podes crearla por API y usar ese `session_id` en las llamadas siguientes:
`curl -X POST http://localhost:8000/api/connect-demo`

## Notas para tu prueba en red local

- **Certificado self-signed**: por eso `verify_ssl=False` esta hardcodeado en
  el frontend. Es aceptable para el prototipo en tu lab, NO para produccion.
- **api_version**: si al conectar te devuelve un error tipo
  `"Unsupported RESTAPI version. The following versions are supported: ..."`,
  la respuesta te dice exactamente cuales acepta tu build — cambia el select
  del frontend a esa version.
- **CORS**: el backend permite cualquier origen (`allow_origins=["*"]`) solo
  para que el HTML suelto pueda pegarle a `localhost:8000` sin lios. Ajustar
  antes de exponerlo a nadie mas.
- **Armado de la arquitectura (`/flow`)**: clasifica cada nodo por rol
  (proxy / repository / mount-server / gateway / backup-server) y descubre el
  mount server leyendo el objeto anidado `mountServer.mountServerId` del repo
  (validado contra el schema real de VBR v12; ver `_extract_id` en `vbr.py`).
  En una instalacion all-in-one el mismo host es proxy-host + repo-host + mount
  server, y se muestra como mount-server. La conexion proxy->repositorio se
  dibuja para todos los proxies. Para inspeccionar el JSON crudo:
  `curl http://localhost:8000/api/<session_id>/repositories | jq`

## Prueba real de disco contra Windows (WinRM + diskspd)

El disco real de Windows esta implementado (`WinRmExecutor`) pero **no se pudo
probar desde el entorno de desarrollo** — hay que validarlo contra un host real.
La conexion es al **host del repositorio** (donde esta el disco), no al proxy.

En el host Windows:
- **WinRM habilitado**: `winrm quickconfig` (y permitir el usuario/host segun tu
  red). El puerto por defecto es 5985 (HTTP).
- Un usuario con permiso de ejecutar PowerShell remoto.
- **diskspd**: si no esta en PATH, la fase 3 lo baja de la URL oficial de
  Microsoft (`WinRmExecutor.DISKSPD_URL`) y lo deja en
  `C:\Windows\Temp\diskspd\`. Requiere salida a internet del host; si no, dejar
  `diskspd.exe` a mano en esa carpeta.

En la consola (pestaña Benchmark): elegi el repositorio, recurso **Disco** (o
Todo), completa **Host/IP, Usuario y Password** del host del repositorio, y segui
los pasos 1→4. Con esos campos llenos usa WinRM real; vacios, mock. Credenciales:
la REST API de VBR NO expone los secretos de sus credenciales, por eso se piden
aparte aca.

Nota: el parser de diskspd (`benchmark.parse_diskspd`) lee la salida de texto; si
tu version de diskspd cambia el formato, es el punto a ajustar (la salida cruda
queda en el resultado del job para inspeccionar).

## Otros pendientes

- **SshExecutor (Linux/fio)**: mismo patron que WinRm, todavia sin implementar
  (hoy un host Linux con credenciales cae a mock).
- **Red y Computo reales**: hoy son mock. Red real necesita iperf3 en los dos
  extremos (uno como server); Computo, un tool de CPU en el nodo que procesa.
- **Proxy atado al repo**: hoy se elige a mano. Lo mas fiel seria deducirlo de
  los backup jobs (`/jobs`), a inspeccionar contra el schema real.

- Persistencia de sesiones y jobs (hoy viven en dicts en memoria del proceso;
  si reiniciás el backend, hay que reconectar)
- Multiples VBR simultaneos desde la misma consola
- Metricas de red (iperf3) y CPU — esta version mide solo disco
