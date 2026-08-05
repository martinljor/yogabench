#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# Yoga Benchmark - reconocimiento READ-ONLY del appliance/host del repositorio.
#
# NO escribe archivos, NO instala nada, NO corre ningun benchmark. Solo lee
# capacidades del OS para decidir si el benchmark ACTIVO (fio) es viable o si
# hay que quedarse con el analisis pasivo (telemetria de Veeam).
#
# Uso:  bash appliance-recon.sh            (corre local en el host)
#   o:  ssh usuario@172.16.0.x 'bash -s' < appliance-recon.sh
# ---------------------------------------------------------------------------
set +e  # nunca abortar: queremos ver TODO lo que se pueda, aunque algo falle

line() { printf '\n=== %s ===\n' "$1"; }
have() { command -v "$1" >/dev/null 2>&1 && echo "  [OK]   $1 -> $(command -v "$1")" || echo "  [----] $1 (ausente)"; }

line "Sistema operativo"
cat /etc/os-release 2>/dev/null | grep -E '^(NAME|VERSION)=' || uname -a

line "Usuario y privilegios"
echo "  whoami: $(whoami 2>/dev/null)"
id 2>/dev/null | sed 's/^/  /'
echo -n "  sudo sin password: "; sudo -n true 2>/dev/null && echo "SI" || echo "no / requiere password"

line "Tipo de shell (clave: bash real vs consola restringida)"
echo "  SHELL=$SHELL"
echo "  shell de login: $(getent passwd "$(whoami)" 2>/dev/null | cut -d: -f7)"
echo "  shells validas del sistema:"; cat /etc/shells 2>/dev/null | sed 's/^/    /'

line "Herramientas de benchmark de disco"
for t in fio dd lsblk nvme ioping hdparm sync; do have "$t"; done

line "Discos y tipo (ROTA=1 disco giratorio, 0=SSD/NVMe)"
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,ROTA,MODEL 2>/dev/null | sed 's/^/  /' || echo "  lsblk no disponible"

line "Montajes / donde vive el repositorio"
df -hT 2>/dev/null | grep -vE 'tmpfs|devtmpfs|overlay' | sed 's/^/  /'
echo "  --- montajes xfs/ext4/nfs/cifs ---"
mount 2>/dev/null | grep -iE 'type (xfs|ext4|nfs|cifs)' | sed 's/^/  /'

line "CLI / binarios de Veeam presentes"
for t in veeamconfig veeam veeamagent; do have "$t"; done
ls -d /opt/veeam* 2>/dev/null | sed 's/^/  /' || echo "  (sin /opt/veeam*)"

line "Salida a internet (para saber si se puede bajar herramientas)"
timeout 4 bash -c 'echo > /dev/tcp/github.com/443' 2>/dev/null && echo "  github.com:443 alcanzable" || echo "  sin salida (o bloqueada) a github:443"

line "FIN del reconocimiento (nada fue modificado)"
