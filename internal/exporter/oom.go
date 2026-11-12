package exporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DockerEvent is a subset of the Docker Engine /events JSON stream.
type DockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

// OOMStore holds cumulative per-container OOM event counts from Docker /events.
// Counts reset when the exporter process restarts (same as cAdvisor's in-memory
// path); Prometheus increase() handles counter resets.
type OOMStore struct {
	mu     sync.RWMutex
	counts map[string]uint64
}

// NewOOMStore returns an empty OOM counter store.
func NewOOMStore() *OOMStore {
	return &OOMStore{counts: make(map[string]uint64)}
}

// Count returns the cumulative oom events for containerID (0 if none).
func (s *OOMStore) Count(containerID string) uint64 {
	if s == nil || containerID == "" {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.counts[containerID]
}

// Increment adds one to the oom counter for containerID.
func (s *OOMStore) Increment(containerID string) {
	if s == nil || containerID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[containerID]++
}

// EventsClient is the Docker /events stream used by the OOM watcher.
type EventsClient interface {
	Events(ctx context.Context, since time.Time) (io.ReadCloser, error)
}

// RunOOMWatcher streams Docker container oom events into store until ctx ends.
// It reconnects with bounded exponential backoff on stream errors. On
// reconnect it passes since=lastEvent to fill gaps; the daemon retains only
// the last 256 events of all types, so that backfill is best-effort only.
func RunOOMWatcher(ctx context.Context, client EventsClient, store *OOMStore) {
	if client == nil || store == nil {
		return
	}
	var sinceUnix atomic.Int64
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		var since time.Time
		if t := sinceUnix.Load(); t > 0 {
			since = time.Unix(t, 0)
		}
		err := watchOOMStream(ctx, client, store, since, &sinceUnix)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("docker events stream: %v; reconnecting in %s", err, backoff)
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func watchOOMStream(ctx context.Context, client EventsClient, store *OOMStore, since time.Time, sinceUnix *atomic.Int64) error {
	body, err := client.Events(ctx, since)
	if err != nil {
		return err
	}
	defer body.Close()

	sc := bufio.NewScanner(body)
	// Docker event lines are small JSON objects; allow headroom for attributes.
	const maxLine = 256 * 1024
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, maxLine)

	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev DockerEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if !strings.EqualFold(ev.Type, "container") {
			continue
		}
		if ev.Time > 0 {
			sinceUnix.Store(ev.Time)
		}
		if !strings.EqualFold(ev.Action, "oom") {
			continue
		}
		id := ev.Actor.ID
		if id == "" {
			continue
		}
		store.Increment(id)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// eventsFiltersJSON is the URL-encoded filter for container oom events only.
func eventsFiltersJSON() string {
	f := map[string][]string{
		"type":  {"container"},
		"event": {"oom"},
	}
	raw, _ := json.Marshal(f)
	return string(raw)
}

// eventsPath returns GET /events?filters=...&since=... query path.
func eventsPath(since time.Time) string {
	q := url.Values{}
	q.Set("filters", eventsFiltersJSON())
	if !since.IsZero() {
		q.Set("since", since.Format(time.RFC3339Nano))
	}
	return "/events?" + q.Encode()
}
