'use client';

// Mirror state, storage, retained logs and its read-only resource specification.
// Mode names are presentation hints; every resource is a Mirror CR.

import { useState, useEffect, useCallback, useMemo } from 'react';
import { SyncLogs } from '@/components/sync-logs';
import { MirrorModeBadge } from '@/components/mirror-mode-badge';
import { ConditionBadges } from '@/components/condition-badges';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { useUsage, useStorage } from '@/lib/hooks';
import { formatBytes, formatStorageQuota } from '@/lib/utils';
import { Job, mirrorMode, displaySyncPhase } from '@/types';
import { durationLabel } from '@/lib/timeline';
import { ArrowLeft, Check, Copy, ChevronRight } from 'lucide-react';

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
    <details className="group/spec min-w-0 rounded-lg border border-border bg-card">
      <summary className="flex cursor-pointer list-none items-center gap-2 p-4 text-sm font-semibold [&::-webkit-details-marker]:hidden">
        <ChevronRight className="h-4 w-4 transition-transform group-open/spec:rotate-90" aria-hidden="true" />Resource specification
        <span className="ml-auto text-xs font-normal text-muted-foreground">YAML</span>
      </summary>
      <div className="px-4 py-3 border-t border-b flex justify-between items-center">
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
    </details>
  );
}

