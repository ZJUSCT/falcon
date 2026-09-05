# Kubernetes assumptions

Falcon relies on standard scheduling, Jobs, PVCs, VolumeSnapshots, and Gateway API resources. Recheck these assumptions when upgrading Kubernetes or the storage driver.

## Storage placement

Bound local PVs constrain Pods through PV `nodeAffinity`; Falcon leaves `spec.nodeName` unset so the scheduler can place synchronization Jobs and support `WaitForFirstConsumer`. Snapshot-clone PVCs do not currently propagate source locality, so Falcon reads the source PV (`status.workPVC` → `volumeName`) and copies required affinity into published Deployment Pods. If the PV is unavailable, publication waits. Shared storage without affinity needs no constraint.

## Jobs and retries

Synchronization Jobs use `backoffLimit: 0`. Falcon owns retries through `spec.sync.failureRetryLimit`, `retryInterval`, and `interval`, with counters persisted in status. `spec.sync.timeout` maps to `activeDeadlineSeconds`; finished Job retention follows `keepFailedJobs` and snapshot generations rather than Kubernetes TTL.
