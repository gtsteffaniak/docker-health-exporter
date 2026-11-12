package exporter

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gtsteffaniak/docker-health-exporter/internal/prom"
)

// Server renders /metrics from the Docker API.
type Server struct {
	client   DockerAPI
	events   EventsClient
	now      func() time.Time
	oomStore *OOMStore
}

// NewServer builds a Server for the given Docker socket and timeout.
func NewServer(socket string, timeout time.Duration) *Server {
	cli := New(socket, timeout)
	return NewServerWithClient(cli, time.Now)
}

// NewServerWithClient builds a Server with a real Client (enables OOM watcher).
func NewServerWithClient(cli *Client, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		client:   cli,
		events:   cli,
		now:      now,
		oomStore: NewOOMStore(),
	}
}

// NewServerWithAPI builds a Server with an injectable DockerAPI (for tests).
func NewServerWithAPI(api DockerAPI, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		client:   api,
		now:      now,
		oomStore: NewOOMStore(),
	}
}

// NewServerWithAPIAndOOM is like NewServerWithAPI but injects a shared OOM store
// (for tests that drive /events).
func NewServerWithAPIAndOOM(api DockerAPI, events EventsClient, store *OOMStore, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	if store == nil {
		store = NewOOMStore()
	}
	s := &Server{
		client:   api,
		events:   events,
		now:      now,
		oomStore: store,
	}
	return s
}

// RunBackground starts goroutines that must run for the process lifetime
// (Docker /events OOM watcher). Safe to skip in short-lived tests.
func (s *Server) RunBackground(ctx context.Context) {
	if s == nil || s.events == nil || s.oomStore == nil {
		return
	}
	go RunOOMWatcher(ctx, s.events, s.oomStore)
}

// New2 is the module-level constructor used by tests.
func New2(socket string, timeout time.Duration) *Client {
	return New(socket, timeout)
}

// ServeMetrics handles GET /metrics.
func (s *Server) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	containers, err := s.client.List(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprint(w, "# HELP docker_health_exporter_up Whether the Docker API answered.\n")
		_, _ = fmt.Fprint(w, "# TYPE docker_health_exporter_up gauge\n")
		_, _ = fmt.Fprint(w, "docker_health_exporter_up 0\n")
		log.Printf("list containers: %v", err)
		return
	}

	enc := prom.NewEncoder(w)
	enc.Gauge("docker_health_exporter_up", "Whether the Docker API answered.", nil, 1)

	for _, c := range containers {
		name := ContainerName(c.Names)
		if name == "" {
			continue
		}
		project, service := ComposeLabels(c, name)
		insp, err := s.client.Inspect(r.Context(), c.ID)
		if err != nil {
			log.Printf("inspect %s: %v", name, err)
		}
		state := HealthState(c, insp)
		longID := c.ID
		labels := map[string]string{
			"name":            name,
			"container":       name,
			"container_id":    longID,
			"image":           c.Image,
			"compose_project": project,
			"compose_service": service,
			"state":           state,
		}
		enc.Gauge("container_health_status", "Docker health of the container, 1 for the current state.", labels, 1)

		var started int64
		if insp != nil {
			started = StartedUnix(insp.State.StartedAt)
		}
		lifecycleLabels := map[string]string{
			"container":       name,
			"name":            name,
			"compose_project": project,
		}
		enc.Gauge("container_started_time_seconds",
			"Unix time the container last started, 0 if never.", lifecycleLabels, float64(started))
		enc.Gauge("container_start_time_seconds",
			"cAdvisor-named Unix start time (same value as container_started_time_seconds).",
			withoutState(labels), float64(started))
		enc.Gauge("container_last_seen", "Unix time of this scrape.", withoutState(labels), float64(s.now().Unix()))

		if insp != nil {
			enc.Counter("container_restart_count", "Docker's restart count for the container (resets when it is recreated).",
				lifecycleLabels, float64(insp.RestartCount))
		}

		var oom uint64
		if s.oomStore != nil {
			oom = s.oomStore.Count(c.ID)
		}
		emitOOMEvents(enc, labels, oom)

		// Usage for every Docker-running container, including healthchecked
		// ones (healthy/unhealthy/starting). Health state is not run state.
		if IsRunning(c) {
			stats, err := s.client.Stats(r.Context(), c.ID)
			if err != nil {
				log.Printf("stats %s: %v", name, err)
			} else {
				emitUsage(enc, labels, stats, insp)
			}
		}
	}
}

// emitOOMEvents writes container_oom_events_total for any container Docker lists,
// including stopped/exited, so increase() fires even when the container stays down.
func emitOOMEvents(enc *prom.Encoder, labels map[string]string, oomEvents uint64) {
	usageLabels := map[string]string{
		"name":            labels["name"],
		"container":       labels["container"],
		"container_id":    labels["container_id"],
		"image":           labels["image"],
		"compose_project": labels["compose_project"],
		"compose_service": labels["compose_service"],
	}
	enc.Counter("container_oom_events_total",
		"Cumulative Docker container oom events (Engine /events Action oom).",
		usageLabels, float64(oomEvents))
}

// emitUsage writes the cAdvisor-compatible usage families for one container.
//
// Only the families actually consumed by the Grafana dashboards and the
// ContainerOOMKilled alert are emitted. cAdvisor's long tail (per-CPU,
// sched, hugetlb, throttling, process stats) is omitted by config on both
// ends, so there is nothing to drop.
func emitUsage(enc *prom.Encoder, labels map[string]string, s *Stats, insp *Inspect) {
	usageLabels := map[string]string{
		"name":            labels["name"],
		"container":       labels["container"],
		"container_id":    labels["container_id"],
		"image":           labels["image"],
		"compose_project": labels["compose_project"],
		"compose_service": labels["compose_service"],
	}

	enc.Counter("container_cpu_usage_seconds_total",
		"Total CPU usage in seconds.", usageLabels, CPUSecs(s))

	enc.Gauge("container_memory_working_set_bytes",
		"Working set memory (usage minus inactive file cache).", usageLabels, float64(WorkingSetBytes(s)))
	enc.Gauge("container_memory_usage_bytes",
		"Total memory usage.", usageLabels, float64(s.MemoryStats.Usage))
	enc.Gauge("container_memory_max_usage_bytes",
		"Maximum memory usage.", usageLabels, float64(s.MemoryStats.MaxUsage))
	enc.Gauge("container_spec_memory_limit_bytes",
		"Configured memory limit from inspect HostConfig.Memory.", usageLabels,
		float64(SpecMemoryLimitBytes(insp, s)))

	rx, tx := NetworkIO(s)
	enc.Counter("container_network_receive_bytes_total",
		"Total network bytes received.", usageLabels, float64(rx))
	enc.Counter("container_network_transmit_bytes_total",
		"Total network bytes transmitted.", usageLabels, float64(tx))

	reads, writes := DiskIO(s)
	enc.Counter("container_fs_reads_bytes_total",
		"Total disk bytes read.", usageLabels, float64(reads))
	enc.Counter("container_fs_writes_bytes_total",
		"Total disk bytes written.", usageLabels, float64(writes))

	enc.Gauge("container_threads",
		"Current thread count.", usageLabels, float64(s.PidsStats.Current))
	enc.Gauge("container_threads_max",
		"Thread limit.", usageLabels, float64(s.PidsStats.Limit))
}

func withoutState(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k == "state" {
			continue
		}
		out[k] = v
	}
	return out
}
