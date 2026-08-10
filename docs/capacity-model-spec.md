# Yoga Benchmark — v0.4 · Capacity & Headroom Model (spec)

## Objetivo
No "qué pasó" (eso ya lo hace Veeam ONE) sino **cuánto puede entregar la infra, dónde está el techo, y qué cambio la mejora — expresado en TIEMPO**.

Salida final por job (y agregada por repo/proxy/entorno):
> "Hoy tarda **T** y entrega **R**; el cuello es **X**; si hacés **Y**, pasa a **~T′ (~Z% más rápido)**."

---

## Fuentes de datos (REST v1.3, sin SSH, seguro en producción)
| Endpoint | Qué aporta |
|---|---|
| `/sessions` | lista de sesiones; filtrar data jobs (backup/replica/copy/restore), excluir config/discover/retention/malware/agent/**delete** |
| `/sessions/{id}/logs` | **UNA** línea `Load: Source% > Proxy% > Network% > Target%` + `Primary bottleneck` **por sesión (nivel job)** |
| `/sessions/{id}/taskSessions` | **por VM**: `bottleneck` (palabra), `processedSize`, `readSize`, `transferredSize`, `duration`, `algorithm` (Full/Increment) |
| `backupInfrastructure/proxies`,`/repositories`,`/managedServers` | `maxTaskCount` (paralelismo), host, path |
| (lab, opcional) fio / iperf | **calibración medida**: escritura/lectura del repo (MB/s), enlace (Mbps) |

### Hechos confirmados con datos reales (no asumir otra cosa)
1. **`Load:` es a nivel job, no por VM** (job de 2 VMs → una sola línea `Load:`). Por VM solo hay la *palabra* bottleneck + métricas.
2. **`processingRate` no es confiable** (string: `"N/A"`, `"2.5 GB"`). ⇒ el rate lo calculamos: `transferredSize/duration` y `readSize/duration`.
3. **Solo sirven corridas que MOVIERON datos.** Un incremental "no-op" trae `readSize=0`, `transferredSize≈32 B`, `rate=N/A`, y `Load: Source 97%` trivial. ⇒ elegir por job la corrida reciente con **mayor `transferredSize`** (idealmente un Full/Active Full); si no hay ninguna con datos → "insufficient data".
4. **`readSize` puede venir 0** aun en algunos casos → priorizar `transferredSize` para el lado Target; usar `readSize` para el lado Source solo si >0.
5. **FLR (FileLevelRestore) NO sirve** para capacidad (sin `Load:` ni throughput) → excluir del filtro de data jobs (`filelevel`/`flr`).
6. **AgentBackup → `taskSessions` da HTTP 500** (bug `ESessionType AgentManagement`): usable a **nivel job** (la línea `Load:` sí viene) pero **sin métricas por VM**. Capturar igual el `Load:`; marcar per-VM como no disponible.
7. **El `Load:` dice DÓNDE está el cuello SIEMPRE; el MB/s absoluto solo es real si la corrida SATURÓ el stage.** Un incremental chico (ej: 0.9 GiB, Network 81%) NO revela el techo del hardware. ⇒ número absoluto = corrida grande (Active Full) **o** benchmark activo (fio/iperf); el `Load:` da el diagnóstico relativo. Datos reales de calibración: DC01 Source 94% (~1 GB/s lectura), Malware Network 81% (~3.2x reducción), agents Source-bound.

---

## Capa de recursos (proxy/repo) — BASE del análisis
Sin conocer los recursos no hay veredicto fundado (ni saber si es viable exigir más). Qué sabemos por fuente:

| Recurso | REST (prod, sin SSH) | SSH (lab) | Manual (prod) |
|---|---|---|---|
| Paralelismo provisto | ✅ `maxTaskCount` (proxy y repo) | — | — |
| Capacidad / free del repo | ✅ `GET /backupInfrastructure/repositories/states` | — | — |
| CPU cores / RAM | ❌ | ✅ `nproc` / `free` | ✅ input del SE |
| Disco (MB/s) | ❌ | ✅ fio | tipo (SSD/HDD) |
| Red (Mbps) | ❌ | ✅ iperf | enlace declarado |

**Regla de dimensionamiento Veeam (aprox):** ~1 task concurrente **por core** + ~2 GB RAM/task ⇒ `tasks_viables ≈ min(cores, RAM_GB/2)`.
- `maxTaskCount < tasks_viables` → **subir task slots** (mejora *gratis*, sin hardware).
- `maxTaskCount ≈ cores` y ese nodo es el cuello → **agregar cores/RAM, o sumar proxy / extent (SOBR)**.
- **Gate de viabilidad:** infra pobre (pocos cores / RAM chica / disco lento) ⇒ marcar *"infra limitada: más concurrencia NO ayuda sin agregar X"*. No sobre-recomendar.
- **Confianza:** en prod solo hay el *provisto* (`maxTaskCount`) + capacidad ⇒ recomendaciones **condicionales** ("si el proxy tiene >N cores…"); el número firme sale del **probe SSH (lab)** o del **input manual**.

---

## Modelo
Pipeline: **Source → Proxy → Network → Target**. La entrega la limita el eslabón más lento × paralelismo (task slots), y ese paralelismo **está acotado por los recursos** (capa de arriba).

Para una corrida que movió datos:
- `R_src = readSize / duration`  (lado fuente, si readSize>0)
- `R_tgt = transferredSize / duration`  (lado repo)
- `U_s` = utilización de cada stage (de la línea `Load:`)
- **Techo por stage**: el stage que topa (`U_s ≈ máx`, ≥~90%) → su techo `C_s ≈ rate observado`. Para los que no topan → `C_s ≈ rate / U_s` (margen implícito).
  - **Métrica por stage**: Source→`readSize/dur`; Target/Network→`transferredSize/dur`; Proxy→`processedSize/dur`.
  - **Solo si saturó**: el `C_s` en MB/s vale si la corrida movió datos suficientes; si no → `insufficient data` y el número absoluto lo da el **benchmark activo** (fio/iperf). El `Load:` igual señala el cuello (relativo).
- **Cuello(s)** = stage(s) al ~100%.

### Proyección de tiempo (el titular)
- `T_now` = duración observada de la sesión/task.
- Relevar el cuello ⇒ el throughput sube hasta que el **siguiente stage** (utilización `U_next`) llega al 100%:
  - `T_new ≈ T_now × (U_next / U_binding)`
- Reportar por cambio recomendado: `T_now → ~T_new (~Z% más rápido)`, **etiquetado como estimación**.
- Si hay stages **co-limitantes** (varios al ~98%), hay que relevarlos a **todos** para ganar (tocar uno solo = 0 mejora).

### "Cuánto puede entregar" (capacidad + headroom)
- Por repo/proxy: `techo × maxTaskCount` = ingest sostenible.
- Entorno: suma → "sostiene ~X TB/h; usás ~Y%; el cap es **\<stage\>** en **\<repo/proxy\>**".

---

## Recomendaciones (atadas al cuello)
| Cuello | Acción sugerida | Validación |
|---|---|---|
| **Source** | datastore/CBT/snapshot; más concurrencia de origen | — |
| **Proxy** | sumar proxy / más task slots / CPU / modo de transporte | — |
| **Network** | enlace proxy↔repo; colocación | **iperf** (lab) |
| **Target** | repo más rápido / más task slots / SOBR con más extents | **fio** (lab) |

---

## Etiquetas de confianza
`measured` (fio/iperf, lab) · `observed` (corrida real con datos) · `estimate` (proyección) · `insufficient data` (ninguna corrida movió datos).

---

## UI (drill-down dentro de Análisis)
- **Entrada de Análisis = selector de modo, NO corre nada al entrar** (perf):
  1. **Un job** → listbox de jobs → toma la última corrida con datos:
     - barra **4-stage** (Source/Proxy/Network/Target) + `Primary bottleneck`
     - **tabla por VM**: nombre, Full/Incr, bottleneck, processed/read/transferred, duración, **rate calculado**
     - **card de capacidad**: R y T actuales · techo · cuello · headroom
     - **recomendaciones** con **tiempo proyectado**
  2. **Global (todos)** → agregado por período; mismo modelo sumado; pesado → **solo al tocar el botón** y **filtrando no-data antes del N+1**.

---

## Ejemplo real (ambiente grande — DC01)
`read 43 GiB · transferred 12.4 GiB · 126 s` · `Load: Source 62 > Proxy 98 > Network 65 > Target 98` · primary=Target.
- `R_src ≈ 350 MiB/s` · `R_tgt ≈ 100 MiB/s` (techo del Target).
- Cuello: **Proxy+Target (98%)**. Siguiente: **Network 65%**.
- Proyección: relevar proxy+target → `T ≈ 126 × 65/98 ≈ 84 s` → **~1:24 (~35% más rápido)**. Subir Source/Network: **0 mejora**.

---

## Caveats (honestos)
- Son **estimaciones**, no garantías; el tool **se valida solo** comparando *proyectado vs* la próxima corrida real.
- Requiere una corrida que **movió datos**; los incrementales no-op se excluyen.
- El **4-stage es a nivel job** (no por VM) — límite de la REST.
- El detalle por-disco/agente (logs en disco `/var/log/VeeamBackup/`) es **extra de lab** (necesita SSH); no entra en el flujo normal.

---

## Alcance v0.4 (primer punto)
1. Refactor de Análisis a **modo on-demand** (Un job / Global).
2. Parser: reusar `bottleneckFromLogs` (línea `Load:`) + métricas por VM de `taskSessions` (rate calculado por nosotros); filtrar FLR/agent.
3. Selección de la corrida "con datos" por job (mayor `transferredSize`).
4. **Capa de recursos:** `maxTaskCount` (proxy/repo) + `/repositories/states` (capacidad) + **input manual** opcional de cores/RAM; gate de viabilidad.
5. Card de capacidad + headroom + **proyección de tiempo** + recomendaciones **resource-gated**.
6. Etiquetas de confianza (measured/observed/estimate/insufficient).
7. (Perf) filtrar no-data antes del N+1.

Fuera de v0.4 (después): probe SSH de recursos en lab (`nproc`/`free`), restore pipeline, fio/iperf integrado a la recomendación, detalle por-disco.
