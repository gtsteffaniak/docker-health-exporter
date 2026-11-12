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
// Inspect State.Health is preferred (exact, no string parsing). When there is
// no healthcheck the run state is used, so one alert rule (ContainerUnhealthy)
// covers healthcheck and plain containers alike.
//
// Legacy list Status parsing ("Up (healthy)", "(unhealthy)", ...) is kept as a
// fallback for Docker builds whose /json embeds health in Status but omit
// State.Health in inspect.
func HealthState(c Container, in *Inspect) string {
	if in != nil && in.State.Health != nil {
		switch strings.ToLower(in.State.Health.Status) {
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

// IsRunning reports whether Docker's run state is "running" (distinct from
// healthcheck status healthy/unhealthy/starting).
func IsRunning(c Container) bool {
	return strings.EqualFold(c.State, "running")
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
	return float64(s.CPUStats.CPUUsage.TotalUsage) / 1e9
}

// inactiveFileBytes returns page-cache inactive file bytes from a stats doc.
// cgroup v1 sets memory_stats.stats.total_inactive_file; v2 sets
// memory_stats.stats.inactive_file. When both keys are present, v1 wins
// (including when total_inactive_file is 0).
func inactiveFileBytes(s *Stats) uint64 {
	if s == nil {
		return 0
	}
	st := s.MemoryStats.Stats
	if st.TotalInactiveFile != nil {
		return *st.TotalInactiveFile
	}
	if st.InactiveFile != nil {
		return *st.InactiveFile
	}
	return 0
}

// SpecMemoryLimitBytes is the configured limit for container_spec_memory_limit_bytes.
// cAdvisor reads HostConfig.Memory from inspect, not memory_stats.limit (which
// reflects host RAM when the container has no cap). When inspect is missing,
// memory_stats.limit is a best-effort fallback. HostConfig.Memory == 0 means
// unlimited; this exporter emits 0 (cAdvisor may report machine memory).
func SpecMemoryLimitBytes(insp *Inspect, stats *Stats) uint64 {
	if insp != nil {
		if insp.HostConfig.Memory > 0 {
			return uint64(insp.HostConfig.Memory)
		}
		return 0
	}
	if stats != nil {
		return stats.MemoryStats.Limit
	}
	return 0
}

// WorkingSetBytes matches cAdvisor container_memory_working_set_bytes:
// usage minus inactive file cache, floored at zero.
func WorkingSetBytes(s *Stats) uint64 {
	if s == nil {
		return 0
	}
	usage := s.MemoryStats.Usage
	inactive := inactiveFileBytes(s)
	if usage > inactive {
		return usage - inactive
	}
	return 0
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
