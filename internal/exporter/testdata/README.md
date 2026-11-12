# Test fixtures

JSON files here mirror real Docker API responses (`/containers/json`, `/json`,
`/stats`).

## Regenerating golden Prometheus text

Golden output for the happy-path regression lives in
`metrics_golden.prom`. Regenerate after intentional metric changes:

```sh
go test ./internal/exporter/ -run TestGoldenMetricsEquivalence -update
```

The test injects a fixed scrape time (`1700000000`) so output is stable.
