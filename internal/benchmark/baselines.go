package benchmark

import "math"

// Baselines de rendimiento esperado ("lo esperado") por recurso. Convierten un
// numero medido en un diagnostico (ok/warn/low) comparando contra lo esperado,
// que es la idea de cuello de botella de Veeam. Valores APROXIMADOS (ballpark).

type diskBaseline struct {
	Label                                  string
	SeqRead, SeqWrite, RandRead, RandWrite float64
}

var diskBaselines = map[string]diskBaseline{
	"nvme-ssd": {"NVMe SSD", 3000, 2200, 350000, 250000},
	"sata-ssd": {"SATA SSD", 550, 500, 90000, 70000},
	"sas-hdd":  {"SAS 10k HDD (RAID)", 250, 220, 800, 500},
	"sata-hdd": {"SATA 7.2k HDD", 170, 150, 180, 140},
}

// orden estable para el catalogo (los mapas de Go iteran al azar).
var diskOrder = []string{"nvme-ssd", "sata-ssd", "sas-hdd", "sata-hdd"}

const defaultDisk = "sata-ssd"

type netBaseline struct {
	Label string
	Mbps  float64
}

var netBaselines = map[string]netBaseline{
	"1gbe":  {"1 GbE", 118},
	"10gbe": {"10 GbE", 1180},
	"25gbe": {"25 GbE", 2950},
	"40gbe": {"40 GbE", 4720},
}

var netOrder = []string{"1gbe", "10gbe", "25gbe", "40gbe"}

const (
	defaultNet = "10gbe"
	okRatio    = 0.85
	warnRatio  = 0.60
)

// Exportados para los catalogos del handler (valores por defecto del selector).
const (
	DefaultDisk = defaultDisk
	DefaultNet  = defaultNet
)

type catalogEntry struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

func DiskCatalog() []catalogEntry {
	out := make([]catalogEntry, 0, len(diskOrder))
	for _, k := range diskOrder {
		out = append(out, catalogEntry{k, diskBaselines[k].Label})
	}
	return out
}

func NetCatalog() []catalogEntry {
	out := make([]catalogEntry, 0, len(netOrder))
	for _, k := range netOrder {
		out = append(out, catalogEntry{k, netBaselines[k].Label})
	}
	return out
}

func verdict(ratio float64) string {
	switch {
	case ratio >= okRatio:
		return "ok"
	case ratio >= warnRatio:
		return "warn"
	default:
		return "low"
	}
}

func expectedForTest(b diskBaseline, name string) float64 {
	switch name {
	case "seqread":
		return b.SeqRead
	case "seqwrite":
		return b.SeqWrite
	case "randread":
		return b.RandRead
	case "randwrite":
		return b.RandWrite
	}
	return 0
}

// annotateDisk agrega a cada fila: esperado, unidad, % y veredicto.
// seq -> compara MB/s ; rand -> compara IOPS.
func annotateDisk(rows []DiskRow, baselineKey string) []DiskRow {
	b, ok := diskBaselines[baselineKey]
	if !ok {
		b = diskBaselines[defaultDisk]
	}
	for i := range rows {
		r := &rows[i]
		isSeq := len(r.Name) >= 3 && r.Name[:3] == "seq"
		measured := r.Iops
		unit := "IOPS"
		if isSeq {
			measured = r.BwMbps
			unit = "MB/s"
		}
		expected := expectedForTest(b, r.Name)
		ratio := 0.0
		if expected > 0 {
			ratio = measured / expected
		}
		r.Resource = "disk"
		r.Expected = expected
		r.ExpectedUnit = unit
		r.MeasuredMetric = round1(measured)
		r.PctOfExpected = int(math.Round(ratio * 100))
		r.Status = verdict(ratio)
	}
	return rows
}
