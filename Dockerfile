# Imagen unica: el backend (FastAPI) sirve la API y tambien el frontend.
FROM python:3.12-slim

WORKDIR /app

# Dependencias primero (mejor cache de capas)
COPY backend/requirements.txt backend/requirements.txt
RUN pip install --no-cache-dir -r backend/requirements.txt

# Codigo: backend + frontend (main.py sirve ../frontend)
COPY backend/ backend/
COPY frontend/ frontend/

EXPOSE 8000
WORKDIR /app/backend

# Un solo servicio, un solo puerto. La consola queda en http://<host>:8000
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
