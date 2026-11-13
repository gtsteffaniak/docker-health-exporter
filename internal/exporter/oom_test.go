package exporter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOOMStoreRecordOOM(t *testing.T) {
	s := NewOOMStore()
	s.RecordOOM("id-a", 100)
	s.RecordOOM("id-a", 200)
	if got := s.Count("id-a"); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	s.RecordOOM("id-a", 100)
	if got := s.Count("id-a"); got != 2 {
		t.Fatalf("dedupe changed count = %d, want 2", got)
	}
	if got := s.Count("missing"); got != 0 {
		t.Fatalf("missing = %d, want 0", got)
	}
}

func TestOOMStoreHighWaterMarkReplay(t *testing.T) {
	s := NewOOMStore()
	const tn = int64(1700000000123456789)
	s.RecordOOM("id", tn)
	s.RecordOOM("id", tn)
	if got := s.Count("id"); got != 1 {
		t.Fatalf("same TimeNano twice: count = %d, want 1", got)
	}
	s.RecordOOM("id", tn-1)
	if got := s.Count("id"); got != 1 {
		t.Fatalf("older TimeNano replay: count = %d, want 1", got)
	}
	s.RecordOOM("id", tn+1)
	if got := s.Count("id"); got != 2 {
		t.Fatalf("newer TimeNano: count = %d, want 2", got)
	}
}

func TestOOMStorePruneRemovesAfterTwoAbsentLists(t *testing.T) {
	s := NewOOMStore()
	s.RecordOOM("gone", 1)
	s.RecordOOM("live", 2)
	s.PruneAfterList([]string{"live"})
	if got := s.Count("gone"); got != 1 {
		t.Fatalf("first absent: gone count = %d, want 1", got)
	}
	s.PruneAfterList([]string{"live"})
	if got := s.Count("gone"); got != 0 {
		t.Fatalf("second absent: gone count = %d, want 0", got)
	}
	if got := s.Count("live"); got != 1 {
		t.Fatalf("live count = %d, want 1", got)
	}
}

func TestOOMStorePruneClearsPendingWhenIDReturns(t *testing.T) {
	s := NewOOMStore()
	s.RecordOOM("id", 1)
	s.PruneAfterList(nil)
	s.PruneAfterList([]string{"id"})
	if got := s.Count("id"); got != 1 {
		t.Fatalf("count after reappear = %d, want 1", got)
	}
	s.PruneAfterList([]string{"id"})
	if got := s.Count("id"); got != 1 {
		t.Fatalf("stable live count = %d, want 1", got)
	}
}

func TestOOMStoreBoundedAcrossRecreates(t *testing.T) {
	s := NewOOMStore()
	const cycles = 50
	var prev string
	for i := 0; i < cycles; i++ {
		live := "id-" + strconv.Itoa(i)
		s.RecordOOM(live, int64(i+1))
		s.PruneAfterList([]string{live})
		s.PruneAfterList([]string{live})
		if prev != "" {
			if got := s.Count(prev); got != 0 {
				t.Fatalf("stale %s count = %d after recreate", prev, got)
			}
		}
		prev = live
	}
	s.mu.RLock()
	n := len(s.counts)
	s.mu.RUnlock()
	if n != 1 {
		t.Fatalf("store has %d entries after %d recreates, want 1", n, cycles)
	}
	if got := s.Count(prev); got != 1 {
		t.Fatalf("live %s count = %d, want 1", prev, got)
	}
}

func TestServeMetricsListErrorDoesNotPruneOOM(t *testing.T) {
	store := NewOOMStore()
	store.RecordOOM("stale", 1)
	api := &stubDockerAPI{
		listErr: fmt.Errorf("docker down"),
	}
	srv := NewServerWithAPIAndOOM(api, nil, store, time.Now)
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "docker_health_exporter_up 0") {
		t.Fatal("expected list failure response")
	}
	if got := store.Count("stale"); got != 1 {
		t.Fatalf("prune on list error: count = %d, want 1", got)
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

	f.events <- marshalEventLine(t, dockerOOMEventAt("id-healthy", "healthy_svc", 1))
	waitForOOMCount(t, srv, "id-healthy", 1)
	body := scrapeServer(t, srv)
	assertMetric(t, body, `container_oom_events_total{.*name="healthy_svc".*} 1`)

	f.events <- marshalEventLine(t, dockerOOMEventAt("id-healthy", "healthy_svc", 2))
	waitForOOMCount(t, srv, "id-healthy", 2)
	body = scrapeServer(t, srv)
	assertMetric(t, body, `container_oom_events_total{.*name="healthy_svc".*} 2`)
}

func TestOOMEventOnStoppedContainerScraped(t *testing.T) {
	f := regressionFixture(t)
	f.events = make(chan []byte, 4)
	srv, _, _ := newTestServerWithEvents(t, f, time.Unix(fixedScrapeUnix, 0))

	f.events <- marshalEventLine(t, dockerOOMEventAt("id-dead", "dead_svc", 99))
	waitForOOMCount(t, srv, "id-dead", 1)

	body := scrapeServer(t, srv)
	assertMetric(t, body, `container_oom_events_total{.*compose_service="dead_svc".*name="dead_svc".*} 1`)
	assertNoMetricForContainer(t, body, "dead_svc", "container_cpu_usage_seconds_total")
}

