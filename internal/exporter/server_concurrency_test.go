package exporter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func manyContainerFixture(t *testing.T, n int, latency time.Duration) *fakeDocker {
	t.Helper()
	stats := defaultStatsJSON(t)
	started := "2026-11-10T22:00:00.123456789Z"
	inspect := json.RawMessage(`{"RestartCount":1,"State":{"StartedAt":"` + started + `","Health":{"Status":"healthy"}},"HostConfig":{"Memory":536870912}}`)
	f := &fakeDocker{
		list:    make([]Container, n),
		inspect: make(map[string]json.RawMessage, n),
		stats:   make(map[string]json.RawMessage, n),
		latency: latency,
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("c%02d", i)
		id := "id-" + name
		f.list[i] = Container{
			ID:     id,
			Names:  []string{"/" + name},
			Image:  "img:1",
			State:  "running",
			Status: "Up (healthy)",
			Labels: map[string]string{
				"com.docker.compose.project": "stack",
				"com.docker.compose.service": name,
			},
		}
		f.inspect[id] = inspect
		f.stats[id] = stats
	}
	return f
}

func scrapeWithLimits(t *testing.T, f *fakeDocker, concurrency int, scrapeTimeout time.Duration) string {
	t.Helper()
	ts := newFakeDockerServer(f)
	t.Cleanup(ts.Close)
	cli := NewHTTPClient(ts.URL, ts.Client())
	srv := NewServerWithAPI(cli, func() time.Time { return time.Unix(fixedScrapeUnix, 0) })
	srv.SetScrapeLimits(concurrency, scrapeTimeout)
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestServeMetricsConcurrencyBound(t *testing.T) {
	const (
		n           = 30
		limit       = 8
		latency     = 50 * time.Millisecond
		wantLatency = 50 * time.Millisecond
	)
	f := manyContainerFixture(t, n, latency)
	body := scrapeWithLimits(t, f, limit, 10*time.Second)

	if got := f.maxInflight.Load(); got > limit {
		t.Errorf("max in-flight inspect/stats = %d, want <= %d", got, limit)
	}
	if got := f.maxInflight.Load(); got < 2 {
		t.Errorf("max in-flight inspect/stats = %d, want some concurrency", got)
	}
	if got := f.inspectCalls.Load(); got != n {
		t.Errorf("inspect calls = %d, want %d", got, n)
	}
	if got := f.statsCalls.Load(); got != n {
		t.Errorf("stats calls = %d, want %d", got, n)
	}
	if !strings.Contains(body, "docker_health_exporter_up 1") {
		t.Fatal("expected up=1")
	}
	if !strings.Contains(body, `compose_project="stack"`) {
		t.Error("missing compose_project")
	}
	if !strings.Contains(body, `compose_service="c00"`) || !strings.Contains(body, `compose_service="c29"`) {
		t.Error("missing compose_service labels")
	}
	if !strings.Contains(body, `name="c00"`) || !strings.Contains(body, `name="c29"`) {
		t.Error("missing container samples")
	}
	_ = wantLatency
}

func TestServeMetricsParallelFasterThanSequential(t *testing.T) {
	const (
		n       = 30
		latency = 50 * time.Millisecond
	)

	seq := manyContainerFixture(t, n, latency)
	startSeq := time.Now()
	seqBody := scrapeWithLimits(t, seq, 1, 15*time.Second)
	seqDur := time.Since(startSeq)

	par := manyContainerFixture(t, n, latency)
	startPar := time.Now()
	parBody := scrapeWithLimits(t, par, 8, 15*time.Second)
	parDur := time.Since(startPar)

	line := fmt.Sprintf("30 containers, 50ms fake API latency (inspect+stats per container):\n  sequential (concurrency=1): %s (max in-flight %d)\n  parallel   (concurrency=8): %s (max in-flight %d)\n",
		seqDur.Round(time.Millisecond), seq.maxInflight.Load(),
		parDur.Round(time.Millisecond), par.maxInflight.Load())
	t.Log("\n" + line)

	if dir := os.Getenv("DHE_TIMING_OUT"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scrape-timing.txt"), []byte(line), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if seq.maxInflight.Load() > 1 {
		t.Errorf("sequential max in-flight = %d, want 1", seq.maxInflight.Load())
	}
	if par.maxInflight.Load() > 8 {
		t.Errorf("parallel max in-flight = %d, want <= 8", par.maxInflight.Load())
	}
	// Sequential: 30 * (50ms inspect + 50ms stats) = 3s. Allow some scheduling slack.
	if seqDur < 2*time.Second {
		t.Errorf("sequential scrape too fast: %s, want at least 2s", seqDur)
	}
	// Parallel: 4 waves * 100ms = 400ms typical. Stay well under half of sequential.
	if parDur > 1500*time.Millisecond {
		t.Errorf("parallel scrape too slow: %s, want < 1.5s", parDur)
	}
	if parDur*2 >= seqDur {
		t.Errorf("parallel %s was not substantially faster than sequential %s", parDur, seqDur)
	}
	if !strings.Contains(seqBody, "docker_health_exporter_up 1") || !strings.Contains(parBody, "docker_health_exporter_up 1") {
		t.Fatal("expected up=1 in both scrapes")
	}
	if seqBody != parBody {
		t.Error("sequential and parallel scrapes must emit identical metrics text")
	}
}

func TestServeMetricsConcurrentOutputDeterministic(t *testing.T) {
	f1 := manyContainerFixture(t, 15, 10*time.Millisecond)
	a := scrapeWithLimits(t, f1, 8, 5*time.Second)
	f2 := manyContainerFixture(t, 15, 10*time.Millisecond)
	b := scrapeWithLimits(t, f2, 8, 5*time.Second)
	if a != b {
		t.Fatal("repeated concurrent scrapes must be byte-identical")
	}

	// List order is preserved: c00 appears before c14 in the first health sample.
	i0 := strings.Index(a, `name="c00"`)
	iN := strings.Index(a, `name="c14"`)
	if i0 < 0 || iN < 0 || i0 > iN {
		t.Errorf("expected list-order emission, c00 at %d, c14 at %d", i0, iN)
	}
}

func TestServeMetricsPartialContainerFailure(t *testing.T) {
	stats := defaultStatsJSON(t)
	started := "2026-11-10T22:00:00.123456789Z"
	okInspect := json.RawMessage(`{"RestartCount":4,"State":{"StartedAt":"` + started + `","Health":{"Status":"healthy"}},"HostConfig":{"Memory":536870912}}`)
	f := &fakeDocker{
		list: []Container{
			{ID: "id-bad-inspect", Names: []string{"/bad_inspect"}, Image: "img:1", State: "running", Status: "Up",
				Labels: map[string]string{"com.docker.compose.project": "stack", "com.docker.compose.service": "bad_inspect"}},
			{ID: "id-bad-stats", Names: []string{"/bad_stats"}, Image: "img:2", State: "running", Status: "Up",
				Labels: map[string]string{"com.docker.compose.project": "stack", "com.docker.compose.service": "bad_stats"}},
			{ID: "id-ok", Names: []string{"/ok_svc"}, Image: "img:3", State: "running", Status: "Up",
				Labels: map[string]string{"com.docker.compose.project": "stack", "com.docker.compose.service": "ok_svc"}},
			{ID: "id-nameless", Names: []string{"/"}, Image: "img:4", State: "running", Status: "Up"},
		},
		inspect: map[string]json.RawMessage{
			"id-bad-stats": okInspect,
			"id-ok":        okInspect,
			"id-nameless":  okInspect,
		},
		stats: map[string]json.RawMessage{
			"id-bad-inspect": stats,
			"id-ok":          stats,
			"id-nameless":    stats,
		},
		inspectStatus: map[string]int{"id-bad-inspect": http.StatusInternalServerError},
		statsStatus:   map[string]int{"id-bad-stats": http.StatusInternalServerError},
	}

	body := scrapeWithLimits(t, f, 8, 5*time.Second)
	if !strings.Contains(body, "docker_health_exporter_up 1") {
		t.Fatal("a per-container failure must not fail the scrape")
	}
	// Inspect failed: fall back to list run state, no restart_count, but stats still collected.
	assertMetric(t, body, `container_health_status{.*name="bad_inspect".*state="running".*} 1`)
	assertNoMetricForContainer(t, body, "bad_inspect", "container_restart_count")
	assertMetric(t, body, `container_cpu_usage_seconds_total{.*name="bad_inspect".*} 3.5`)

	// Stats failed: inspect/health/restart present, no usage.
	assertMetric(t, body, `container_health_status{.*name="bad_stats".*state="healthy".*} 1`)
	assertMetric(t, body, `container_restart_count{.*name="bad_stats".*} 4`)
	assertNoMetricForContainer(t, body, "bad_stats", "container_cpu_usage_seconds_total")

	// Healthy container is complete, including compose labels.
	assertMetric(t, body, `container_health_status{.*compose_project="stack".*compose_service="ok_svc".*name="ok_svc".*state="healthy".*} 1`)
	assertMetric(t, body, `container_cpu_usage_seconds_total{.*name="ok_svc".*} 3.5`)

	if strings.Contains(body, "id-nameless") || strings.Contains(body, `name=""`) {
		t.Error("unnamed containers must still be skipped")
	}
}

func TestServeMetricsScrapeTimeout(t *testing.T) {
	f := manyContainerFixture(t, 20, 200*time.Millisecond)
	start := time.Now()
	body := scrapeWithLimits(t, f, 8, 150*time.Millisecond)
	dur := time.Since(start)
	if dur > time.Second {
		t.Errorf("scrape held for %s after 150ms deadline", dur)
	}
	if !strings.Contains(body, "docker_health_exporter_up 1") {
		t.Error("list succeeded, up should still be 1 when later calls time out")
	}
}

func TestSetScrapeLimitsDefaults(t *testing.T) {
	s := NewServerWithAPI(nil, time.Now)
	if s.scrapeConcurrency() != DefaultScrapeConcurrency {
		t.Errorf("default concurrency = %d, want %d", s.scrapeConcurrency(), DefaultScrapeConcurrency)
	}
	if d, ok := s.scrapeDeadline(); !ok || d != DefaultScrapeTimeout {
		t.Errorf("default deadline = %v ok=%v, want %s", d, ok, DefaultScrapeTimeout)
	}

	s.SetScrapeLimits(0, 0)
	if s.scrapeConcurrency() != DefaultScrapeConcurrency {
		t.Errorf("zero concurrency should default, got %d", s.scrapeConcurrency())
	}
	if d, ok := s.scrapeDeadline(); !ok || d != DefaultScrapeTimeout {
		t.Errorf("zero timeout should default, got %v ok=%v", d, ok)
	}

	s.SetScrapeLimits(3, -1)
	if s.scrapeConcurrency() != 3 {
		t.Errorf("concurrency = %d, want 3", s.scrapeConcurrency())
	}
	if _, ok := s.scrapeDeadline(); ok {
		t.Error("negative timeout should disable the extra deadline")
	}
}
