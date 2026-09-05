# Current limitations

This list reflects the current implementation.

- `sync.maxConcurrent` is in-memory; restart recovery can briefly exceed the limit.
- Scheduling depends on one `status.nextSyncAt` requeue; a lost requeue may delay a Mirror.
- Mirror-specific Prometheus metrics are not yet exposed; the proposed metric set is documented in `docs/ai_generated/260905-falcon-prometheus-metrics.md`.
- Falcon currently relies on CRD schema/CEL and reconcile-time validation. A webhook is not required yet: the existing checks cover structural and controller-specific rules, while derived-resource validation remains delegated to Kubernetes and Gateway API. Consider a webhook later if validation must be shared before persistence or needs external state.
- If an active published PVC is deleted externally, Falcon does not reconstruct its immutable contents. This is deliberate: the source snapshot may also be gone, and recreating a PVC with the same name could produce an empty or different volume. Recovery requires a new synchronization and publication; operators should treat published PVCs and snapshots as controller-managed data.
- `/api/usage` supports OpenEBS ZFS LocalPV metadata only; other CSI data is omitted.
