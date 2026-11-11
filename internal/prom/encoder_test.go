package prom

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncoderHoistsHelpType(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.Gauge("container_cpu_usage_seconds_total", "Total CPU usage in seconds.", map[string]string{"name": "a"}, 1.5)
	enc.Gauge("container_cpu_usage_seconds_total", "Total CPU usage in seconds.", map[string]string{"name": "b"}, 2.5)

	out := buf.String()
	if strings.Count(out, "# HELP container_cpu_usage_seconds_total") != 1 {
		t.Errorf("expected one HELP line, got %d:\n%s", strings.Count(out, "# HELP container_cpu_usage_seconds_total"), out)
	}
	if !strings.Contains(out, `container_cpu_usage_seconds_total{name="a"} 1.5`) {
		t.Errorf("missing sample a:\n%s", out)
	}
}

func TestLabelsSorted(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.Counter("x", "x", map[string]string{"z": "1", "a": "2"}, 1)
	out := buf.String()
	ia, iz := strings.Index(out, `a="2"`), strings.Index(out, `z="1"`)
	if ia < 0 || iz < 0 || ia > iz {
		t.Errorf("labels not sorted: %s", out)
	}
}
