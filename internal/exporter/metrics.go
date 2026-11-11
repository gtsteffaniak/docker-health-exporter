package exporter

import (
	"strings"
	"time"
)

// standalone is the compose_project of a container not started by Compose
// (`docker run`). All Alloy relabeling, Loki streams, and resolvers must use
// the same value so the Docker dashboard can group every container the same
// way.
const standalone = "standalone"

// ContainerName returns the first non-empty name, without the leading "/".
func ContainerName(names []string) string {
	for _, n := range names {
		n = strings.TrimPrefix(n, "/")
		if n != "" {
			return n
		}
	}
	return ""
}

// ComposeLabels returns the Compose project and service, falling back to
// "standalone" and the container name for a container Compose did not create.
func ComposeLabels(c Container, name string) (project, service string) {
	project, service = c.Labels["com.docker.compose.project"], c.Labels["com.docker.compose.service"]
	if project == "" {
		project = standalone
	}
	if service == "" {
		service = name
	}
	return project, service
}

// HealthState normalises a container into a single state word:
//
//	healthy | unhealthy | starting | running | stopped | unknown
//
// The Inspect Health block is preferred (exact, no string parsing). When
// there is no healthcheck the run state is used, so one alert rule
// (ContainerUnhealthy) covers healthcheck and plain containers alike.
//
// Legacy list Status parsing ("Up (healthy)", "(unhealthy)", ...) is kept
// as a fallback for Docker builds whose /json embeds health in Status but
// has no Health.Status in the inspect document.
func HealthState(c Container, in *Inspect) string {
	if in != nil && in.Health != nil {
		switch strings.ToLower(in.Health.Status) {
		case "healthy":
			return "healthy"
		case "unhealthy":
			return "unhealthy"
		case "starting":
			return "starting"
		}
	}
	return healthStateFromList(c)
}

func healthStateFromList(c Container) string {
	status := strings.ToLower(c.Status)
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	switch strings.ToLower(c.State) {
	case "running":
		return "running"
	case "exited", "dead":
		return "stopped"
	default:
		return "unknown"
	}
}

// StartedUnix parses Docker's RFC 3339 StartedAt. A never-started container
// reports "0001-01-01T00:00:00Z", which maps to 0 so dashboards can guard on > 0.
func StartedUnix(v string) int64 {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil || t.Year() < 2000 {
		return 0
	}
	return t.Unix()
}

// DiskIO splits cAdvisor-compatible fs read/write totals out of the Docker
// blkio io_service_bytes_recursive list, which is a slice of {op, value}.
func DiskIO(s *Stats) (reads, writes uint64) {
	if s == nil {
		return 0, 0
	}
	for _, e := range s.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			reads += e.Value
		case "write":
			writes += e.Value
		}
	}
	return reads, writes
}

// NetworkIO totals rx/tx across every interface in a stats document.
func NetworkIO(s *Stats) (rx, tx uint64) {
	if s == nil {
		return 0, 0
	}
	for _, n := range s.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	return rx, tx
}

// CPUSecs converts Docker's total CPU usage (nanoseconds) to the seconds
// counter cAdvisor exposes as container_cpu_usage_seconds_total.
func CPUSecs(s *Stats) float64 {
	if s == nil {
		return 0
	}
	return float64(s.CPUUsage.TotalUsage) / 1e9
}

// WorkingSetBytes is the cAdvisor container_memory_working_set_bytes
// analogue. The Docker `usage` field is already working-set (page cache
// included) when the kernel is on cgroup v2, which is what all supported
// hosts run; usage is capped at the limit/summary total when Docker
// reports it.
func WorkingSetBytes(s *Stats) uint64 {
	if s == nil {
		return 0
	}
	return s.MemoryStats.Usage
}

// OOMEvents is best-effort from Docker stats: Docker exposes no per-
// container OOM counter in /stats (the cgroup pid is needed to read
// memory.events, which the exporter does not read). Returning 0 keeps the
// series present so the alert expression stays valid rather than erroring
// on a missing metric. A future rev can source real OOMs from
// /sys/fs/cgroup/system.slice/docker-<id>.scope/memory.events when the
// exporter has the id or the cgroup path.
func OOMEvents(s *Stats) uint64 {
	_ = s
	return 0
}
