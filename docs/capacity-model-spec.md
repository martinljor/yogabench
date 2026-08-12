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

---

# v0.5 · Motor de VEREDICTO (autogestión)

**El problema de v0.4:** mostrábamos **datos** (como Veeam ONE) — 4-stage, rates, recomendaciones genéricas escondidas en un colapsable. El SE tenía que interpretar.
**v0.5:** el tool **razona y dictamina**. La salida principal ya no es una tabla, es **una conclusión accionable**.

## Las 4 señales que se sintetizan
| # | Señal | De dónde | Aporta |
|---|---|---|---|
| 1 | **Observado** | REST (sesiones + `Load:`) | qué stage topa, % de corridas, rates, duración |
| 2 | **Causa** | logs en disco del VBR (modo deep, SMB/C$) | transporte real y **por qué**, 4-stage **por VM**, opciones del job |
| 3 | **Recursos** | `maxTaskCount` + cores/RAM + `/repositories/states` | si la recomendación es viable (gate) |
| 4 | **Medido** | fio / iperf (lab) | **techo real** del hardware *(pendiente de wire)* |

## Salida (`internal/analysis/verdict.go` → `BuildVerdict`)
- **Headline**: qué topa y cuánto (`hl.bound` / `hl.bound2` co-limitantes / `hl.boundnopct` sin % / `hl.balanced` / `hl.nodata`).
- **Causa**: por qué (`cause.nbdHotadd`, `cause.slots`, `cause.repowrite`, …) con flag **`causeKnown`** — sin deep **no se inventa** una causa: se ofrece correr el deep.
- **Ganancia**: `gainPct` + `currentSec → targetSec`, del modelo de proyección **acotado a `maxCredibleGain` (75%)**.
- **Acciones** rankeadas por impacto (`high` > `hygiene` > `medium` > `verify` > `info`); la que libera el cuello lleva la **ganancia**; `alt` marca alternativas **no acumulativas**.
- **Señales**: qué entró y qué falta (✓/○) — le dice al usuario **cómo hacer el veredicto más firme**.

## Reglas clave (deterministas, testeadas en `verdict_test.go`)
- **Source + deep=nbd + hotadd no disponible** → causa confirmada, acción #1 `act.hotadd` con ganancia. *El caso estrella que vONE no da.*
- **Proxy/Target + cores conocidos**: `slots < ~min(cores, RAM/2)` → `act.slots` **firme** con el número exacto; `viable<=2` → `act.scaleHost` (**subir concurrencia no ayuda**); ya al límite → `act.atCapacity`.
- **Sin cores/RAM** → nunca se promete un número: `act.slotsUnknown` como *verify*.
- **Serialización** (deep): con `S`=Σ duración por VM y `T`=duración de la corrida, `S/T ≈ 1` (y VMs parejas) → `act.serial`. `S << T` **no** es serialización sino overhead: no se opina.
- **Skew**: ≥3 VMs y una se lleva ≥60% del tiempo → `act.skew`.
- **Repo <10% libre** → `act.repoFree` (*high* si <5%).
- **Confianza `insufficient`** → **no hay veredicto de performance**: sólo `act.window`.

## i18n
El motor emite **código + params** (`vd.<code>`) **y** el texto en inglés como respaldo (logs/diagnóstico). El WebUI traduce a ES/EN/PT: el veredicto se ve en el idioma del cliente.

## Por qué NO un LLM (decisión)
El núcleo es **determinista**: mismo input = mismo veredicto, auditable, **offline** y sin que ningún dato del cliente salga del sitio (nuestro diferencial). Un LLM queda como **narrador opcional** más adelante (opt-in, con modelo **local**), nunca como el motor.

## Señal #4 — MEDIDO (v0.6.1)
La señal que le falta a Veeam ONE. Un `Target 96%` de la REST **no distingue** dos situaciones que llevan a decisiones **opuestas**:

| Medido (fio) | Job escribe | Veredicto | Acción |
|---|---|---|---|
| 800 MB/s | 100 MB/s (**13%**) | `cause.starvedTarget` — **el disco NO es el límite** | subir streams/slots + **"no compres storage"** |
| 120 MB/s | 110 MB/s (**92%**) | `cause.maxedTarget` — **el disco SÍ es el límite** | storage más rápido / más extents |

Umbrales: `starvedPct=60` · `maxedPct=80`. Idem para **Network** con iperf (`cause.starvedNet` / `cause.maxedNet`, el write del job pasado a Mbps). Cuando hay medición **no se vuelve a pedir** fio/iperf, y la causa medida es **final** (`causeFinal`): no la pisa una causa deducida como `cause.slots`.

