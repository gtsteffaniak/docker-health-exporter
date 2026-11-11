// Package exporter implements the Docker API client and the
// /metrics renderer for docker-health-exporter.
//
// Everything here is stdlib only, on purpose: the binary stays static,
// has no dependency to vendor, and can run in a distroless/scrach image
// with the Docker socket as its single host mount.
//
// The scrape path is deliberately stateless — no background poller, no
// cache. One HTTP request to /metrics fans out to one Docker list call,
// one inspect per container, and one stats call per running container.
// With the default 30s Alloy scrape that is a handful of unix-socket
// round trips, which is both simpler and more predictable on memory than
// keeping a cAdvisor-sized housekeeper alive.
package exporter
