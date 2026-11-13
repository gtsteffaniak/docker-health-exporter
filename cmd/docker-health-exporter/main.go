// Command docker-health-exporter exposes Docker container health and
// cAdvisor-compatible resource usage as Prometheus metrics.
//
// A single scrape of /metrics lists containers via the Docker API over the
// unix socket, then inspects state/health and reads one-shot stats for each
// container with a bounded number of concurrent API calls. A background
// goroutine streams GET /events (container oom) over the
// same socket for container_oom_events_total. No daemon, no cgroup mounts, no
// third-party dependencies: stdlib only,
// so the static binary runs in a scratch-class image with the socket as its
// only host touchpoint.
//
// Health and restart metrics (docker-health-exporter):
//
//	docker_health_exporter_up, container_health_status{state},
//	container_restart_count, container_started_time_seconds
//
// cAdvisor-compatible usage metrics (job labels are added by the caller,
// e.g. Alloy's discovery.relabel — this binary never sets site/host):
//
//	container_cpu_usage_seconds_total, container_memory_working_set_bytes,
//	container_network_receive_bytes_total,
//	container_network_transmit_bytes_total, container_fs_reads_bytes_total,
//	container_fs_writes_bytes_total, container_oom_events_total (Docker oom events),
//	container_start_time_seconds, container_last_seen (gauges; uptime uses started_time)
//
// Usage:
//
//	docker-health-exporter [-listen :9415] [-docker-socket /var/run/docker.sock]
//	                      [-timeout 5s] [-scrape-concurrency 8] [-scrape-timeout 10s]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gtsteffaniak/docker-health-exporter/internal/exporter"
)

func main() {
	var (
		addr          = flag.String("listen", ":9415", "address to serve metrics on")
		socket        = flag.String("docker-socket", "/var/run/docker.sock", "Docker API socket")
		timeout       = flag.Duration("timeout", 5*time.Second, "Docker API request timeout")
		concurrency   = flag.Int("scrape-concurrency", envInt("SCRAPE_CONCURRENCY", exporter.DefaultScrapeConcurrency), "max concurrent Docker inspect and stats requests per scrape")
		scrapeTimeout = flag.Duration("scrape-timeout", envDuration("SCRAPE_TIMEOUT", exporter.DefaultScrapeTimeout), "maximum time a /metrics scrape may spend calling the Docker API")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := exporter.NewServer(*socket, *timeout)
	srv.SetScrapeLimits(*concurrency, *scrapeTimeout)
	srv.RunBackground(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		srv.ServeMetrics(w, r)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	_ = os.Stdout
	fmt.Println("docker-health-exporter listening on", *addr)
	log.Fatal(httpSrv.ListenAndServe())
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q, using %d", key, v, fallback)
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("invalid %s=%q, using %s", key, v, fallback)
		return fallback
	}
	return d
}