func TestReconnectReplayDoesNotDoubleCount(t *testing.T) {
	const tn = int64(1700000000123456789)
	line := []byte(`{"Type":"container","Action":"oom","Actor":{"ID":"id-replay"},"timeNano":` +
		strconv.FormatInt(tn, 10) + `}` + "\n")
	client := &replayEventsClient{streams: [][]byte{line, line}}
	store := NewOOMStore()
	var lastNano atomic.Int64

	ctx := context.Background()
	if err := watchOOMStream(ctx, client, store, time.Time{}, &lastNano); err != nil && err != io.EOF {
		t.Fatalf("first stream: %v", err)
	}
	if got := store.Count("id-replay"); got != 1 {
		t.Fatalf("after first stream count = %d", got)
	}
	since := time.Unix(0, lastNano.Load())
	if err := watchOOMStream(ctx, client, store, since, &lastNano); err != nil && err != io.EOF {
		t.Fatalf("reconnect stream: %v", err)
	}
	if got := store.Count("id-replay"); got != 1 {
		t.Fatalf("after replay count = %d, want 1", got)
	}
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

func TestReconnectInclusiveSinceDistinctContainerSameNano(t *testing.T) {
	const tn = int64(1700000000123456789)
	mk := func(id string) []byte {
		return []byte(`{"Type":"container","Action":"oom","Actor":{"ID":"` + id +
			`"},"timeNano":` + strconv.FormatInt(tn, 10) + `}` + "\n")
	}
	client := &replayEventsClient{streams: [][]byte{
		mk("id-a"),
		append(append([]byte(nil), mk("id-a")...), mk("id-b")...),
	}}
	store := NewOOMStore()
	var lastNano atomic.Int64
	ctx := context.Background()

	if err := watchOOMStream(ctx, client, store, time.Time{}, &lastNano); err != nil && err != io.EOF {
		t.Fatalf("first stream: %v", err)
	}
	if got := store.Count("id-a"); got != 1 {
		t.Fatalf("after first stream id-a = %d", got)
	}
	since := time.Unix(0, lastNano.Load())
	if err := watchOOMStream(ctx, client, store, since, &lastNano); err != nil && err != io.EOF {
		t.Fatalf("reconnect: %v", err)
	}
	if got := client.sinceAt; len(got) != 1 || got[0] != tn {
		t.Fatalf("reconnect since = %v, want [%d]", got, tn)
	}
	if got := store.Count("id-a"); got != 1 {
		t.Fatalf("id-a after replay = %d, want 1", got)
	}
	if got := store.Count("id-b"); got != 1 {
		t.Fatalf("id-b after inclusive reconnect = %d, want 1", got)
	}
}

func TestTwoOOMAtSameSecondWithoutTimeNano(t *testing.T) {
	store := NewOOMStore()
	line := func() []byte {
		return []byte(`{"Type":"container","Action":"oom","Actor":{"ID":"id-1"},"time":1700000000}` + "\n")
	}
	var lastNano atomic.Int64
	ctx := context.Background()
	body := strings.NewReader(string(line()) + string(line()))
	client := &staticEventsClient{body: body}
	if err := watchOOMStream(ctx, client, store, time.Time{}, &lastNano); err != nil && err != io.EOF {
		t.Fatalf("watch: %v", err)
	}
	if got := store.Count("id-1"); got != 2 {
		t.Fatalf("count = %d, want 2 (no TimeNano dedupe)", got)
	}
}

func TestEventsPathSinceNumeric(t *testing.T) {
	since := time.Unix(1700000000, 123456789)
	path := eventsPath(since)
	if strings.Contains(path, "RFC") || strings.Contains(path, "T17") {
		t.Fatalf("since must not be RFC3339: %s", path)
	}
	want := "since=1700000000.123456789"
	if !strings.Contains(path, want) {
		t.Fatalf("since param = %q, want substring %q", path, want)
	}
	pathNoFrac := eventsPath(time.Unix(42, 0))
	if !strings.Contains(pathNoFrac, "since=42") {
		t.Fatalf("integer since: %s", pathNoFrac)
	}
}

func dockerOOMEvent(id, name string) map[string]any {
	return dockerOOMEventAt(id, name, time.Now().UnixNano())
}

func dockerOOMEventAt(id, name string, timeNano int64) map[string]any {
	return map[string]any{
		"Type":   "container",
		"Action": "oom",
		"Actor": map[string]any{
			"ID": id,
			"Attributes": map[string]string{
				"name": name,
			},
		},
		"time":     timeNano / 1_000_000_000,
		"timeNano": timeNano,
	}
}

type replayEventsClient struct {
	streams [][]byte
	sinceAt []int64
	calls   atomic.Int32
}

func (r *replayEventsClient) Events(_ context.Context, since time.Time) (io.ReadCloser, error) {
	if !since.IsZero() {
		r.sinceAt = append(r.sinceAt, since.UnixNano())
	}
	i := int(r.calls.Add(1)) - 1
	if i >= len(r.streams) {
		i = len(r.streams) - 1
	}
	return io.NopCloser(bytes.NewReader(r.streams[i])), nil
}

type staticEventsClient struct {
	body io.Reader
}

func (s *staticEventsClient) Events(_ context.Context, _ time.Time) (io.ReadCloser, error) {
	return io.NopCloser(s.body), nil
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

type stubDockerAPI struct {
	listErr error
}

func (s *stubDockerAPI) List(context.Context) ([]Container, error) {
	return nil, s.listErr
}

func (s *stubDockerAPI) Inspect(context.Context, string) (*Inspect, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubDockerAPI) Stats(context.Context, string) (*Stats, error) {
	return nil, fmt.Errorf("not implemented")
}

func scrapeServer(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
