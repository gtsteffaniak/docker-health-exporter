// Package exporter implements the Docker API client and the
// /metrics renderer for docker-health-exporter.
//
// Everything here is stdlib only, on purpose: the binary stays static,
// has no dependency to vendor, and can run in a distroless/scrach image
// with the Docker socket as its single host mount.
//
// Scraping /metrics is stateless aside from container_oom_events_total: one
// HTTP request fans out to list, inspect per container, and stats per running
// container. A background goroutine streams GET /events (filtered to container
// oom) over the same socket into an in-memory counter; counts reset on
// exporter restart. With the default 30s Alloy scrape, /metrics is a handful
// of unix-socket round trips — far lighter than a cAdvisor-sized housekeeper.
package exporter
