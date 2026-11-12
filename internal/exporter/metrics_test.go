package exporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthState(t *testing.T) {
	cases := []struct {
		name string
		c    Container
		in   *Inspect
		want string
	}{
		{"healthy list", Container{State: "running", Status: "Up 2 hours (healthy)"}, nil, "healthy"},
		{"unhealthy list", Container{State: "running", Status: "Up 5 minutes (unhealthy)"}, nil, "unhealthy"},
		{"starting list", Container{State: "running", Status: "Up 3 seconds (health: starting)"}, nil, "starting"},
		{"running list", Container{State: "running", Status: "Up 2 hours"}, nil, "running"},
		{"stopped list", Container{State: "exited", Status: "Exited (0) 1 hour ago"}, nil, "stopped"},
		{"unknown list", Container{State: "paused", Status: "Paused"}, nil, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HealthState(tc.c, tc.in); got != tc.want {
				t.Errorf("HealthState() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHealthStateInspectStateHealth(t *testing.T) {
	raw := readTestdata(t, "inspect_health.json")
	var in Inspect
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	c := Container{State: "running", Status: "Up (unhealthy)"}
	if got := HealthState(c, &in); got != "healthy" {
		t.Errorf("State.Health wins: got %q, want healthy", got)
	}
	if in.State.Health == nil {
		t.Fatal("expected State.Health unmarshalled")
	}
}

func TestInspectUnmarshalWrongTopLevelHealth(t *testing.T) {
	// Top-level Health is not where Docker puts it; must not populate State.Health.
	var in Inspect
	if err := json.Unmarshal([]byte(`{"Health":{"Status":"healthy"},"State":{}}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.State.Health != nil {
		t.Error("top-level Health must not bind to removed field")
	}
}

func TestStatsUnmarshalRealDockerJSON(t *testing.T) {
	raw := readTestdata(t, "docker_stats_sample.json")
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.CPUStats.CPUUsage.TotalUsage != 3_500_000_000 {
		t.Errorf("total_usage = %d, want 3.5e9", s.CPUStats.CPUUsage.TotalUsage)
	}
	if got := CPUSecs(&s); got != 3.5 {
		t.Errorf("CPUSecs = %v, want 3.5 (not zero)", got)
	}
	if s.CPUStats.CPUUsage.UsageInKernelmode != 500_000_000 {
		t.Errorf("kernel mode = %d", s.CPUStats.CPUUsage.UsageInKernelmode)
	}
}

func TestWorkingSetBytesInactiveFile(t *testing.T) {
	raw := readTestdata(t, "docker_stats_sample.json")
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	inactive := inactiveFileBytes(&s)
	want := s.MemoryStats.Usage - inactive
	if got := WorkingSetBytes(&s); got != want {
		t.Errorf("WorkingSetBytes = %d, want %d (usage - inactive_file)", got, want)
	}
}

func TestWorkingSetBytesCgroupV1JSON(t *testing.T) {
	raw := readTestdata(t, "docker_stats_cgroupv1.json")
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.MemoryStats.Stats.TotalInactiveFile == nil || *s.MemoryStats.Stats.TotalInactiveFile != 200 {
		t.Fatal("expected v1 total_inactive_file unmarshalled under stats")
	}
	if got := WorkingSetBytes(&s); got != 800 {
		t.Errorf("got %d, want 800", got)
	}
}

func TestInactiveFileV1PrecedenceWhenBothPresent(t *testing.T) {
	raw := readTestdata(t, "docker_stats_both_inactive_fields.json")
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if got := inactiveFileBytes(&s); got != 300 {
		t.Errorf("inactive = %d, want v1 total_inactive_file 300", got)
	}
	if got := WorkingSetBytes(&s); got != 4700 {
		t.Errorf("working set = %d, want 4700", got)
	}
}

func TestInactiveFileV1ZeroStillUsed(t *testing.T) {
	zero := uint64(0)
	s := &Stats{}
	s.MemoryStats.Usage = 1000
	hundred := uint64(100)
	s.MemoryStats.Stats.TotalInactiveFile = &zero
	s.MemoryStats.Stats.InactiveFile = &hundred
	if got := inactiveFileBytes(s); got != 0 {
		t.Errorf("v1 zero must win over v2: got %d", got)
	}
	if got := WorkingSetBytes(s); got != 1000 {
		t.Errorf("working set = %d, want 1000", got)
	}
}

func TestWorkingSetBytesFloorAtZero(t *testing.T) {
	fiveHundred := uint64(500)
	s := &Stats{}
	s.MemoryStats.Usage = 100
	s.MemoryStats.Stats.InactiveFile = &fiveHundred
	if got := WorkingSetBytes(s); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestSpecMemoryLimitBytes(t *testing.T) {
	var in Inspect
	in.HostConfig.Memory = 536870912
	if got := SpecMemoryLimitBytes(&in, nil); got != 536870912 {
		t.Errorf("inspect limit = %d", got)
	}
	in.HostConfig.Memory = 0
	if got := SpecMemoryLimitBytes(&in, nil); got != 0 {
		t.Errorf("unlimited should be 0, got %d", got)
	}
	s := &Stats{}
	s.MemoryStats.Limit = 999
	if got := SpecMemoryLimitBytes(nil, s); got != 999 {
		t.Errorf("nil inspect fallback = %d", got)
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

func TestDiskIOEmptyBlkio(t *testing.T) {
	raw := readTestdata(t, "docker_stats_cgroupv2_empty_blkio.json")
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	r, w := DiskIO(&s)
	if r != 0 || w != 0 {
		t.Errorf("empty blkio: got (%d,%d), want (0,0)", r, w)
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

func TestCPUSecsNil(t *testing.T) {
	if got := CPUSecs(nil); got != 0 {
		t.Errorf("CPUSecs(nil) = %v", got)
	}
}

func TestIsRunning(t *testing.T) {
	if !IsRunning(Container{State: "running"}) {
		t.Error("expected running")
	}
	if IsRunning(Container{State: "exited"}) {
		t.Error("exited is not running")
	}
}

func TestRegressionHealthyContainerCPUAndUsage(t *testing.T) {
	// Guards bugs #1 and #3: usage must not depend on health state "healthy",
	// and CPU must unmarshal from nested cpu_stats.
	raw := readTestdata(t, "docker_stats_sample.json")
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if CPUSecs(&s) == 0 {
		t.Fatal("CPU must be non-zero after unmarshalling real-shaped JSON")
	}
	c := Container{State: "running", Status: "Up (healthy)"}
	var in Inspect
	_ = json.Unmarshal(readTestdata(t, "inspect_health.json"), &in)
	if HealthState(c, &in) != "healthy" {
		t.Fatal("fixture should be healthy")
	}
	if !IsRunning(c) {
		t.Fatal("healthy container must still be Docker-running")
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
