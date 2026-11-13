package exporter

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gtsteffaniak/docker-health-exporter/internal/prom"
)

// DefaultScrapeConcurrency is the maximum number of Docker inspect and stats
// requests allowed in flight at once during a single /metrics scrape.
const DefaultScrapeConcurrency = 8

// DefaultScrapeTimeout is the default deadline for Docker API work in one
// /metrics scrape. It is meant to sit at or under a typical collector
// scrape_timeout (the README Alloy example uses 10s).
const DefaultScrapeTimeout = 10 * time.Second

// Server renders /metrics from the Docker API.
type Server struct {
	client        DockerAPI
	events        EventsClient
	now           func() time.Time
	oomStore      *OOMStore
	concurrency   int
	scrapeTimeout time.Duration
}

// NewServer builds a Server for the given Docker socket and per-request
// timeout, using DefaultScrapeConcurrency and DefaultScrapeTimeout.
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
		client:        cli,
		events:        cli,
		now:           now,
		oomStore:      NewOOMStore(),
		concurrency:   DefaultScrapeConcurrency,
		scrapeTimeout: DefaultScrapeTimeout,
	}
}

// NewServerWithAPI builds a Server with an injectable DockerAPI (for tests).
func NewServerWithAPI(api DockerAPI, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		client:        api,
		now:           now,
		oomStore:      NewOOMStore(),
		concurrency:   DefaultScrapeConcurrency,
		scrapeTimeout: DefaultScrapeTimeout,
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
		client:        api,
		events:        events,
		now:           now,
		oomStore:      store,
		concurrency:   DefaultScrapeConcurrency,
		scrapeTimeout: DefaultScrapeTimeout,
	}
	return s
}

// SetScrapeLimits sets the maximum number of concurrent Docker inspect and
// stats requests per scrape and the overall Docker API deadline for one
// /metrics call. A concurrency less than 1 uses DefaultScrapeConcurrency. A
// timeout of 0 uses DefaultScrapeTimeout; a negative timeout disables the
// extra scrape deadline (the request context still applies).
func (s *Server) SetScrapeLimits(concurrency int, scrapeTimeout time.Duration) *Server {
	if s == nil {
		return s
	}
	s.concurrency = concurrency
	s.scrapeTimeout = scrapeTimeout
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
	ctx := r.Context()
	if d, ok := s.scrapeDeadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	containers, err := s.client.List(ctx)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprint(w, "# HELP docker_health_exporter_up Whether the Docker API answered.\n")
		_, _ = fmt.Fprint(w, "# TYPE docker_health_exporter_up gauge\n")
		_, _ = fmt.Fprint(w, "docker_health_exporter_up 0\n")
		log.Printf("list containers: %v", err)
		return
	}

	if s.oomStore != nil {
		liveIDs := make([]string, len(containers))
		for i := range containers {
			liveIDs[i] = containers[i].ID
		}
		s.oomStore.PruneAfterList(liveIDs)
	}

	results := s.fetchContainers(ctx, containers)

	enc := prom.NewEncoder(w)
	enc.Gauge("docker_health_exporter_up", "Whether the Docker API answered.", nil, 1)

	// Emit in Docker list order so concurrent collection cannot shuffle
	// the Prometheus text output.
	for _, res := range results {
		s.emitContainer(enc, res)
	}
}

// containerSnapshot is the inspect/stats result for one listed container.
type containerSnapshot struct {
	c       Container
	name    string
	project string
	service string
	insp    *Inspect
	stats   *Stats
}

func (s *Server) scrapeConcurrency() int {
	if s == nil || s.concurrency < 1 {
		return DefaultScrapeConcurrency
	}
	return s.concurrency
}

func (s *Server) scrapeDeadline() (time.Duration, bool) {
	if s == nil {
		return DefaultScrapeTimeout, true
	}
	if s.scrapeTimeout < 0 {
		return 0, false
	}
	if s.scrapeTimeout == 0 {
		return DefaultScrapeTimeout, true
	}
	return s.scrapeTimeout, true
}

// fetchContainers inspects (and, when running, stats) each named container
// with a bounded number of workers. Per-container API errors are logged and
// leave the corresponding fields nil; they do not fail the scrape. Results
// are returned in the same order as the input list, with unnamed containers
// omitted, matching the previous sequential loop.
func (s *Server) fetchContainers(ctx context.Context, containers []Container) []containerSnapshot {
	jobs := make([]containerSnapshot, 0, len(containers))
	for _, c := range containers {
		name := ContainerName(c.Names)
		if name == "" {
			continue
		}
		project, service := ComposeLabels(c, name)
		jobs = append(jobs, containerSnapshot{
			c:       c,
			name:    name,
			project: project,
			service: service,
		})
	}

	limit := s.scrapeConcurrency()
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			job := &jobs[i]
			insp, err := s.client.Inspect(ctx, job.c.ID)
			if err != nil {
				log.Printf("inspect %s: %v", job.name, err)
			} else {
				job.insp = insp
			}
			if !IsRunning(job.c) {
				return
			}
			stats, err := s.client.Stats(ctx, job.c.ID)
			if err != nil {
				log.Printf("stats %s: %v", job.name, err)
				return
			}
			job.stats = stats
		}(i)
	}
	wg.Wait()
	return jobs
}

func (s *Server) emitContainer(enc *prom.Encoder, res containerSnapshot) {
	c := res.c
	name := res.name
	project, service := res.project, res.service
	insp := res.insp
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
	if IsRunning(c) && res.stats != nil {
		emitUsage(enc, labels, res.stats, insp)
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
