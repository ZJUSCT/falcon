'use client';

// Per-mirror detail — read-only port of the legacy Mirror Detail view.
//
// Three data sources:
//   - GET /api/jobs  → live status row for this mirror (refreshed every 5s).
//   - GET /api/usage → storage usage for this mirror (refreshed every 30s;
//     silently absent when the usage feature is not deployed — the Storage
//     Usage card shows a hint instead of erroring).
//   - GET /api/repos/<id> → the Mirror/ProxyMirror CR **spec** as YAML,
//     rendered read-only in a plain monospace block with a copy button.
//
// Dropped relative to the legacy component: TriggerButton (manual sync) and
// the action history table — no backend anymore. The legacy ZFS storage
// panel returns as the Storage Usage card, backed by /api/usage. The spec is
// fetched once (it only changes when someone edits the CR; a page reload
// picks that up).

import { useState, useEffect, useCallback, useMemo } from 'react';
import type { ReactNode } from 'react';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { useUsage } from '@/lib/hooks';
import { formatBytes } from '@/lib/utils';
import { Job } from '@/types';
import { ArrowLeft, Check, Copy } from 'lucide-react';

interface MirrorDetailProps {
  mirrorId: string;
  onBack: () => void;
}

function SpecViewer({ mirrorId }: { mirrorId: string }) {
  const [spec, setSpec] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setSpec(null);
    setError(null);
    setCopied(false);
    apiClient
      .getRepoSpec(mirrorId)
      .then(text => {
        if (!cancelled) setSpec(text);
      })
      .catch(err => {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to fetch spec');
      });
    return () => {
      cancelled = true;
    };
  }, [mirrorId]);

  const handleCopy = useCallback(async () => {
    if (spec === null) return;
    try {
      await navigator.clipboard.writeText(spec);
    } catch {
      // Clipboard API can be unavailable (http:// origins other than
      // localhost). Fall back to a transient textarea + execCommand.
      const textarea = document.createElement('textarea');
      textarea.value = spec;
      document.body.appendChild(textarea);
      textarea.select();
      document.execCommand('copy');
      document.body.removeChild(textarea);
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }, [spec]);

  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="px-4 py-3 border-b flex justify-between items-center">
        <span className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
          Resource Spec (read-only)
        </span>
        <button
          onClick={handleCopy}
          disabled={spec === null}
          className="flex items-center gap-1.5 px-2.5 py-1 text-xs rounded-md border border-border bg-secondary text-secondary-foreground hover:bg-accent transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
          title="Copy spec to clipboard"
        >
          {copied ? <Check className="h-3.5 w-3.5 text-green-500" /> : <Copy className="h-3.5 w-3.5" />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <div className="p-4">
        {error ? (
          <div className="text-sm text-destructive font-mono">{error}</div>
        ) : spec === null ? (
          <div className="text-sm text-muted-foreground">Loading spec...</div>
        ) : (
          <pre className="text-xs font-mono leading-relaxed overflow-x-auto whitespace-pre text-foreground">
            {spec}
          </pre>
        )}
      </div>
    </div>
  );
}

// One line of the Storage Usage card: a monospace label (sync PVC or
// snapshot name) with its size on the right; `meta` renders extra context
// (e.g. the snapshot age) between label and size.
function UsageLine({ label, meta, bytes }: { label: string; meta?: ReactNode; bytes: number | null | undefined }) {
  const size = formatBytes(bytes);
  return (
    <div className="flex items-baseline justify-between gap-4">
      <span className="min-w-0 font-mono break-all">{label}</span>
      <span className="flex items-baseline gap-2 whitespace-nowrap">
        {meta}
        {size !== null ? (
          <span className="font-mono tabular-nums">{size}</span>
        ) : (
          <span className="font-mono text-muted-foreground">—</span>
        )}
      </span>
    </div>
  );
}

