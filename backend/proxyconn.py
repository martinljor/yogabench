"""
Conexiones al proxy (Capa 3) + factory de executor.
----------------------------------------------------
Guarda, por (session_id, target_id), como llegar al proxy para correr el
benchmark: modo (mock/winrm), SO y credenciales. La password del proxy vive
SOLO aca (server-side), igual que el token de VBR — el frontend nunca la ve de
vuelta.

Decidir el modo:
  - sesion demo, o sin password  -> mock (demostrable sin tocar nada)
  - Windows + password           -> winrm (real)
  - Linux real                   -> todavia no implementado (cae a mock, con nota)
"""

from executors import MockExecutor, WinRmExecutor

# "session_id:target_id" -> conn dict
CONNECTIONS: dict[str, dict] = {}


def _key(session_id: str, target_id: str) -> str:
    return f"{session_id}:{target_id}"


def get(session_id: str, target_id: str):
    return CONNECTIONS.get(_key(session_id, target_id))


def save(session_id: str, target_id: str, conn: dict) -> None:
    CONNECTIONS[_key(session_id, target_id)] = conn


def clear_session(session_id: str) -> None:
    prefix = session_id + ":"
    for k in [k for k in CONNECTIONS if k.startswith(prefix)]:
        del CONNECTIONS[k]


def make_conn(target: dict, os_type: str, demo: bool,
              host: str = None, username: str = None, password: str = None,
              port: int = 5985, transport: str = "ntlm") -> dict:
    """Arma el conn dict y decide el modo."""
    if demo or not password:
        mode = "mock"
    elif os_type == "windows":
        mode = "winrm"
    else:
        # SshExecutor (Linux) aun no implementado -> mock, pero avisamos.
        mode = "mock"
    return {
        "mode": mode,
        "os_type": os_type,
        "target_id": target.get("id"),
        "target_label": target.get("name", "proxy"),
        "host": host,
        "port": port,
        "username": username,
        "password": password,
        "transport": transport,
        "deployed": False,
    }


def build_executor(conn: dict):
    if conn["mode"] == "winrm":
        return WinRmExecutor(host=conn["host"], username=conn["username"],
                             password=conn["password"], port=conn["port"],
                             transport=conn["transport"])
    return MockExecutor(os_type=conn["os_type"], target_id=conn["target_id"],
                        tools_present=conn.get("deployed", False))
