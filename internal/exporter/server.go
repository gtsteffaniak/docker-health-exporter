package exporter

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gtsteffaniak/docker-health-exporter/internal/prom"
)

// Server renders /metrics from the Docker API.
type Server struct {
	client *Client
	now    func() time.Time
}

// New builds a Server for the given Docker socket and timeout.
func NewServer(socket string, timeout time.Duration) *Server {
	return &Server{client: New(socket, timeout), now: time.Now}
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

	ok := make([]*prom.Row, 0, len(containers))
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

		// cAdvisor-compatible start/last-seen, from inspect StartedAt.
		var started int64
		if insp != nil {
			started = StartedUnix(insp.State.StartedAt)
		}
		// These are counters because they are monotonic per container
		// lifetime; dashboards use rate()/increase() on them.
		enc.Counter("container_start_time_seconds", "Unix time the container last started, 0 if never.", labels, float64(started))
		enc.Counter("container_last_seen", "Unix time of this scrape.", withoutState(labels), float64(s.now().Unix()))

		if insp != nil {
			enc.Counter("container_restart_count", "Docker's restart count for the container (resets when it is recreated).",
				map[string]string{"container": name, "name": name, "compose_project": project},
				float64(insp.RestartCount))
		}

		// Usage — only running containers have one-shot stats; stopped ones
		// simply carry no usage series for this scrape.
		if insp != nil && state == "running" {
			stats, err := s.client.Stats(r.Context(), c.ID)
			if err != nil {
				log.Printf("stats %s: %v", name, err)
			} else {
				emitUsage(enc, labels, stats)
			}
		}

		ok = append(ok, &prom.Row{Labels: labels})
	}
	_ = ok
}

// emitUsage writes the cAdvisor-compatible usage families for one container.
//
// Only the seven families actually consumed by the Grafana dashboards and
// the ContainerOOMKilled alert are emitted. cAdvisor's long tail (per-CPU,
// sched, hugetlb, throttling, process stats) is omitted by config on both
// ends, so there is nothing to drop.
//
// All usage families are counters (monotonic per container). Memory
// working-set is the exception: it is a gauge (current working set), matching
// cAdvisor's container_memory_working_set_bytes.
func emitUsage(enc *prom.Encoder, labels map[string]string, s *Stats) {
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
		"Working set memory.", usageLabels, float64(WorkingSetBytes(s)))
	enc.Gauge("container_memory_usage_bytes",
		"Total memory usage.", usageLabels, float64(s.MemoryStats.Usage))
	enc.Gauge("container_memory_max_usage_bytes",
		"Maximum memory usage.", usageLabels, float64(s.MemoryStats.MaxUsage))
	enc.Gauge("container_memory_limit_bytes",
		"Memory limit.", usageLabels, float64(s.MemoryStats.Limit))

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

	enc.Counter("container_oom_events_total",
		"OOM kills (best-effort, currently 0 — see OOMEvents).", usageLabels, float64(OOMEvents(s)))

	enc.Gauge("container_threads_max",
		"Maximum threads seen.", usageLabels, float64(s.PidsStats.Current))
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