De dónde sale: `Manager.DiskWriteMBps(sessionID, repoID)` (mejor `seqwrite` completado de ese repo) y `Manager.Iperf(sessionID)` (último iperf, guardado con `SetIperf`). El **server** arma `analysis.Measured` (`measuredFor`) y rehace el veredicto — el paquete `analysis` no depende de `benchmark`.

## Pendiente
- Derivar cores/RAM de la utilización en vez del input manual.
- Jobs que no lista `v1/jobs` (AHV/Proxmox/Morpheus/NAS/Object Storage/Solaris).

---

# v0.6.1 · El deep analiza la corrida CORRECTA

Los Job/Task logs de Veeam **acumulan varias corridas en el mismo archivo**. El parser usaba `FindStringSubmatch` (primera coincidencia = corrida **más vieja**) para el `Load:`/`Busy:`/transporte/opciones, pero las duraciones salían de un mapa donde la última gana → **el veredicto podía mezclar dos corridas distintas**.

Fix (`internal/deeplog/parse.go`):
- `lastRunSegment(s)` — la línea `Load:`/`Busy:` cierra cada corrida, así que todo lo que viene después de la **penúltima** pertenece a la última. Es la única frontera confiable sin depender de marcas internas de Veeam (que cambian entre versiones).
- `lastSubmatch(re, s)` — última coincidencia, no la primera, para Load/primary/transporte/dedup/compresión/bloque.
- `RunAt` — timestamp de la corrida analizada, **tal como lo escribió el log** (no se parsea la fecha: el formato depende del locale del VBR). Se muestra como pill en el deep para que el usuario **verifique** de qué corrida se está hablando.
- Notas explícitas cuando **no hay `Task.*.log`** (agentes/plugins) o **no hay línea `Load:`**, en vez de mostrar una tabla vacía sin explicación.

Tests: `parse_multirun_test.go` (log sintético de 2 corridas con formato calcado del real) fija que todo — Load, primary, transporte+motivo, opciones, duración, Busy por VM y discos sin duplicar — sale de la corrida más reciente.

---

# v0.6 · VEREDICTO DEL ENTORNO (assessment)

El veredicto por job dice qué arreglar en **un job**; esto dice qué arreglar en la **infra**. Se calcula desde los `Record` que ya trae el modo Global → **cero llamadas REST extra** (`internal/analysis/assessment.go`).

## Métricas (y por qué cada una es defendible)
| Métrica | Cómo | Por qué así |
|---|---|---|
| **Capacidad sostenida** | bytes repartidos en las horas que abarca cada corrida (`spread`), se toma el **bucket máximo** | Una corrida de 02:00 a 06:00 **no** movió todo a las 02:00. Da "lo que la infra sostuvo de verdad", no un promedio aguado |
| **Cuello recurrente** | distribución del `primary` **ponderada por bytes transferidos** | **La diferencia con vONE**: vONE cuenta corridas, así que 20 incrementales no-op le ganan a un full de 2 TB. Acá el cuello es el que afecta a los bytes reales |
| **Hotspots** | repo/proxy que concentra el dato *binding*; rate = bytes / **unión de intervalos** | Sumar duraciones duplica el tiempo cuando dos jobs le pegan en paralelo y **subestima** el rate del recurso |
| **Ventana de backup** | carga por hora del día (de los mismos buckets = actividad, no hora de arranque) + **jobs distintos** que arrancan en cada hora | Detecta el pico evitable: escalonar es **gratis**, no requiere hardware |

## Acciones del entorno
`act.envRepo` (repo que topa → fio + task slots) · `act.envProxy` (proxy/enlace → iperf + CPU) · `act.envStagger` (≥3 jobs distintos en la hora pico con ≥40% del dato) · `act.envNoData` (jobs que no se pueden dictaminar) · `act.envMeasure` (medir el techo: la pata que falta). Mismo tipo `Action` que el veredicto por job → el WebUI las pinta igual.

## Umbrales
`hotspotShare=35%` del dato binding para nombrar un recurso · `envStageShare=40%` para declarar cuello de entorno · `staggerJobs=3` jobs distintos en la misma hora · `lowFloor=16 MiB` para que una corrida cuente como "movió datos".

## UI
El modo **Global** ahora abre con el veredicto del entorno (headline + KPIs + acciones + histograma por hora); el agregado por proxy/repo y el resumen del período quedan colapsados. El resumen del período **cuenta corridas a propósito** (como vONE) con una nota que explica el contraste: es la demostración del diferencial.

## Diagnóstico
El JSON de diagnóstico ahora incluye `analyzed`: los veredictos por job y el assessment que el usuario **ya vio en pantalla** (cacheados en la sesión, cero REST extra) → se reproduce offline lo que vio y se calibran las reglas.
