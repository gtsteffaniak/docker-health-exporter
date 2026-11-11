package exporter

import "testing"

func TestHealthState(t *testing.T) {
	cases := map[string]Container{
		"healthy":   {State: "running", Status: "Up 2 hours (healthy)"},
		"unhealthy": {State: "running", Status: "Up 5 minutes (unhealthy)"},
		"starting":  {State: "running", Status: "Up 3 seconds (health: starting)"},
		"running":   {State: "running", Status: "Up 2 hours"},
		"stopped":   {State: "exited", Status: "Exited (0) 1 hour ago"},
	}
	for want, c := range cases {
		if got := HealthState(c, nil); got != want {
			t.Errorf("HealthState(%q) = %q, want %q", c.Status, got, want)
		}
	}
}

func TestHealthStateInspect(t *testing.T) {
	var in Inspect
	in.Health = &struct {
		Status string `json:"Status"`
	}{Status: "healthy"}
	c := Container{State: "running", Status: "Up (unhealthy)"}
	if got := HealthState(c, &in); got != "healthy" {
		t.Errorf("inspect health wins over Status: got %q, want healthy", got)
	}
}

func TestStartedUnix(t *testing.T) {
	if got := StartedUnix("0001-01-01T00:00:00Z"); got != 0 {
		t.Errorf("never-started container = %d, want 0", got)
	}
	if got := StartedUnix("2026-11-10T22:00:00.123456789Z"); got != 1794348000 {
		t.Errorf("startedUnix = %d, want 1794348000", got)
	}
}

func TestContainerName(t *testing.T) {
	if got := ContainerName([]string{"/immich_server"}); got != "immich_server" {
		t.Errorf("ContainerName = %q", got)
	}
}

func TestComposeLabels(t *testing.T) {
	c := Container{Labels: map[string]string{"com.docker.compose.project": "immich", "com.docker.compose.service": "server"}}
	if p, s := ComposeLabels(c, "immich_server"); p != "immich" || s != "server" {
		t.Errorf("compose container = %q/%q", p, s)
	}
	if p, s := ComposeLabels(Container{}, "portainer"); p != "standalone" || s != "portainer" {
		t.Errorf("docker run container = %q/%q, want standalone/portainer", p, s)
	}
}

func TestDiskIO(t *testing.T) {
	s := &Stats{}
	s.BlkioStats.IoServiceBytesRecursive = []struct {
		Op    string `json:"op"`
		Value uint64 `json:"value"`
	}{{"Read", 100}, {"Write", 200}, {"Read", 50}}
	if r, w := DiskIO(s); r != 150 || w != 200 {
		t.Errorf("DiskIO = (%d,%d), want (150,200)", r, w)
	}
}

func TestNetworkIO(t *testing.T) {
	s := &Stats{}
	s.Networks = map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{"eth0": {RxBytes: 10, TxBytes: 20}, "eth1": {RxBytes: 5, TxBytes: 7}}
	if rx, tx := NetworkIO(s); rx != 15 || tx != 27 {
		t.Errorf("NetworkIO = (%d,%d), want (15,27)", rx, tx)
	}
}

func TestCPUSecsAndWorkingSet(t *testing.T) {
	s := &Stats{}
	s.CPUUsage.TotalUsage = 2_000_000_000 // 2s
	s.MemoryStats.Usage = 12345
	if got := CPUSecs(s); got != 2 {
		t.Errorf("CPUSecs = %v, want 2", got)
	}
	if got := WorkingSetBytes(s); got != 12345 {
		t.Errorf("WorkingSetBytes = %d, want 12345", got)
	}
}
