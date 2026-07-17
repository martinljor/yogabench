#!/usr/bin/env bash
# Levanta la consola web a demanda (NO es un servicio de sistema).
# Primera vez: crea el venv e instala dependencias. Ctrl+C para detener.
#
#   ./run.sh            -> escucha en 0.0.0.0:8000
#   PORT=9000 ./run.sh  -> otro puerto
set -e

cd "$(dirname "$0")/backend"

PY="${PYTHON:-python3}"
PORT="${PORT:-8000}"

if [ ! -d .venv ]; then
  echo ">> Primera ejecucion: creando entorno virtual e instalando dependencias..."
  "$PY" -m venv .venv
  ./.venv/bin/pip install --quiet --upgrade pip
  ./.venv/bin/pip install --quiet -r requirements.txt
fi

echo ">> Consola web en http://localhost:${PORT}  (Ctrl+C para detener)"
exec ./.venv/bin/uvicorn main:app --host 0.0.0.0 --port "${PORT}"
