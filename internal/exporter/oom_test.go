package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOOMStoreIncrement(t *testing.T) {
	s := NewOOMStore()
	s.Increment("id-a")
	s.Increment("id-a")
	if got := s.Count("id-a"); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	if got := s.Count("missing"); got != 0 {
		t.Fatalf("missing = %d, want 0", got)
	}
}

func TestOOMEventIncrementsMetric(t *testing.T) {
	f := regressionFixture(t)
	f.events = make(chan []byte, 4)
	srv, _, _ := newTestServerWithEvents(t, f, time.Unix(fixedScrapeUnix, 0))

	ev := dockerOOMEvent("id-healthy", "healthy_svc")
	f.events <- marshalEventLine(t, ev)
	waitForOOMCount(t, srv, "id-healthy", 1)

	body := scrapeServer(t, srv)
	assertMetric(t, body, `container_oom_events_total{.*compose_project="stack".*name="healthy_svc".*} 1`)
}

func TestTwoOOMEventsCumulative(t *testing.T) {
	f := regressionFixture(t)
	f.events = make(chan []byte, 4)
	srv, _, _ := newTestServerWithEvents(t, f, time.Unix(fixedScrapeUnix, 0))

	f.events <- marshalEventLine(t, dockerOOMEvent("id-healthy", "healthy_svc"))
	waitForOOMCount(t, srv, "id-healthy", 1)
	body := scrapeServer(t, srv)
	assertMetric(t, body, `container_oom_events_total{.*name="healthy_svc".*} 1`)

	f.events <- marshalEventLine(t, dockerOOMEvent("id-healthy", "healthy_svc"))
	waitForOOMCount(t, srv, "id-healthy", 2)
	body = scrapeServer(t, srv)
	assertMetric(t, body, `container_oom_events_total{.*name="healthy_svc".*} 2`)
}

func TestNonOOMEventsIgnored(t *testing.T) {
	f := regressionFixture(t)
	f.events = make(chan []byte, 8)
	srv, _, _ := newTestServerWithEvents(t, f, time.Unix(fixedScrapeUnix, 0))

	for _, action := range []string{"start", "die", "health_status", "restart"} {
		f.events <- marshalEventLine(t, map[string]any{
			"Type": "container", "Action": action,
			"Actor": map[string]any{"ID": "id-healthy"},
			"time":  time.Now().Unix(),
		})
	}
	time.Sleep(50 * time.Millisecond)
	if got := srv.oomStore.Count("id-healthy"); got != 0 {
		t.Fatalf("non-oom incremented store: %d", got)
	}
}

func TestStartAfterOOMDoesNotIncrement(t *testing.T) {
	f := regressionFixture(t)
	f.events = make(chan []byte, 8)
	srv, _, _ := newTestServerWithEvents(t, f, time.Unix(fixedScrapeUnix, 0))

	f.events <- marshalEventLine(t, dockerOOMEvent("id-plain", "plain_svc"))
	waitForOOMCount(t, srv, "id-plain", 1)

	f.events <- marshalEventLine(t, map[string]any{
		"Type": "container", "Action": "start",
		"Actor": map[string]any{"ID": "id-plain"},
		"time":  time.Now().Unix(),
	})
	time.Sleep(50 * time.Millisecond)
	if got := srv.oomStore.Count("id-plain"); got != 1 {
		t.Fatalf("count after start = %d, want 1", got)
	}
}

func TestMalformedEventIgnored(t *testing.T) {
	f := regressionFixture(t)
	f.events = make(chan []byte, 4)
	srv, _, _ := newTestServerWithEvents(t, f, time.Unix(fixedScrapeUnix, 0))

	f.events <- []byte("not json\n")
	f.events <- marshalEventLine(t, map[string]any{"Type": "network", "Action": "oom"})
	f.events <- marshalEventLine(t, map[string]any{"Type": "container", "Action": "oom"})
	time.Sleep(50 * time.Millisecond)
	if got := srv.oomStore.Count("id-healthy"); got != 0 {
		t.Fatalf("unexpected increment: %d", got)
	}
}

func TestEventsPathFiltersOOMOnly(t *testing.T) {
	path := eventsPath(time.Time{})
	if !strings.Contains(path, "filters=") {
		t.Fatal("missing filters")
	}
	if !strings.Contains(path, "oom") || !strings.Contains(path, "container") {
		t.Fatalf("unexpected path: %s", path)
	}
}

func dockerOOMEvent(id, name string) map[string]any {
	return map[string]any{
		"Type":   "container",
		"Action": "oom",
		"Actor": map[string]any{
			"ID": id,
			"Attributes": map[string]string{
				"name": name,
			},
		},
		"time":     time.Now().Unix(),
		"timeNano": time.Now().UnixNano(),
	}
}

func waitForOOMCount(t *testing.T, srv *Server, id string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.oomStore.Count(id) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("oom count for %s did not reach %d (got %d)", id, want, srv.oomStore.Count(id))
}

func scrapeServer(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
