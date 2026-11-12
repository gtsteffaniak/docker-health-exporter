package exporter

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestServeMetricsRegression(t *testing.T) {
	srv := newTestServer(t, regressionFixture(t), time.Unix(fixedScrapeUnix, 0))
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "docker_health_exporter_up 1") {
		t.Fatal("missing exporter up")
	}

	// Healthy running container: health + usage (CPU non-zero in seconds).
	assertMetric(t, body, `container_health_status{.*state="healthy".*} 1`)
	assertMetric(t, body, `container_cpu_usage_seconds_total{.*name="healthy_svc".*} 3.5`)
	assertNoMetricForContainer(t, body, "dead_svc", "container_cpu_usage_seconds_total")

	for _, tc := range []struct {
		name, state string
		wantUsage   bool
	}{
		{"healthy_svc", "healthy", true},
		{"plain_svc", "running", true},
		{"sick_svc", "unhealthy", true},
		{"boot_svc", "starting", true},
		{"dead_svc", "stopped", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertMetric(t, body, `container_health_status{.*name="`+tc.name+`".*state="`+tc.state+`".*} 1`)
			pat := `container_cpu_usage_seconds_total{.*name="` + tc.name + `"`
			if tc.wantUsage {
				assertMetric(t, body, pat)
			} else if regexp.MustCompile(pat).MatchString(body) {
				t.Errorf("exited container must not emit usage: %s", tc.name)
			}
		})
	}

	assertMetric(t, body, `container_started_time_seconds{.*name="healthy_svc".*} 1.794348e\+09`)
	assertType(t, body, "container_started_time_seconds", "gauge")
	assertType(t, body, "container_start_time_seconds", "gauge")
	assertType(t, body, "container_last_seen", "gauge")
	assertType(t, body, "container_cpu_usage_seconds_total", "counter")
	assertType(t, body, "container_restart_count", "counter")
	assertType(t, body, "container_memory_working_set_bytes", "gauge")
	assertType(t, body, "container_spec_memory_limit_bytes", "gauge")

	assertMetric(t, body, `container_restart_count{.*compose_project="stack".*name="healthy_svc".*} 1`)
	assertMetric(t, body, `container_restart_count{.*compose_project="standalone".*name="portainer".*} 2`)

	// Working set = usage - inactive (fixture: 104857600 - 20971520 = 83886080)
	assertMetric(t, body, `container_memory_working_set_bytes{.*name="healthy_svc".*} 8.388608e\+07`)

	assertMetric(t, body, `container_oom_events_total{.*name="healthy_svc".*} 0`)
	assertMetric(t, body, `container_threads{.*name="healthy_svc".*} 12`)
	assertMetric(t, body, `container_threads_max{.*name="healthy_svc".*} 4096`)

	// Network/disk sums
	assertMetric(t, body, `container_network_receive_bytes_total{.*name="healthy_svc".*} 1300`)
	assertMetric(t, body, `container_fs_reads_bytes_total{.*name="healthy_svc".*} 5120`)
}

func TestServeMetricsComposeLabels(t *testing.T) {
	body := scrapeMetrics(t, regressionFixture(t))
	if !strings.Contains(body, `compose_project="myproj"`) || !strings.Contains(body, `compose_service="web"`) {
		t.Error("compose labels missing for compose_web")
	}
	if !strings.Contains(body, `name="portainer"`) || !strings.Contains(body, `compose_project="standalone"`) {
		t.Error("standalone docker-run labels missing")
	}
}

func TestServeMetricsDockerAPIFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	srv := NewServerWithAPI(NewHTTPClient(ts.URL, ts.Client()), time.Now)
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "docker_health_exporter_up 0") {
		t.Errorf("expected up=0 on list failure")
	}
}

func scrapeMetrics(t *testing.T, f *fakeDocker) string {
	t.Helper()
	srv := newTestServer(t, f, time.Unix(fixedScrapeUnix, 0))
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func assertMetric(t *testing.T, body, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if !re.MatchString(body) {
		t.Errorf("pattern %q not found in output", pattern)
	}
}

func assertNoMetricForContainer(t *testing.T, body, container, family string) {
	t.Helper()
	re := regexp.MustCompile(family + `{.*name="` + container + `"`)
	if re.MatchString(body) {
		t.Errorf("container %s should not have %s", container, family)
	}
}

func assertType(t *testing.T, body, name, typ string) {
	t.Helper()
	want := "# TYPE " + name + " " + typ
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if sc.Text() == want {
			return
		}
	}
	t.Errorf("missing %q", want)
}