// Storage Usage card — per-mirror footprint from GET /api/usage (30s poll).
// Sync PVC first, then one row per snapshot (size + age), then the total.
// Degradations: ProxyMirror rows show "Not applicable" (no sync/snapshot
// concept), a missing record or a 404-ing endpoint shows a plain hint, and
// `complete: false` adds a light "data may be partial" notice.
function StorageUsageCard({ mirrorId, kind }: { mirrorId: string; kind: string | undefined }) {
  const usage = useUsage();
  const mirrorUsage = useMemo(() => (usage?.mirrors ?? []).find(entry => entry.name === mirrorId) ?? null, [usage, mirrorId]);

  return (
    <div className="rounded-lg border border-border bg-card p-4 space-y-3">
      <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Storage Usage</h3>
      {mirrorUsage && !mirrorUsage.complete && (
        <div className="text-xs text-amber-500">Some storage nodes did not respond; data may be partial.</div>
      )}
      {kind === 'ProxyMirror' ? (
        <div className="text-sm text-muted-foreground">Not applicable</div>
      ) : mirrorUsage === null ? (
        <div className="text-sm text-muted-foreground">Usage data unavailable</div>
      ) : (
        <div className="text-sm space-y-1.5">
          {mirrorUsage.sync ? (
            <div className="flex items-baseline justify-between gap-4">
              <span className="min-w-0 break-all">
                <span className="mr-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">Sync PVC</span>
                <span className="font-mono">{mirrorUsage.sync.pvc}</span>
              </span>
              <span className="whitespace-nowrap font-mono tabular-nums">
                {formatBytes(mirrorUsage.sync.referencedBytes) ?? '—'}
              </span>
            </div>
          ) : (
            <div className="text-muted-foreground">No ZFS data yet (never synced or not covered by an agent).</div>
          )}
          {mirrorUsage.snapshots.map(snapshot => (
            <UsageLine
              key={snapshot.name}
              label={snapshot.name}
              bytes={snapshot.writtenBytes}
              // Snapshot createdAt is epoch seconds; RelativeTime wants a date string.
              meta={<RelativeTime date={new Date(snapshot.createdAt * 1000).toISOString()} variant="compact" />}
            />
          ))}
          <div className="flex items-baseline justify-between gap-4 border-t border-border pt-1.5">
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Total</span>
            <span className="whitespace-nowrap font-mono tabular-nums">{formatBytes(mirrorUsage.totalBytes) ?? '—'}</span>
          </div>
        </div>
      )}
    </div>
  );
}

export function MirrorDetail({ mirrorId, onBack }: MirrorDetailProps) {
  const [job, setJob] = useState<Job | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const fetchJob = async () => {
      try {
        const jobs = await apiClient.getJobs();
        setJob(jobs.find(j => j.id === mirrorId) || null);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch mirror details');
      } finally {
        setLoading(false);
      }
    };

    setLoading(true);
    fetchJob();
    const interval = setInterval(() => {
      fetchJob().catch(err => console.warn('Background mirror detail refresh failed:', err));
    }, 5000);
    return () => clearInterval(interval);
  }, [mirrorId]);

  return (
    <div className="p-6 space-y-6">
      <button
        onClick={onBack}
        className="flex items-center gap-2 text-xs text-muted-foreground hover:text-foreground transition-colors"
      >
        <ArrowLeft className="h-4 w-4" />
        Back to Mirrors
      </button>

      <div className="flex items-center justify-between">
        <h1 className="text-lg font-bold font-mono">{mirrorId}</h1>
        <div className="flex items-center gap-2">
          {job && (
            <>
              <span
                className={`px-1.5 py-0.5 rounded text-[10px] font-mono ${
                  job.kind === 'ProxyMirror' ? 'bg-violet-500/15 text-violet-400' : 'bg-primary/10 text-primary'
                }`}
              >
                {job.kind || 'Mirror'}
              </span>
              <StatusBadge status={job.status} />
            </>
          )}
        </div>
      </div>

      {/* Job Status */}
      {loading ? (
        <div className="flex items-center justify-center h-32">
          <div className="text-muted-foreground">Loading mirror details...</div>
        </div>
      ) : error ? (
        <div className="flex items-center justify-center h-32">
          <div className="text-destructive">Error: {error}</div>
        </div>
      ) : job ? (
        <div className="rounded-lg border border-border bg-card p-4 space-y-3">
          <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Sync Status</h3>
          <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4 text-sm">
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Status</div>
              <StatusBadge status={job.status} />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Phase</div>
              <div className="font-mono">{job.phase || '—'}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Action</div>
              {job.last_action_status ? <StatusBadge status={job.last_action_status} /> : <span className="font-mono">—</span>}
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Namespace</div>
              <div className="font-mono">{job.namespace || '—'}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Active PVC</div>
              <div className="font-mono text-xs break-all">{job.active_pvc || '—'}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Success</div>
              <RelativeTime date={job.last_success_at} variant="compact" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Failure</div>
              <RelativeTime date={job.last_failure_at} variant="compact" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Attempt</div>
              <RelativeTime date={job.last_attempt_at} variant="compact" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Next Attempt</div>
              <RelativeTime date={job.next_attempt_at} variant="compact" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Finished</div>
              <RelativeTime date={job.last_finished_at} variant="compact" />
            </div>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card p-4 text-sm text-muted-foreground">
          No sync job found for this mirror (ProxyMirror resources without sync history may look like this).
        </div>
      )}

      {/* Storage Usage (GET /api/usage, 30s poll; silent degrade when absent) */}
      <StorageUsageCard mirrorId={mirrorId} kind={job?.kind} />

      {/* CRD spec, read-only */}
      <SpecViewer mirrorId={mirrorId} />
    </div>
  );
}
