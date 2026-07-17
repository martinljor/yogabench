# Despliegue en una VM Linux (appliance de backup)

La idea: una VM **en la misma red que el VBR**, que pueda alcanzar la REST API del
VBR (9419). El backend sirve la API y la consola web en **un solo puerto (8000)**.
Para el modo Analisis (Carril B) alcanza con llegar al VBR; para el benchmark
activo (Carril A) ademas hay que llegar a los proxies/repos (WinRM 5985 / SSH 22).

> Validá primero la conectividad: desde la VM, `curl -k https://<vbr>:9419/api/`
> debe responder.

---

## Rocky Linux / RHEL 8-9 (dnf + firewalld)

```bash
# 1. Docker CE + compose
sudo dnf -y install dnf-plugins-core
sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
sudo dnf -y install docker-ce docker-ce-cli containerd.io docker-compose-plugin git
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"   # cerra sesion y volve a entrar

# 2. Traer el codigo y levantar
git clone <URL-del-repo> yoga-benchmark && cd yoga-benchmark
docker compose up -d --build

# 3. Abrir el puerto en el firewall (idealmente solo a IPs de administracion)
sudo firewall-cmd --add-port=8000/tcp --permanent && sudo firewall-cmd --reload

# 4. Verificar
curl -s http://localhost:8000/health          # -> {"ok":true,...}
```

Abrí en el navegador: `http://<ip-de-la-vm>:8000`
Actualizar: `git pull && docker compose up -d --build` · Bajar: `docker compose down`

> Alternativa sin Docker en Rocky (nativo con systemd):
> `sudo dnf -y install python3 python3-pip git` y despues seguí la "Opcion B"
> de mas abajo (el venv + unit de systemd son identicos; Rocky 9 trae Python 3.9,
> compatible con el codigo).

---

## Ubuntu / Debian (apt)

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
