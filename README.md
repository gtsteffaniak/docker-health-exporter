# docker-health-exporter

A tiny, stdlib-only Prometheus exporter that reports **Docker container health
plus cAdvisor-compatible resource usage** from the Docker API over the unix
socket — with no housekeeper daemon, no cgroup parsing, and no third-party
dependencies.

The health/restart metrics originated from
[`scrape_insert_influxdb`](https://github.com/gtsteffaniak/scrape_insert_influxdb)'s
`dockerStats`, the usage metrics replicate the subset of cAdvisor that the
stack actually consumes.

## What it exposes

### Health and lifecycle (original)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `docker_health_exporter_up` | gauge | — | 1 if the Docker API answered, 0 otherwise |
| `container_health_status` | gauge | `container`, `name`, `state`, `image`, `compose_project`, `compose_service` | 1 for the current state: `healthy`/`unhealthy`/`starting`/`running`/`stopped`/`unknown` |
| `container_restart_count` | counter | `container`, `name`, `compose_project` | Docker `RestartCount` (resets on recreate) |
| `container_started_time_seconds` | gauge | `container`, `name`, `compose_project` | Unix time the container last started, 0 if never |

`state` prefers inspect `State.Health.Status` (exact); when there is no
healthcheck it falls back to the run state, so one alert rule covers both.

### Resource usage (cAdvisor-compatible subset)

The families actually consumed by Grafana dashboards and the
`ContainerOOMKilled` alert. Per-CPU, scheduler, hugetlb, and throttle series
cAdvisor ships by default are **not** emitted — nothing queries them.

| Metric | Type | Meaning |
|---|---|---|
| `container_cpu_usage_seconds_total` | counter | Seconds of CPU consumed |
| `container_memory_working_set_bytes` | gauge | Usage minus inactive file cache (cAdvisor working set) |
| `container_memory_usage_bytes` | gauge | Total memory usage |
| `container_memory_max_usage_bytes` | gauge | Peak memory usage (often 0 on cgroup v2) |
| `container_spec_memory_limit_bytes` | gauge | `HostConfig.Memory` from inspect (0 = unlimited; cAdvisor may show host RAM) |
| `container_network_receive_bytes_total` | counter | Total bytes received, summed over all interfaces |
| `container_network_transmit_bytes_total` | counter | Total bytes transmitted |
| `container_fs_reads_bytes_total` | counter | Total disk bytes read, summed over blkio entries |
| `container_fs_writes_bytes_total` | counter | Total disk bytes written |
| `container_oom_events_total` | counter | Cumulative Docker container `oom` events (`GET /events`, see below) |
| `container_threads` | gauge | Current thread count |
| `container_threads_max` | gauge | Thread limit (`pids_stats.limit`) |
| `container_start_time_seconds` | gauge | cAdvisor-named alias of started time |
| `container_last_seen` | gauge | Unix time of this scrape |

Usage series (CPU, memory, network, disk, threads) are emitted for every
container in Docker **run state** `running`, including those reporting
`healthy` / `unhealthy` / `starting` from a healthcheck. Stopped containers
carry no usage for that scrape. **`container_oom_events_total` is emitted for
every container Docker lists** (running or stopped) so
`increase(container_oom_events_total[15m])` fires even when the container stays
exited after an OOM.

### Deliberate differences from cAdvisor

| Topic | This exporter |
|---|---|
| `container_oom_events_total` | Counts Engine **EventOOM** (`Action: oom` on `GET /events`), not cAdvisor's `/dev/kmsg` parse (no privileged device). In-memory counter resets on exporter restart; Prometheus `increase()` treats resets, but if the counter returns to the same value before the next scrape (e.g. 1 → restart → 1), no increase is recorded in that interval. Streams live while the exporter runs. On reconnect, `since=` backfill uses the last processed event timestamp (inclusive) and is best-effort (daemon keeps ~256 events total). OOMs during a stream outage before any event has been recorded are not backfilled. After each successful container list, per-container OOM state for IDs absent from two consecutive lists is dropped so compose recreate churn does not grow memory without bound (removed containers lose their counter series, which is normal for Prometheus counters). Typically container memory-limit OOMs the daemon attributes; host-wide kills stay on `node_vmstat_oom_kill`. |
| Network / disk | Summed across all interfaces / devices — no `interface` or `device` labels |
| Per-CPU / sched / hugetlb | Not emitted — dashboards do not query them (`container_cpu_usage_seconds_total` is emitted) |
| `machine_*`, `cadvisor_version_info` | Not emitted |
| Unlimited memory cap | `container_spec_memory_limit_bytes` is `0`; cgroup `memory_stats.limit` is not used when inspect says unlimited |
| Cardinality | Far smaller than full cAdvisor |

## Usage

```sh
# Terminal/run directly
docker-health-exporter -listen :9415

# Docker (socket is the only mount)
docker run -d --name docker-health-exporter \
  -p 127.0.0.1:9415:9415 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  gtsteffaniak/docker-health-exporter:latest
```

Scrape:

```sh
curl -s http://localhost:9415/metrics | head
```

Health check: `GET /healthz` returns 200.

### Compose (monitoring agent)

```yaml
docker-health-exporter:
  image: gtsteffaniak/docker-health-exporter:latest
  restart: unless-stopped
  user: root
  mem_limit: 64m
  ports:
    - 127.0.0.1:9415:9415
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock:ro
```

In Alloy (or any collector), scrape it and relabel, exactly as you would a
cAdvisor target:

```alloy
prometheus.scrape "docker_health" {
  targets = [
    {
      __address__ = "127.0.0.1:9415",
      site        = sys.env("MONITORING_SITE"),
      host        = sys.env("MONITORING_HOST"),
      instance    = sys.env("MONITORING_HOST"),
      job         = "docker-health",
    },
  ]
  forward_to      = [prometheus.remote_write.central.receiver]
  scrape_interval = "30s"
  scrape_timeout  = "10s"
}
```

**Label note:** `name`, `container`, `compose_project`, and `compose_service`
are set by the exporter. `site`, `host`, `instance`, and `job` are added by
the collector's relabeling. The exporter deliberately never sets them, which
is what makes it drop-in for Prometheus, Alloy, VictoriaMetrics, Thanos, or
any other standard Prometheus-compatible stack.

## Migration from cAdvisor

Replace the cAdvisor sidecar with this exporter and change the Alloy
`job="cadvisor"` scrape target to `job="docker-health"` — or relabel
`job` to `docker-health`. The usage families are name-, type- and
unit-compatible, so dashboards and copy queries
(`rate(container_cpu_usage_seconds_total[$__rate_interval])`,
`container_memory_working_set_bytes`,
`time() - container_started_time_seconds > 0`) keep working without edits.
The one deliberate difference is cardinality: this exporter emits far fewer
series (no per-CPU, sched, hugetlb, or throttle breakdowns), which is the
point — on a 1 GiB VPS that is the difference between ~63 MiB (cAdvisor) and
~3 MiB (this exporter).

## Development

```sh
gofmt -w .
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/dhe ./cmd/docker-health-exporter
docker build -t docker-health-exporter .
```

Golden regression output: see `internal/exporter/testdata/README.md`.

## Layout

```
cmd/docker-health-exporter/   main entrypoint (flags, HTTP wiring, shutdown)
internal/exporter/            Docker API client + /metrics renderer + tests
internal/prom/                minimal Prometheus text encoder + tests
```

All metrics paths are shared, race-free and allocation-light by design: one
list call, one inspect per container, one one-shot stats call per running
container per scrape — no background goroutine keeps state between scrapes.
