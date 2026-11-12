package exporter

import (
	"bufio"
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gtsteffaniak/docker-health-exporter/internal/prom"
)

var updateGolden = flag.Bool("update", false, "regenerate testdata/metrics_golden.prom")

// monitoringFamilies lists every metric family the gtsteffaniak/monitoring
// Docker dashboards and rules-docker.yaml query today.
var monitoringFamilies = []struct {
	name string
	typ  string
}{
	{"docker_health_exporter_up", "gauge"},
	{"container_health_status", "gauge"},
	{"container_restart_count", "counter"},
	{"container_started_time_seconds", "gauge"},
	{"container_start_time_seconds", "gauge"},
	{"container_cpu_usage_seconds_total", "counter"},
	{"container_memory_working_set_bytes", "gauge"},
	{"container_memory_usage_bytes", "gauge"},
	{"container_network_receive_bytes_total", "counter"},
	{"container_network_transmit_bytes_total", "counter"},
	{"container_fs_reads_bytes_total", "counter"},
	{"container_fs_writes_bytes_total", "counter"},
	{"container_oom_events_total", "counter"},
}

func TestGoldenMetricsEquivalence(t *testing.T) {
	body := scrapeMetrics(t, regressionFixture(t))
	goldenPath := filepath.Join("testdata", "metrics_golden.prom")

	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if body != string(want) {
		t.Fatalf("metrics output differs from golden %s (run go test -update)", goldenPath)
	}
}

func TestMonitoringFamilyPresence(t *testing.T) {
	body := scrapeMetrics(t, regressionFixture(t))
	types := parseTypes(body)

	for _, fam := range monitoringFamilies {
		if types[fam.name] != fam.typ {
			t.Errorf("family %s: got TYPE %q, want %q", fam.name, types[fam.name], fam.typ)
		}
		if !strings.Contains(body, fam.name+"{") && fam.name != "docker_health_exporter_up" {
			t.Errorf("missing samples for %s", fam.name)
		}
	}
	if !strings.Contains(body, "docker_health_exporter_up 1") {
		t.Error("missing docker_health_exporter_up sample")
	}

	assertMetric(t, body, `container_started_time_seconds{.*name="healthy_svc".*} [1-9]`)
	assertMetric(t, body, `container_cpu_usage_seconds_total{.*name="healthy_svc".*} 3.5`)
}

func TestHealthyContainerMustEmitUsageRegression(t *testing.T) {
	body := scrapeMetrics(t, regressionFixture(t))
	n := strings.Count(body, `container_memory_usage_bytes{`)
	if n < 6 {
		t.Errorf("expected usage for each running container, got %d memory_usage series", n)
	}
}

func TestEmitUsageOOMCounterPresent(t *testing.T) {
	var buf bytes.Buffer
	enc := prom.NewEncoder(&buf)
	labels := map[string]string{
		"name": "x", "container": "x", "container_id": "id", "image": "i",
		"compose_project": "standalone", "compose_service": "x",
	}
	emitOOMEvents(enc, labels, 0)
	out := buf.String()
	if !strings.Contains(out, "# TYPE container_oom_events_total counter") {
		t.Fatal("missing oom TYPE")
	}
	if !regexp.MustCompile(`container_oom_events_total\{.*\} 0`).MatchString(out) {
		t.Fatal("oom must be 0")
	}
}

func parseTypes(body string) map[string]string {
	out := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			out[parts[2]] = parts[3]
		}
	}
	return out
}
