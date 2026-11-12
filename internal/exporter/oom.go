package exporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"strconv"
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
	seen   map[string]map[int64]struct{} // container ID -> event TimeNano
}

// NewOOMStore returns an empty OOM counter store.
func NewOOMStore() *OOMStore {
	return &OOMStore{
		counts: make(map[string]uint64),
		seen:   make(map[string]map[int64]struct{}),
	}
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

// RecordOOM increments the oom counter unless this (containerID, timeNano) was
// already recorded (reconnect replay dedupe). timeNano may be 0 when Docker
// omits it; those events are counted without dedupe.
func (s *OOMStore) RecordOOM(containerID string, timeNano int64) {
	if s == nil || containerID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if timeNano > 0 {
		m := s.seen[containerID]
		if m == nil {
			m = make(map[int64]struct{})
			s.seen[containerID] = m
		}
		if _, dup := m[timeNano]; dup {
			return
		}
		m[timeNano] = struct{}{}
	}
	s.counts[containerID]++
}

func eventTimeNano(ev DockerEvent) int64 {
	if ev.TimeNano > 0 {
		return ev.TimeNano
	}
	if ev.Time > 0 {
		return ev.Time * 1_000_000_000
	}
	return 0
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
	var lastEventNano atomic.Int64
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		var since time.Time
		if t := lastEventNano.Load(); t > 0 {
			// Inclusive since: Docker may return several OOMs at the same
			// timestamp. Reconnecting at t lets a later container in that
			// second arrive; OOMStore dedupes exact (id, TimeNano) replays.
			since = time.Unix(0, t)
		}
		err := watchOOMStream(ctx, client, store, since, &lastEventNano)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil || err == io.EOF:
			// Stream ended cleanly (EOF on normal close). Reset delay so a later
			// blip does not inherit a 30s wait from earlier failures.
			backoff = time.Second
		default:
			log.Printf("docker events stream: %v; reconnecting in %s", err, backoff)
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

func watchOOMStream(ctx context.Context, client EventsClient, store *OOMStore, since time.Time, lastEventNano *atomic.Int64) error {
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
		tn := eventTimeNano(ev)
		if tn > 0 {
			for {
				prev := lastEventNano.Load()
				if tn <= prev {
					break
				}
				if lastEventNano.CompareAndSwap(prev, tn) {
					break
				}
			}
		}
		if !strings.EqualFold(ev.Action, "oom") {
			continue
		}
		id := ev.Actor.ID
		if id == "" {
			continue
		}
		store.RecordOOM(id, ev.TimeNano)
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

// dockerEventSinceParam encodes since for GET /events. Docker Engine expects a
// Unix timestamp with optional fractional seconds, not RFC3339.
func dockerEventSinceParam(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	sec := since.Unix()
	nsec := since.Nanosecond()
	if nsec == 0 {
		return strconv.FormatInt(sec, 10)
	}
	return fmt.Sprintf("%d.%09d", sec, nsec)
}

// eventsPath returns GET /events?filters=...&since=... query path.
func eventsPath(since time.Time) string {
	q := url.Values{}
	q.Set("filters", eventsFiltersJSON())
	if s := dockerEventSinceParam(since); s != "" {
		q.Set("since", s)
	}
	return "/events?" + q.Encode()
}
