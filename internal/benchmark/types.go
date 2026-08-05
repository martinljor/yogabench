package benchmark

// DiskRow: metrica normalizada de un sub-test de disco + su anotacion vs baseline.
type DiskRow struct {
	Name           string  `json:"name"`
	Mode           string  `json:"mode"`
	BwMbps         float64 `json:"bw_mbps"`
	Iops           float64 `json:"iops"`
	LatMs          float64 `json:"lat_ms"`
	Resource       string  `json:"resource"`
	Expected       float64 `json:"expected"`
	ExpectedUnit   string  `json:"expected_unit"`
	MeasuredMetric float64 `json:"measured_metric"`
	PctOfExpected  int     `json:"pct_of_expected"`
	Status         string  `json:"status"`
}

// SimpleMetric: recurso de una sola cifra (red / computo) anotado vs baseline.
type SimpleMetric struct {
	Resource      string  `json:"resource"`
	Label         string  `json:"label"`
	Value         float64 `json:"value"`
	Unit          string  `json:"unit"`
	Expected      float64 `json:"expected"`
	ExpectedLabel string  `json:"expected_label,omitempty"`
	PctOfExpected int     `json:"pct_of_expected"`
	Status        string  `json:"status"`
}

// Results agrupa los tres recursos posibles. nil = no corrido (JSON null).
type Results struct {
	Disk    []DiskRow     `json:"disk"`
	Net     *SimpleMetric `json:"net"`
	Compute *SimpleMetric `json:"compute"`
}

// Job: ciclo de vida de un benchmark (queued -> running -> completed/failed).
type Job struct {
	ID              string  `json:"id"`
	SessionID       string  `json:"session_id"`
	RepositoryID    string  `json:"repository_id"`
	RepositoryLabel string  `json:"repository_label"`
	Operation       string  `json:"operation"`
	Resource        string  `json:"resource"`
	ProxyLabel      string  `json:"proxy_label"`
	MountLabel      string  `json:"mount_label"`
	OSType          string  `json:"os_type"`
	Tool            string  `json:"tool"`
	DiskBaseline    string  `json:"disk_baseline"`
	NetBaseline     string  `json:"net_baseline"`
	Status          string  `json:"status"`
	Progress        int     `json:"progress"`
	Results         Results `json:"results"`
	Error           string  `json:"error"`
}

// --- opciones de benchmark (repos/proxies para el formulario) ---------------

type MountInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
}

type RepoOption struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	HostOS string     `json:"host_os"`
	Mount  *MountInfo `json:"mount"`
}

type ProxyOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
}
