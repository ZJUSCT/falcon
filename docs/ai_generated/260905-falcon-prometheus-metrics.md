# Proposed Falcon Prometheus metrics

Falcon should expose a small set of labels and gauges that describe mirror
freshness and publication availability. The custom UI remains the primary
operator view; Prometheus metrics are intended for alerting, dashboards, and
capacity trends.

Recommended labels are `namespace`, `name`, and `kind` (`Mirror` or
`ProxyMirror`). Avoid unbounded labels such as URLs, error messages, pod names,
snapshot names, or sync request values.

## Mirror synchronization

- `falcon_mirror_sync_status{...}`: gauge, one for the current main state
  (`idle`, `queued`, `running`, `snapshotting`, `publishing`, `paused`,
  `failed`). Value is 1 for the selected state.
- `falcon_mirror_sync_started_at_seconds{...}`: gauge containing the current
  transaction start time, or 0 when idle.
- `falcon_mirror_sync_finished_at_seconds{...}`: gauge containing the most
  recent terminal sync finish time.
- `falcon_mirror_sync_next_at_seconds{...}`: gauge containing the next
  scheduled attempt, or 0 when none is scheduled.
- `falcon_mirror_sync_consecutive_failures{...}`: gauge from
  `status.consecutiveFailures`.
- `falcon_mirror_sync_total{...,result="succeeded|failed"}`: counter of
  observed terminal sync results.

## Publication

- `falcon_mirror_ready{...}`: gauge, 1 when the current generation is Ready.
- `falcon_mirror_degraded{...}`: gauge, 1 when the current generation is
  Degraded.
- `falcon_mirror_progressing{...}`: gauge, 1 while synchronization or
  publication is converging.
- `falcon_mirror_publish_available{...,service="http|rsync"}`: gauge, 1 when
  the service has an available deployment endpoint.
- `falcon_mirror_active_snapshot_timestamp_seconds{...}`: gauge containing
  the timestamp encoded in the active snapshot name, or 0 when unpublished.
- `falcon_mirror_published_bytes{...}`: gauge from `status.sizeBytes`, or 0
  when unavailable.

## Controller health and capacity

- `falcon_sync_jobs_running`: gauge of currently running synchronization Jobs.
- `falcon_sync_jobs_queued`: gauge of transactions waiting for the global
  concurrency limit.
- `falcon_reconcile_total{controller,result="success|error"}`: counter.
- `falcon_reconcile_duration_seconds{controller}`: histogram.

Metrics should be emitted from controller state transitions and the cached CR
state, with bounded label values. They should complement conditions and Events,
not replace them; users needing the exact reason or message should inspect the
CR status.
