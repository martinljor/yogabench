"""
Baselines de rendimiento esperado ("lo esperado") por recurso.
---------------------------------------------------------------
Convierte un numero suelto en un diagnostico: "esto es bueno o malo?". Es la
idea de cuello de botella de Veeam: se compara lo MEDIDO contra lo ESPERADO.

Tres recursos:
  - disco    -> por tipo de almacenamiento (NVMe / SATA SSD / SAS HDD / SATA HDD)
  - red      -> por velocidad de enlace (1 / 10 / 25 / 40 GbE)
  - computo  -> throughput de compresion de referencia (por ahora, valor fijo)

IMPORTANTE: los valores son APROXIMADOS (ballpark), para dar contexto, no specs
exactas. Ajustar al hardware real del cliente.
"""

# --- Disco (unidad por test: seq=MB/s, rand=IOPS) ---------------------------
DISK_BASELINES = {
    "nvme-ssd": {"label": "NVMe SSD", "seqread": 3000, "seqwrite": 2200, "randread": 350000, "randwrite": 250000},
    "sata-ssd": {"label": "SATA SSD", "seqread": 550, "seqwrite": 500, "randread": 90000, "randwrite": 70000},
    "sas-hdd":  {"label": "SAS 10k HDD (RAID)", "seqread": 250, "seqwrite": 220, "randread": 800, "randwrite": 500},
    "sata-hdd": {"label": "SATA 7.2k HDD", "seqread": 170, "seqwrite": 150, "randread": 180, "randwrite": 140},
}
DEFAULT_DISK = "sata-ssd"

# --- Red (throughput efectivo aprox, ~0.94 de la tasa de linea, en MB/s) -----
NET_BASELINES = {
    "1gbe":  {"label": "1 GbE", "mbps": 118},
    "10gbe": {"label": "10 GbE", "mbps": 1180},
    "25gbe": {"label": "25 GbE", "mbps": 2950},
    "40gbe": {"label": "40 GbE", "mbps": 4720},
}
DEFAULT_NET = "10gbe"

# --- Computo (MB/s de compresion de referencia) -----------------------------
COMPUTE_EXPECTED_MBPS = 550

# Umbrales de veredicto (ratio medido/esperado).
_OK = 0.85
_WARN = 0.60


def verdict(ratio: float) -> str:
    if ratio >= _OK:
        return "ok"
    if ratio >= _WARN:
        return "warn"
    return "low"


def disk_catalog() -> list:
    return [{"key": k, "label": v["label"]} for k, v in DISK_BASELINES.items()]


def net_catalog() -> list:
    return [{"key": k, "label": v["label"]} for k, v in NET_BASELINES.items()]


def annotate_disk(results: list, baseline_key: str) -> list:
    """Agrega a cada fila de disco: esperado, unidad, % y veredicto.
    seq -> compara MB/s ; rand -> compara IOPS."""
    b = DISK_BASELINES.get(baseline_key, DISK_BASELINES[DEFAULT_DISK])
    for r in results:
        is_seq = r["name"].startswith("seq")
        measured = r["bw_mbps"] if is_seq else r["iops"]
        expected = b.get(r["name"], 0)
        ratio = (measured / expected) if expected else 0
        r["resource"] = "disk"
        r["expected"] = expected
        r["expected_unit"] = "MB/s" if is_seq else "IOPS"
        r["measured_metric"] = round(measured, 1)
        r["pct_of_expected"] = round(ratio * 100)
        r["status"] = verdict(ratio)
    return results


def annotate_net(measured_mbps: float, net_key: str) -> dict:
    b = NET_BASELINES.get(net_key, NET_BASELINES[DEFAULT_NET])
    expected = b["mbps"]
    ratio = measured_mbps / expected if expected else 0
    return {
        "resource": "net", "label": "Throughput de red",
        "value": round(measured_mbps, 1), "unit": "MB/s",
        "expected": expected, "expected_label": b["label"],
        "pct_of_expected": round(ratio * 100), "status": verdict(ratio),
    }


def annotate_compute(measured_mbps: float) -> dict:
    expected = COMPUTE_EXPECTED_MBPS
    ratio = measured_mbps / expected if expected else 0
    return {
        "resource": "compute", "label": "Compresion (CPU)",
        "value": round(measured_mbps, 1), "unit": "MB/s",
        "expected": expected, "pct_of_expected": round(ratio * 100),
        "status": verdict(ratio),
    }
