# Despliegue en un servidor Ubuntu (appliance de backup)

La idea: una VM Ubuntu **en la misma red que el VBR**, que pueda alcanzar la REST
API del VBR (9419) **y** los proxies/repos por su canal de admin (WinRM 5985/5986
en Windows, SSH 22 en Linux). El backend sirve la API y la consola web en **un
solo puerto (8000)**.

> Validá primero la conectividad: desde la VM, `curl -k https://<vbr>:9419/api/`
> debe responder, y tenés que poder llegar a los proxies/repos por WinRM/SSH.

---

## Opcion A — Docker (recomendada, "momentanea")

Requisitos: Docker + plugin compose.

```bash
# 1. Instalar Docker (Ubuntu)
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2
sudo usermod -aG docker $USER && newgrp docker

# 2. Traer el codigo
git clone <URL-del-repo> yoga-benchmark
cd yoga-benchmark

# 3. Levantar
docker compose up -d --build

# 4. Verificar
curl -s http://localhost:8000/health     # -> {"ok":true,...}
```

Abrí en el navegador: `http://<ip-de-la-vm>:8000`

Actualizar a una version nueva:
```bash
git pull && docker compose up -d --build
```

Bajar todo sin dejar rastro:
```bash
docker compose down
```

---

## Opcion B — Nativa con systemd (sin Docker)

```bash
# 1. Dependencias
sudo apt-get update && sudo apt-get install -y python3-venv git
git clone <URL-del-repo> /opt/yoga-benchmark
cd /opt/yoga-benchmark/backend
python3 -m venv .venv
./.venv/bin/pip install -r requirements.txt

# 2. Servicio systemd
sudo tee /etc/systemd/system/yoga-benchmark.service >/dev/null <<'EOF'
[Unit]
Description=Yoga Benchmark (Veeam proxy benchmark)
After=network-online.target

[Service]
WorkingDirectory=/opt/yoga-benchmark/backend
ExecStart=/opt/yoga-benchmark/backend/.venv/bin/uvicorn main:app --host 0.0.0.0 --port 8000
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now yoga-benchmark
```

Abrí: `http://<ip-de-la-vm>:8000` · Logs: `journalctl -u yoga-benchmark -f`

---

## Notas de seguridad (antes de exponerlo)

- **Puerto/red**: 8000 queda abierto a la red de la VM. Restringilo con firewall
  a las IPs de administracion (ej: `ufw allow from <ip-admin> to any port 8000`).
- **Sin auth todavia**: la consola no pide login. No la expongas fuera de la red
  de gestion. (Auth = pendiente).
- **Credenciales**: hoy viven en memoria del proceso. Para produccion, mover a un
  vault. No persistir en claro.
- **CORS**: `allow_origins=["*"]` es innecesario ahora que todo va por el mismo
  origen; conviene cerrarlo antes de exponer.
