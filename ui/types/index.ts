export interface MirrorCondition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason: string;
  message: string;
  observedGeneration?: number;
  lastTransitionTime: string;
}

// Wire types of the Kubernetes controller API.

export interface Job {
  conditions: MirrorCondition[];
  sync_phase?: 'Waiting' | 'Pending' | 'Syncing' | 'Snapshotting' | 'Retrying' | 'Cancelling';
  // Legacy-compatible fields.
  id: string;
  status: 'Waiting' | 'Running' | 'Paused' | string; // ProxyMirror: raw phase (Ready/Pending/Degraded)
  updated_at: string;
  last_success_at: string;
  last_failure_at: string;
  last_attempt_at: string;
  next_attempt_at: string;
  last_action_status: 'Running' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Cancelling' | 'Pending' | '';
  actions: string[]; // legacy field, always empty

  // New fields.
  kind: 'Mirror' | 'ProxyMirror';
  namespace?: string;
  phase: string; // raw CR status.phase
  active_pvc?: string;
  last_finished_at: string;
  paused: boolean;
  sync_busy: boolean;
  can_abort: boolean;
}

export const zeroTime = '0001-01-01T00:00:00Z';

export function isZeroTime(value: string | undefined): boolean {
  return !value || value === zeroTime || new Date(value).getTime() <= 0;
}

// GET /api/usage — cluster-wide storage usage aggregation. The endpoint
// replies 404 ({"error": "usage aggregation is disabled"}) when the usage
// feature is not deployed; the UI degrades silently to "no data".
// `MirrorUsage.name` matches the `id` of a /api/jobs entry (Mirror CR name).
// ProxyMirror resources never appear (no sync/snapshot concept).
export interface MirrorUsageSync {
  pvc: string; // ZFS dataset backing PVC
  referencedBytes: number;
  writtenBytes: number; // incremental bytes since latest snapshot (or referencedBytes when no baseline)
}

export interface MirrorUsageSnapshot {
  name: string;
  writtenBytes: number;
  referencedBytes: number;
  createdAt: number; // epoch seconds; snapshots are ordered newest first
}

export interface MirrorUsage {
  name: string;
  activeSnapshot?: string;
  sync: MirrorUsageSync | null; // null: no ZFS data yet (never synced or agent does not cover it)
  snapshots: MirrorUsageSnapshot[];
  totalBytes: number; // ZFS dataset usedBytes, including snapshot-held space
  complete: boolean; // false: some agent nodes did not respond (see errors) — data is advisory
  errors: string[];
}

export interface UsageResponse {
  generatedAt: string;
  mirrors: MirrorUsage[];
}

// GET /api/storage — per-node ZFS inventory (pools → datasets → snapshots),
// the raw material behind the usage aggregation. Same deployment story as
// /api/usage: the endpoint replies 404 when the usage feature is not
// deployed, and the UI degrades silently to "no data" (hint card).
//
// Sentinel conventions shared by most numeric fields: 0 means "unknown"
// (the agent could not read the property; ZFS itself never reports a real
// 0 for sizes/percentages in practice), "" health means unknown, and null
// object refs mean "no Kubernetes object is responsible" (manual dataset /
// manual snapshot). See internal/webapi storage handler for the encoder.
export interface StorageObjectRef {
  namespace: string; // Kubernetes namespace of the owning object
  name: string; // object name within the namespace
}

export interface StorageSnapshot {
  name: string; // full "dataset@snapshot" name
  volumeSnapshot: StorageObjectRef | null; // null: manual snapshot (no VolumeSnapshot CR)
  writtenBytes: number; // incremental bytes vs. the previous snapshot
  referencedBytes: number;
  createdAt: number; // epoch seconds
}

export interface StorageDataset {
  name: string; // full dataset path, e.g. "tank/pvc-xxxx"
  pvc: StorageObjectRef | null; // null: not openebs-managed (manual dataset)
  usedBytes: number; // on-disk footprint, including snapshot-held space
  referencedBytes: number;
  writtenBytes: number; // incremental bytes since the latest snapshot
  logicalUsedBytes: number; // pre-compression size; 0 = unknown
  snapshots: StorageSnapshot[];
}

export interface StoragePool {
  name: string; // ZFS pool name, e.g. "tank"
  sizeBytes: number; // 0 = unknown
  allocatedBytes: number;
  freeBytes: number;
  capacityPercent: number; // used percent; 0 = unknown
  fragmentationPercent: number; // 0 = unknown (also 0 when ZFS prints "-")
  health: string; // "" = unknown; ONLINE/DEGRADED/FAULTED/OFFLINE/UNAVAIL/...
  datasets: StorageDataset[];
}

export interface StorageNode {
  node: string; // storage agent hostname
  pools: StoragePool[];
}

export interface StorageResponse {
  generatedAt: string; // RFC3339
  complete: boolean; // false: some agent failed or no ready agent (see errors)
  errors: string[]; // per-agent failure reasons, e.g. `storage-2: unexpected status "500"`
  nodes: StorageNode[];
}
