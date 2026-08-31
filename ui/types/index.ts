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
