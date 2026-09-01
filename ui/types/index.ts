// Wire types of the new (Kubernetes controller) read-only API.
//
// Job mirrors the JSON of GET /api/jobs served by internal/webapi (see
// JobEntry there): the legacy Docker-era shape plus the new K8s-specific
// fields (kind, namespace, phase, active_pvc, last_finished_at).
// Everything mutation-related from the old UI (queue, workers, actions,
// repo editing) no longer exists and therefore has no type here.

export interface Job {
  // Legacy-compatible fields.
  id: string;
  status: 'Waiting' | 'Running' | 'Paused' | string; // ProxyMirror: raw phase (Ready/Pending/Degraded)
  updated_at: string;
  last_success_at: string;
  last_failure_at: string;
  last_attempt_at: string;
  next_attempt_at: string;
  last_action_status: 'Running' | 'Succeeded' | 'Failed' | '';
  actions: string[]; // legacy field, always empty

  // New fields.
  kind: 'Mirror' | 'ProxyMirror';
  namespace?: string;
  phase: string; // raw CR status.phase
  active_pvc?: string;
  last_finished_at: string;
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
