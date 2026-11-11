// Package prom is a minimal Prometheus text-format encoder.
//
// One writer per scrape, HELP/TYPE hoisted to one line per family, labels
// sorted for stable output (sort is capped at family size, not container
// count, so this stays cheap). No client libraries: the exporter already
// vendors nothing.
package prom

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Encoder writes to an io.Writer with HELP/TYPE emitted on first use per
// family.
type Encoder struct {
	w    io.Writer
	seen map[string]struct{}
}

// Row is kept for parity with callers that accumulate per-container rows.
type Row struct {
	Labels map[string]string
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w, seen: make(map[string]struct{})}
}

// family emits HELP/TYPE once per family.
func (e *Encoder) family(name, help, typ string) {
	if _, ok := e.seen[name]; ok {
		return
	}
	e.seen[name] = struct{}{}
	_, _ = fmt.Fprintf(e.w, "# HELP %s %s\n", name, help)
	_, _ = fmt.Fprintf(e.w, "# TYPE %s %s\n", name, typ)
}

func labelsString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%s=%q", k, labels[k])
	}
	b.WriteString("}")
	return b.String()
}

func (e *Encoder) sample(name string, labels map[string]string, v float64) {
	_, _ = fmt.Fprintf(e.w, "%s%s %g\n", name, labelsString(labels), v)
}

// Gauge writes a gauge sample.
func (e *Encoder) Gauge(name, help string, labels map[string]string, v float64) {
	e.family(name, help, "gauge")
	e.sample(name, labels, v)
}

// Counter writes a counter sample.
func (e *Encoder) Counter(name, help string, labels map[string]string, v float64) {
	e.family(name, help, "counter")
	e.sample(name, labels, v)
}
