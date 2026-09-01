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
          Resource Spec
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

// Storage Usage card — per-mirror footprint from GET /api/usage (30s poll).
// Sync PVC first, then one row per snapshot (size + age), then the total.
// Degradations: ProxyMirror rows show "Not applicable" (no sync/snapshot
// concept), a missing record or a 404-ing endpoint shows a plain hint, and
// `complete: false` adds a light "data may be partial" notice.
function StorageUsageCard({ mirrorId, kind, syncTime }: { mirrorId: string; kind: string | undefined; syncTime?: string }) {
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
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-[10px] uppercase tracking-wide text-muted-foreground border-b border-border">
              <tr><th className="py-1.5 text-left font-semibold">Name</th><th className="py-1.5 text-left font-semibold">Time</th><th className="py-1.5 text-right font-semibold">Size</th></tr>
            </thead>
            <tbody className="divide-y divide-border/60">
              {mirrorUsage.sync && (
                <tr>
                  <td className="py-2 break-all">
                    <span className="font-mono">{mirrorUsage.sync.pvc}</span>
                    <span className="ml-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">Sync</span>
                  </td>
                  <td className="py-2 text-muted-foreground">{syncTime ? <RelativeTime date={syncTime} variant="absolute" /> : '—'}</td>
                  <td className="py-2 text-right font-mono tabular-nums">{formatBytes(mirrorUsage.sync.writtenBytes) ?? '—'}</td>
                </tr>
              )}
              {!mirrorUsage.sync && mirrorUsage.snapshots.length === 0 && (
                <tr><td colSpan={3} className="py-2 text-muted-foreground">No ZFS data yet (never synced or not covered by an agent).</td></tr>
              )}
              {mirrorUsage.snapshots.map((snapshot, index) => (
                <tr key={snapshot.name}>
                  <td className="py-2 break-all">
                    <span className="font-mono">{snapshot.name}</span>
                    {snapshot.name === mirrorUsage.activeSnapshot && (
                      <span className="ml-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">Active</span>
                    )}
                  </td>
                  <td className="py-2"><RelativeTime date={new Date(snapshot.createdAt * 1000).toISOString()} variant="absolute" /></td>
                  <td className="py-2 text-right font-mono tabular-nums">{formatBytes(index === mirrorUsage.snapshots.length - 1 ? snapshot.referencedBytes : snapshot.writtenBytes) ?? '—'}</td>
                </tr>
              ))}
              <tr className="border-t border-border">
                <td className="py-2 font-semibold" colSpan={2}>Total</td>
                <td className="py-2 text-right font-mono tabular-nums">{formatBytes(mirrorUsage.totalBytes) ?? '—'}</td>
              </tr>
            </tbody>
          </table>
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
              <RelativeTime date={job.last_success_at} variant="absolute" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Failure</div>
              <RelativeTime date={job.last_failure_at} variant="absolute" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Attempt</div>
              <RelativeTime date={job.last_attempt_at} variant="absolute" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Next Attempt</div>
              <RelativeTime date={job.next_attempt_at} variant="countdown" />
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Last Finished</div>
              <RelativeTime date={job.last_finished_at} variant="absolute" />
            </div>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card p-4 text-sm text-muted-foreground">
          No sync job found for this mirror (ProxyMirror resources without sync history may look like this).
        </div>
      )}

      {/* Storage Usage (GET /api/usage, 30s poll; silent degrade when absent) */}
      <StorageUsageCard mirrorId={mirrorId} kind={job?.kind} syncTime={job?.last_finished_at} />

      {/* CRD spec, read-only */}
      <SpecViewer mirrorId={mirrorId} />
    </div>
  );
}
