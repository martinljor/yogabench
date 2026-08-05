package benchmark

import (
	"encoding/json"
	"fmt"
	"math"
)

// Estructura (parcial) del JSON de `fio --output-format=json`. Es el punto donde
// "salida de fio" se vuelve "metricas": lo comparten el MockExecutor (genera
// esta forma) y el SshExecutor (la recibe real del appliance).
type fioReport struct {
	Jobs []fioJobJSON `json:"jobs"`
}

type fioJobJSON struct {
	Jobname string `json:"jobname"`
	Read    fioDir `json:"read"`
	Write   fioDir `json:"write"`
}

type fioDir struct {
	Bw    float64 `json:"bw"` // KiB/s
	Iops  float64 `json:"iops"`
	LatNs struct {
		Mean float64 `json:"mean"`
	} `json:"lat_ns"`
}

// normalizeFio: fio-JSON -> filas normalizadas. Cada corrida trae read y write;
// devolvemos el lado con actividad (segun el modo del test).
func normalizeFio(rep fioReport) []DiskRow {
	out := make([]DiskRow, 0, len(rep.Jobs))
	for _, j := range rep.Jobs {
		isWrite := j.Write.Iops > 0 && j.Read.Iops == 0
		sec := j.Read
		mode := "read"
		if isWrite {
			sec, mode = j.Write, "write"
		}
		out = append(out, DiskRow{
			Name:   j.Jobname,
			Mode:   mode,
			BwMbps: round1(sec.Bw / 1024), // KiB/s -> MB/s
			Iops:   math.Round(sec.Iops),
			LatMs:  roundN(sec.LatNs.Mean/1_000_000, 3), // ns -> ms
		})
	}
	return out
}

func parseFioReport(raw []byte) (fioReport, error) {
	var rep fioReport
	err := json.Unmarshal(raw, &rep)
	return rep, err
}

// --- comando fio (usado por el SshExecutor real) ---------------------------

type fioParams struct {
	bs, rw           string
	iodepth, numjobs int
}

// Parametros por sub-test. libaio + direct=1 para respetar la profundidad de
// cola (fio con engine sincrono la capa a 1 y subestima los IOPS aleatorios).
func fioParamsFor(name string) fioParams {
	switch name {
	case "seqread":
		return fioParams{"1M", "read", 8, 2}
	case "seqwrite":
		return fioParams{"1M", "write", 8, 2}
	case "randread":
		return fioParams{"4k", "randread", 32, 2}
	default: // randwrite
		return fioParams{"4k", "randwrite", 32, 2}
	}
}

// fioCommand arma la linea de fio. `engine` permite fallback (libaio -> psync)
// si el appliance no tiene el modulo async. --unlink=1 borra el archivo de prueba.
func fioCommand(dir, name, engine string, duration int) string {
	p := fioParamsFor(name)
	return fmt.Sprintf(
		"fio --name=%s --directory='%s' --rw=%s --bs=%s --size=1G --numjobs=%d "+
			"--iodepth=%d --ioengine=%s --direct=1 --runtime=%d --time_based "+
			"--group_reporting --unlink=1 --output-format=json",
		name, dir, p.rw, p.bs, p.numjobs, p.iodepth, engine, duration)
}
