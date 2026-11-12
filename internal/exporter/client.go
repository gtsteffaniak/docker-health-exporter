package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Container is the subset of the Docker list response this exporter needs.
// List has no restart count, start time, or live usage, so those come from
// Inspect and Stats.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// Health is Docker's inspect State.Health block (healthcheck status).
type Health struct {
	Status string `json:"Status"`
}

// Inspect is the subset of /containers/{id}/json this exporter needs.
// Health lives under State.Health in the real Docker JSON document.
type Inspect struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		StartedAt string  `json:"StartedAt"`
		Health    *Health `json:"Health,omitempty"`
	} `json:"State"`
	HostConfig struct {
		Memory int64 `json:"Memory"`
	} `json:"HostConfig"`
}

// MemoryStatsDetail holds cgroup memory fields nested under memory_stats.stats.
// cgroup v1 uses total_inactive_file; cgroup v2 uses inactive_file.
type MemoryStatsDetail struct {
	InactiveFile      *uint64 `json:"inactive_file"`
	TotalInactiveFile *uint64 `json:"total_inactive_file"`
}

// Stats is the subset of /containers/{id}/stats (one-shot) this exporter
// needs. CPU is nested under cpu_stats per the Docker API; disk fields are
// bytes; network fields are cumulative counters.
type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage        uint64 `json:"total_usage"`
			UsageInKernelmode uint64 `json:"usage_in_kernelmode"`
			UsageInUsermode   uint64 `json:"usage_in_usermode"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage    uint64            `json:"usage"`
		MaxUsage uint64            `json:"max_usage"`
		Limit    uint64            `json:"limit"`
		Stats    MemoryStatsDetail `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
		Limit   uint64 `json:"limit"`
	} `json:"pids_stats"`
}

// DockerAPI is the subset of the Docker HTTP API used to render /metrics.
type DockerAPI interface {
	List(ctx context.Context) ([]Container, error)
	Inspect(ctx context.Context, id string) (*Inspect, error)
	Stats(ctx context.Context, id string) (*Stats, error)
}

// Client talks to the Docker API over the unix socket or an HTTP base URL.
type Client struct {
	hc      *http.Client
	socket  string
	baseURL string // non-empty in tests (httptest server URL)
}

// New returns a Client dialing the Docker API at socket with the given
// per-request timeout.
func New(socket string, timeout time.Duration) *Client {
	return &Client{
		socket: socket,
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// NewHTTPClient returns a Client that GETs baseURL+path via hc (for tests).
func NewHTTPClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		hc:      hc,
		baseURL: baseURL,
	}
}

// List returns all containers (all=1) from the Docker API.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var out []Container
	if err := c.get(ctx, "/containers/json?all=1", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Inspect returns the health/restart/config subset for one container.
func (c *Client) Inspect(ctx context.Context, id string) (*Inspect, error) {
	if id == "" {
		return nil, fmt.Errorf("container has no id")
	}
	var out Inspect
	if err := c.get(ctx, "/containers/"+id+"/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stats returns the one-shot stats document for one running container.
// stream=false and one-shot=true (Engine 20.10 / API 1.41+) return one sample
// immediately instead of waiting ~1s for a second CPU priming sample.
func (c *Client) Stats(ctx context.Context, id string) (*Stats, error) {
	if id == "" {
		return nil, fmt.Errorf("container has no id")
	}
	var out Stats
	if err := c.get(ctx, "/containers/"+id+"/stats?stream=false&one-shot=true", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) get(ctx context.Context, path string, into any) error {
	url := "http://docker" + path
	if c.baseURL != "" {
		url = c.baseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker returned %s for %s", resp.Status, path)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
