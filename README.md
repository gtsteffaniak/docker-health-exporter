# docker-health-exporter

A tiny, stdlib-only Prometheus exporter that reports **Docker container health
plus cAdvisor-compatible resource usage** from the Docker API over the unix
socket — with no housekeeper daemon, no cgroup parsing, and no third-party
dependencies.

It was extracted from the monitoring agent (`monitoring/docker-health-exporter`
in `gportal` / `proxy-service` / `monitoring`) when it grew the usage metrics
that used to come from cAdvisor. The health/restart metrics originated from
[`scrape_insert_influxdb`](https://github.com/gtsteffaniak/scrape_insert_influxdb)'s
`dockerStats` leg; the usage metrics replicate the subset of cAdvisor that the
stack actually consumes.

## What it exposes

### Health and lifecycle (original)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `docker_health_exporter_up` | gauge | — | 1 if the Docker API answered, 0 otherwise |
| `container_health_status` | gauge | `container`, `name`, `state`, `image`, `compose_project`, `compose_service` | 1 for the current state: `healthy`/`unhealthy`/`starting`/`running`/`stopped`/`unknown` |
| `container_restart_count` | counter | `container`, `name`, `compose_project` | Docker `RestartCount` (resets on recreate) |
| `container_started_time_seconds` | gauge | `container`, `name`, `compose_project` | Unix time the container last started, 0 if never |

`state` prefers the inspect `Health.Status` (exact); when there is no
healthcheck it falls back to the run state, so one alert rule covers both.

### Resource usage (cAdvisor-compatible subset)

The seven families actually consumed by Grafana dashboards and the
`ContainerOOMKilled` alert. Per-CPU, scheduler, hugetlb, and throttle series
cAdvisor ships by default are **not** emitted — nothing queries them, and
dropping them is the point.

| Metric | Type | Meaning |
|---|---|---|
| `container_cpu_usage_seconds_total` | counter | Seconds of CPU consumed |
| `container_memory_working_set_bytes` | gauge | Current working-set memory |
| `container_memory_usage_bytes` | gauge | Total memory usage |
| `container_memory_max_usage_bytes` | gauge | Peak memory usage |
| `container_memory_limit_bytes` | gauge | Memory limit |
| `container_network_receive_bytes_total` | counter | Total bytes received, summed over all interfaces |
| `container_network_transmit_bytes_total` | counter | Total bytes transmitted |
| `container_fs_reads_bytes_total` | counter | Total disk bytes read |
| `container_fs_writes_bytes_total` | counter | Total disk bytes written |
| `container_oom_events_total` | counter | OOM kills (best-effort — see below) |
| `container_threads_max` | gauge | Current thread count |
| `container_start_time_seconds` | counter | cAdvisor-named alias of started time |
| `container_last_seen` | counter | Unix time of this scrape |

Usage series are only emitted for **running** containers. Stopped containers
carry no usage for the scrape they are absent from.

### OOM caveat

Docker's `/stats` endpoint exposes no per-container OOM counter (the cgroup
pid is needed to read `memory.events`). `container_oom_events_total` is
therefore emitted as `0` so the alert expression stays valid instead of
erroring on a missing metric. A future revision can source real OOMs from
`/sys/fs/cgroup/.../docker-<id>.scope/memory.events` when the exporter runs
with the container's cgroup path.

## Usage

```sh
# Terminal/run directly (host user must be in the docker group, or root)
docker-health-exporter -listen :9415

# Docker: non-root (image default user is nobody)
GID=$(getent group docker | cut -d: -f3)
docker run -d --name docker-health-exporter \
  -p 127.0.0.1:9415:9415 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  --group-add "$GID" \
  gtstef/docker-health-exporter:latest
```

Scrape:

```sh
curl -s http://localhost:9415/metrics | head
# docker_health_exporter_up 1  → socket access OK
```

### Running without root

The image runs as **`nobody` (UID 65534)**. Do **not** set `user: root` in
Compose unless you accept full host impact if the exporter is compromised.

Access to `/var/run/docker.sock` is a **Unix file permission**, not a network
capability. `CAP_NET_*` and similar caps do **not** grant socket access.
`CAP_DAC_OVERRIDE` can bypass permissions but is broader than membership in the
`docker` group and is a poor tradeoff here.

**Recommended — supplemental `docker` group (Compose):**

```yaml
docker-health-exporter:
  image: gtstef/docker-health-exporter:latest
  restart: unless-stopped
  mem_limit: 64m
  group_add:
    - "${DOCKER_GID:?host docker group GID — getent group docker | cut -d: -f3}"
  ports:
    - 127.0.0.1:9415:9415
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock:ro
```

Set `DOCKER_GID` in `.env` to the **numeric** GID from the host (the group name
`docker` often does not exist inside the Alpine image).

**Equivalent — `docker run`:**

```sh
docker run ... --group-add "$(getent group docker | cut -d: -f3)" ...
```

**Alternative — primary group instead of `group_add`:**

```yaml
user: "65534:998" # nobody + host docker GID
```

**Alternative — Docker socket proxy:** run a filtered proxy container that
holds the socket and expose a narrow HTTP API; point the exporter at that URL
( `-docker-socket` is unix-only today — proxy setups usually front the socket
with a sidecar and are heavier than `group_add`).

**Security note:** members of the `docker` group can effectively act as root on
the host via the Engine API. This sidecar still needs that access; `group_add`
limits the blast radius to this one static binary instead of every process in
a root-namespaced container.

**Grafana Alloy** on the same hosts uses the image's `alloy` user (UID **473**)
with the same `DOCKER_GID`, plus `cap_add: [NET_RAW]` for ICMP probes — see the
monitoring repo README ("Alloy permissions").

### Compose (monitoring agent)

```yaml
docker-health-exporter:
  image: gtstef/docker-health-exporter:latest
  restart: unless-stopped
  mem_limit: 64m
  group_add:
    - "${DOCKER_GID}"
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
`job` to `docker-health`. The seven usage families are name-, type- and
unit-compatible, so dashboards and copy queries
(`rate(container_cpu_usage_seconds_total[$__rate_interval])`,
`container_memory_working_set_bytes`,
`increase(container_oom_events_total[15m])`) keep working without edits.
The one deliberate difference is cardinality: this exporter emits far fewer
series (no per-CPU, sched, hugetlb, or throttle breakdowns), which is the
point — on a 1 GiB VPS that is the difference between ~63 MiB (cAdvisor) and
~3 MiB (this exporter).

## Development

```sh
go vet ./...
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/dhe ./cmd/docker-health-exporter
docker build -t docker-health-exporter .
```

## Layout

```
cmd/docker-health-exporter/   main entrypoint (flags, HTTP wiring, shutdown)
internal/exporter/            Docker API client + /metrics renderer + tests
internal/prom/                minimal Prometheus text encoder + tests
```

All metrics paths are shared, race-free and allocation-light by design: one
list call, one inspect per container, one one-shot stats call per running
container per scrape — no background goroutine keeps state between scrapes.
