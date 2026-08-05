package benchmark

import "testing"

// JSON real de fio (recortado a lo que consumimos) para confirmar que el parser
// del canal SSH mapea bien el schema: bw en KiB/s, iops, lat_ns.mean en ns.
const realFioJSON = `{
  "fio version": "fio-3.35",
  "jobs": [
    {"jobname":"seqwrite","read":{"bw":0,"iops":0,"lat_ns":{"mean":0}},
     "write":{"bw":913149,"iops":891.747767,"lat_ns":{"mean":2239849}}},
    {"jobname":"seqread","read":{"bw":2342912,"iops":2287,"lat_ns":{"mean":6990920}},
     "write":{"bw":0,"iops":0,"lat_ns":{"mean":0}}}
  ]
}`

func TestNormalizeRealFio(t *testing.T) {
	rep, err := parseFioReport([]byte(realFioJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rows := normalizeFio(rep)
	if len(rows) != 2 {
		t.Fatalf("esperaba 2 filas, hay %d", len(rows))
	}

	w := rows[0]
	if w.Name != "seqwrite" || w.Mode != "write" {
		t.Errorf("fila0: %s/%s", w.Name, w.Mode)
	}
	if w.BwMbps != 891.7 || w.Iops != 892 || w.LatMs != 2.24 {
		t.Errorf("seqwrite: bw=%v iops=%v lat=%v (esperaba 891.7/892/2.24)", w.BwMbps, w.Iops, w.LatMs)
	}

	r := rows[1]
	if r.Name != "seqread" || r.Mode != "read" {
		t.Errorf("fila1: %s/%s", r.Name, r.Mode)
	}
	if r.BwMbps != 2288 || r.Iops != 2287 || r.LatMs != 6.991 {
		t.Errorf("seqread: bw=%v iops=%v lat=%v (esperaba 2288/2287/6.991)", r.BwMbps, r.Iops, r.LatMs)
	}
}

// El comando que genera el tool debe coincidir con el que validamos en campo.
func TestFioCommandMatchesField(t *testing.T) {
	got := fioCommand("/var/lib/veeam/backup", "randwrite", "libaio", 15)
	want := "fio --name=randwrite --directory='/var/lib/veeam/backup' --rw=randwrite --bs=4k " +
		"--size=1G --numjobs=2 --iodepth=32 --ioengine=libaio --direct=1 --runtime=15 " +
		"--time_based --group_reporting --unlink=1 --output-format=json"
	if got != want {
		t.Errorf("comando fio distinto:\n got: %s\nwant: %s", got, want)
	}
}
