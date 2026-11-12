package exporter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDocker serves list/inspect/stats fixtures keyed by container ID.
type fakeDocker struct {
	list    []Container
	inspect map[string]json.RawMessage
	stats   map[string]json.RawMessage
}

func (f *fakeDocker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json" && r.Method == http.MethodGet:
			if r.URL.Query().Get("all") != "1" {
				http.Error(w, "expected all=1", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.list)
		case strings.HasSuffix(r.URL.Path, "/json") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/json"), "/containers/")
			raw, ok := f.inspect[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		case strings.Contains(r.URL.Path, "/stats") && r.Method == http.MethodGet:
			q := r.URL.Query()
			if q.Get("stream") != "false" || q.Get("one-shot") != "true" {
				http.Error(w, "expected stream=false&one-shot=true", http.StatusBadRequest)
				return
			}
			path := strings.TrimPrefix(r.URL.Path, "/containers/")
			id, _, _ := strings.Cut(path, "/stats")
			raw, ok := f.stats[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	})
}

func newFakeDockerServer(f *fakeDocker) *httptest.Server {
	return httptest.NewServer(f.handler())
}

func newTestServer(t *testing.T, f *fakeDocker, now time.Time) *Server {
	t.Helper()
	ts := newFakeDockerServer(f)
	t.Cleanup(ts.Close)
	cli := NewHTTPClient(ts.URL, ts.Client())
	return NewServerWithAPI(cli, func() time.Time { return now })
}

const fixedScrapeUnix = int64(1700000000)

func defaultStatsJSON(t *testing.T) json.RawMessage {
	t.Helper()
	return readTestdata(t, "docker_stats_sample.json")
}

func regressionFixture(t *testing.T) *fakeDocker {
	t.Helper()
	stats := defaultStatsJSON(t)
	started := "2026-11-10T22:00:00.123456789Z"
	mkInspect := func(healthStatus string, restarts int, hasHealth bool) json.RawMessage {
		var h string
		if hasHealth {
			h = `,"Health":{"Status":"` + healthStatus + `"}`
		}
		return json.RawMessage(`{"RestartCount":` + itoa(restarts) + `,"State":{"StartedAt":"` + started + `"` + h + `},"HostConfig":{"Memory":536870912}}`)
	}
	return &fakeDocker{
		list: []Container{
			{ID: "id-healthy", Names: []string{"/healthy_svc"}, Image: "img:1", State: "running", Status: "Up 2 hours (healthy)",
				Labels: map[string]string{"com.docker.compose.project": "stack", "com.docker.compose.service": "healthy_svc"}},
			{ID: "id-plain", Names: []string{"/plain_svc"}, Image: "img:2", State: "running", Status: "Up 1 hour"},
			{ID: "id-sick", Names: []string{"/sick_svc"}, Image: "img:3", State: "running", Status: "Up (unhealthy)"},
			{ID: "id-boot", Names: []string{"/boot_svc"}, Image: "img:4", State: "running", Status: "Up (health: starting)"},
			{ID: "id-dead", Names: []string{"/dead_svc"}, Image: "img:5", State: "exited", Status: "Exited (0)"},
			{ID: "id-compose", Names: []string{"/compose_web"}, Image: "img:6", State: "running", Status: "Up",
				Labels: map[string]string{"com.docker.compose.project": "myproj", "com.docker.compose.service": "web"}},
			{ID: "id-standalone", Names: []string{"/portainer"}, Image: "img:7", State: "running", Status: "Up"},
		},
		inspect: map[string]json.RawMessage{
			"id-healthy":    mkInspect("healthy", 1, true),
			"id-plain":      mkInspect("", 0, false),
			"id-sick":       mkInspect("unhealthy", 3, true),
			"id-boot":       mkInspect("starting", 0, true),
			"id-dead":       mkInspect("", 0, false),
			"id-compose":    mkInspect("", 0, false),
			"id-standalone": mkInspect("", 2, false),
		},
		stats: map[string]json.RawMessage{
			"id-healthy":    stats,
			"id-plain":      stats,
			"id-sick":       stats,
			"id-boot":       stats,
			"id-compose":    stats,
			"id-standalone": stats,
		},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