// Storage Usage card — per-mirror footprint from GET /api/usage (30s poll).
// Sync PVC first, then one row per snapshot (size + age), then the total.
// Missing data is explicit; incomplete aggregation is marked as partial.
// Cache mode uses the separate inventory-based card below.
function StorageUsageCard({ job }: { job: Job }) {
  const usage = useUsage();
  const mirrorUsage = useMemo(() => (usage?.mirrors ?? []).find(entry => entry.name === job.id) ?? null, [usage, job.id]);

  return (
    <div className="rounded-lg border border-border bg-card p-4 space-y-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Storage Usage</h3>
        <span className="font-mono text-sm" title="ZFS footprint including snapshots / configured PVC capacity">{formatStorageQuota(mirrorUsage?.totalBytes, job.storage_quota_bytes)}</span>
      </div>
      <p className="text-xs text-muted-foreground">Used / configured capacity, including snapshots. Rows show incremental space; the oldest snapshot is the baseline.</p>
      {mirrorUsage && !mirrorUsage.complete && (
        <div className="text-xs text-amber-500">Some storage nodes did not respond; data may be partial.</div>
      )}
      {mirrorUsage === null ? (
        <div className="text-sm text-muted-foreground">Usage data unavailable</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-[10px] uppercase tracking-wide text-muted-foreground border-b border-border">
              <tr><th className="py-1.5 text-left font-semibold">Name</th><th className="py-1.5 text-left font-semibold">Time</th><th className="py-1.5 text-right font-semibold">Space</th></tr>
            </thead>
            <tbody className="divide-y divide-border/60">
              {mirrorUsage.sync && (
                <tr>
                  <td className="py-2 pr-4 break-all">
                    <span className="font-mono">{mirrorUsage.sync.pvc}</span>
                    <span className="ml-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">Sync</span>
                  </td>
                  <td className="py-2 text-muted-foreground">{job.last_finished_at ? <RelativeTime date={job.last_finished_at} /> : '—'}</td>
                  <td className="py-2 pl-4 text-right font-mono tabular-nums whitespace-nowrap">{formatBytes(mirrorUsage.sync.writtenBytes) ?? '—'}</td>
                </tr>
              )}
              {!mirrorUsage.sync && mirrorUsage.snapshots.length === 0 && (
                <tr><td colSpan={3} className="py-2 text-muted-foreground">No ZFS data yet (never synced or not covered by an agent).</td></tr>
              )}
              {mirrorUsage.snapshots.map((snapshot, index) => (
                <tr key={snapshot.name}>
                  <td className="py-2 pr-4 break-all">
                    <span className="font-mono">{snapshot.name}</span>
                    {snapshot.name === mirrorUsage.activeSnapshot && (
                      <span className="ml-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">Active</span>
                    )}
                  </td>
                  <td className="py-2"><RelativeTime date={new Date(snapshot.createdAt * 1000).toISOString()} /></td>
                  <td className="py-2 pl-4 text-right font-mono tabular-nums whitespace-nowrap">{formatBytes(index === mirrorUsage.snapshots.length - 1 ? snapshot.referencedBytes : snapshot.writtenBytes) ?? '—'}</td>
                </tr>
              ))}
              <tr className="border-t border-border">
                <td className="py-2 font-semibold" colSpan={2}>Total</td>
                <td className="py-2 pl-4 text-right font-mono tabular-nums whitespace-nowrap">{formatBytes(mirrorUsage.totalBytes) ?? '—'}</td>
              </tr>
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function CacheStorageCard({ job }: { job: Job }) {
  const storage = useStorage();
  const datasets = storage?.nodes.flatMap(node => node.pools.flatMap(pool => pool.datasets))
    .filter(dataset => dataset.mirror === job.id && dataset.pvc?.namespace === job.namespace) ?? [];
  const used = datasets.length > 0 ? datasets.reduce((total, dataset) => total + dataset.usedBytes, 0) : undefined;
  return <section className="rounded-lg border border-border bg-card p-4 space-y-2">
    <div className="flex flex-wrap justify-between gap-2">
      <h3 className="text-sm font-semibold">Cache storage</h3>
      <span className="font-mono">{formatStorageQuota(used, job.storage_quota_bytes)}</span>
    </div>
    <p className="text-xs text-muted-foreground">Used / configured capacity. Cached content is populated on demand.</p>
    {storage && !storage.complete && <p className="text-xs text-amber-600">Some storage nodes did not respond; usage may be incomplete.</p>}
    {datasets.length === 0 && <p className="text-xs text-muted-foreground">Cache usage is not available from the storage inventory.</p>}
  </section>;
}

export function MirrorDetail({ mirrorId, onBack }: MirrorDetailProps) {
  const [job, setJob] = useState<Job | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const fetchJob = async () => {
      try {
        const jobs = await apiClient.getJobs();
        if (cancelled) return;
        setJob(jobs.find(j => j.id === mirrorId) || null);
        setError(null);
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to fetch mirror details');
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    setLoading(true);
    setJob(null);
    fetchJob();
    const interval = setInterval(fetchJob, 5000);
    return () => { cancelled = true; clearInterval(interval); };
  }, [mirrorId]);

  const phase = job ? displaySyncPhase(job) : undefined;
  return (
    <div className="min-w-0 p-4 sm:p-6 space-y-4">
      <button onClick={onBack} className="flex items-center gap-2 text-xs text-muted-foreground hover:text-foreground transition-colors">
        <ArrowLeft className="h-4 w-4" />Back to Mirrors
      </button>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2">
          {job && <MirrorModeBadge job={job} />}
          <h1 className="break-all text-lg font-bold font-mono">{mirrorId}</h1>
        </div>
        {job && <ConditionBadges conditions={job.conditions} />}
      </div>
      {loading ? <p className="py-8 text-sm text-muted-foreground">Loading mirror details…</p> :
        error ? <p role="alert" className="py-8 text-sm text-destructive">{error}</p> :
        !job ? <p className="rounded-lg border p-4 text-sm text-muted-foreground">Mirror not found.</p> : <>
          <section className="rounded-lg border border-border bg-card p-4 space-y-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-sm font-semibold">{mirrorMode(job) === 'sync' ? 'Synchronization' : 'Publication'}</h2>
              {phase && <StatusBadge status={phase} />}
            </div>
            {mirrorMode(job) === 'sync' ? <>
              <div className="grid grid-cols-2 gap-x-6 gap-y-4 text-xs lg:grid-cols-4">
                <div><div className="mb-1 text-muted-foreground">Last result</div>{job.last_action_status ? <StatusBadge status={job.last_action_status} /> : '—'}</div>
                <div><div className="mb-1 text-muted-foreground">Last attempt</div><RelativeTime date={job.last_attempt_at} /></div>
                <div><div className="mb-1 text-muted-foreground">Next attempt</div>{job.paused ? <span>Schedule paused</span> : <RelativeTime date={job.next_attempt_at} />}</div>
                <div><div className="mb-1 text-muted-foreground">Previous sync duration</div>{job.last_sync_duration_seconds ? durationLabel(job.last_sync_duration_seconds * 1000) : 'Not available'}</div>
                <div><div className="mb-1 text-muted-foreground">Last success</div><RelativeTime date={job.last_success_at} /></div>
                <div><div className="mb-1 text-muted-foreground">Last failure</div><RelativeTime date={job.last_failure_at} /></div>
                <div><div className="mb-1 text-muted-foreground">Last finished</div><RelativeTime date={job.last_finished_at} /></div>
                <div><div className="mb-1 text-muted-foreground">Active volume</div><span className="break-all font-mono">{job.active_pvc || '—'}</span></div>
              </div>
            </> : <p className="text-xs text-muted-foreground">{mirrorMode(job) === 'cache' ? 'Requests are served through a local cache.' : 'Requests are forwarded to the upstream.'} There is no periodic synchronization.</p>}
          </section>

          {(job.conditions ?? []).length > 0 && <details className="group/conditions rounded-lg border border-border bg-card">
            <summary className="flex cursor-pointer list-none items-center gap-2 p-4 text-sm font-semibold [&::-webkit-details-marker]:hidden">
              <ChevronRight className="h-4 w-4 transition-transform group-open/conditions:rotate-90" aria-hidden="true" />Condition details
              <span className="ml-auto text-xs font-normal text-muted-foreground">{job.conditions.length} observations</span>
            </summary>
            <div className="space-y-3 border-t p-4">
              {job.conditions.map(condition => <div key={condition.type} className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs">
                <span className="font-semibold">{condition.type}</span>
                <span className={`rounded px-1.5 py-0.5 ${condition.status === 'True' ? 'bg-primary/10 text-primary' : 'bg-muted text-muted-foreground'}`}>{condition.status}</span>
                <span className="font-mono text-muted-foreground">{condition.reason}</span>
                <span className="ml-auto text-muted-foreground"><RelativeTime date={condition.lastTransitionTime} /></span>
                {condition.message && <p className="w-full break-words text-muted-foreground">{condition.message}</p>}
              </div>)}
            </div>
          </details>}

          {mirrorMode(job) === 'sync' && <StorageUsageCard job={job} />}
          {mirrorMode(job) === 'cache' && <CacheStorageCard job={job} />}
          {/* Sibling keys must differ while still resetting each panel on mirror changes. */}
          {mirrorMode(job) === 'sync' && <SyncLogs key={`logs:${mirrorId}`} mirrorId={mirrorId} />}
          <SpecViewer key={`spec:${mirrorId}`} mirrorId={mirrorId} />
        </>}
    </div>
  );
}
