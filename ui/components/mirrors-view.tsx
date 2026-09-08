'use client';

// Mirror list and administrator controls.

import { useState, useEffect, useMemo } from 'react';
import type { KeyboardEvent } from 'react';
import { ConditionBadges } from '@/components/condition-badges';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { useUsage } from '@/lib/hooks';
import { formatBytes } from '@/lib/utils';
import { Job } from '@/types';
import { Pause, Play, RefreshCw, Search, Square } from 'lucide-react';

const actionButtonClass = "inline-flex h-7 w-7 shrink-0 items-center justify-center rounded border border-border transition-colors enabled:hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:cursor-not-allowed disabled:opacity-40";

interface MirrorsViewProps {
  onMirrorClick: (id: string) => void;
}

// Active sync work first, then by next scheduled attempt (no schedule last).
const statusPriority: Record<string, number> = {
  Syncing: 0,
  Snapshotting: 0,
  Cancelling: 0,
  Pending: 1,
  Retrying: 2,
  Waiting: 3,
};

function compareJobs(a: Job, b: Job): number {
  const pA = statusPriority[a.sync_phase ?? ''] ?? 4;
  const pB = statusPriority[b.sync_phase ?? ''] ?? 4;
  if (pA !== pB) return pA - pB;

  const zeroDate = '0001-01-01T00:00:00Z';
  const dateA = a.next_attempt_at || zeroDate;
  const dateB = b.next_attempt_at || zeroDate;
  if (dateA === zeroDate && dateB !== zeroDate) return 1;
  if (dateB === zeroDate && dateA !== zeroDate) return -1;
  return new Date(dateA).getTime() - new Date(dateB).getTime();
}

export function MirrorsView({ onMirrorClick }: MirrorsViewProps) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [canAdminister, setCanAdminister] = useState(false);
  const [busyActions, setBusyActions] = useState<Set<string>>(new Set());
  const [actionError, setActionError] = useState<string | null>(null);
  useEffect(() => { apiClient.canAdminister().then(setCanAdminister).catch(() => setCanAdminister(false)); }, []);

  async function runAction(job: Job, action: 'pause' | 'resume' | 'sync' | 'abort') {
    const key = `${job.namespace}/${job.id}`;
    setBusyActions(previous => new Set(previous).add(key));
    setActionError(null);
    try {
      const updated = await apiClient.mirrorAction(job.id, action);
      setJobs(previous => previous.map(item => item.kind === 'Mirror' && item.id === updated.id && item.namespace === updated.namespace ? updated : item));
    } catch (error) {
      setActionError(`${job.id}: ${error instanceof Error ? error.message : 'Action failed'}`);
    } finally {
      setBusyActions(previous => { const next = new Set(previous); next.delete(key); return next; });
    }
  }
  const [search, setSearch] = useState('');
  const [conditionFilter, setConditionFilter] = useState('All');
  const [phaseFilter, setPhaseFilter] = useState('All');
  const usage = useUsage();

  // Join usage rows onto jobs by id (usage `name` === job `id`).
  const usageByName = useMemo(
    () => new Map((usage?.mirrors ?? []).map(entry => [entry.name, entry])),
    [usage]
  );

  useEffect(() => {
    const fetchInitial = async () => {
      try {
        setLoading(true);
        setJobs(await apiClient.getJobs());
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch mirrors');
      } finally {
        setLoading(false);
      }
    };

    fetchInitial();
    const interval = setInterval(() => {
      apiClient
        .getJobs()
        .then(setJobs)
        .catch(err => console.warn('Background mirrors refresh failed:', err));
    }, 5000);
    return () => clearInterval(interval);
  }, []);

  if (loading && jobs.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading mirrors...</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-destructive">Error: {error}</div>
      </div>
    );
  }

  const sorted = [...jobs].sort(compareJobs);

  const filtered = sorted.filter(job => {
    if (search && !job.id.toLowerCase().includes(search.toLowerCase())) return false;
    if (conditionFilter !== 'All' && !(job.conditions ?? []).some(condition => condition.type === conditionFilter && condition.status === 'True')) return false;
    if (phaseFilter !== 'All' && job.sync_phase !== phaseFilter) return false;
    return true;
  });

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, id: string) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onMirrorClick(id);
    }
  };

  const syncingCount = jobs.filter(job => job.sync_phase === 'Syncing' || job.sync_phase === 'Cancelling').length;
  const pendingCount = jobs.filter(job => job.sync_phase === 'Pending').length;
  const conditionCount = (type: string) => jobs.filter(job => job.conditions?.some(condition => condition.type === type && condition.status === 'True')).length;

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-bold">Mirrors</h2>
        <span className="text-xs text-muted-foreground font-mono">{jobs.length} total</span>
      </div>

      <div className="grid gap-3 grid-cols-2 md:grid-cols-4">
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Active syncs</div>
          <div className="text-xl font-bold tabular-nums text-blue-500 mt-1">{syncingCount}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Pending syncs</div>
          <div className="text-xl font-bold tabular-nums text-yellow-500 mt-1">{pendingCount}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Progressing</div>
          <div className="text-xl font-bold tabular-nums text-orange-500 mt-1">{conditionCount('Progressing')}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Degraded</div>
          <div className="text-xl font-bold tabular-nums text-red-500 mt-1">
            {conditionCount('Degraded')}
          </div>
        </div>
      </div>

      <div className="flex flex-col sm:flex-row gap-3">
        <div className="relative flex-1">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <input
            type="text"
            placeholder="Filter by name..."
            value={search}
            onChange={e => setSearch(e.target.value)}
            className="w-full pl-9 pr-3 py-1.5 text-xs rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/60"
          />
        </div>
        <select aria-label="Filter conditions" value={conditionFilter} onChange={e => setConditionFilter(e.target.value)} className="rounded-lg border border-input bg-background px-3 py-2 text-sm">
          <option value="All">All conditions</option>
          {['Ready', 'Progressing', 'Degraded'].map(value => <option key={value}>{value}</option>)}
        </select>
        <select aria-label="Filter sync phase" value={phaseFilter} onChange={e => setPhaseFilter(e.target.value)} className="rounded-lg border border-input bg-background px-3 py-2 text-sm">
          <option value="All">All sync phases</option>
          {['Waiting', 'Pending', 'Syncing', 'Snapshotting', 'Retrying', 'Cancelling'].map(value => <option key={value}>{value}</option>)}
        </select>
      </div>

      {actionError && <p role="alert" className="text-sm text-red-500">{actionError}</p>}

      {filtered.length === 0 ? (
        <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
          No mirrors match the current filter.
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                <tr>
                  <th className="px-3 py-2 text-center">Job</th>
                  <th className="px-3 py-2 text-center">Kind</th>
                  <th className="px-3 py-2 text-center">Conditions</th>
                  <th className="px-3 py-2 text-center">Sync phase</th>
                  {canAdminister && <th className="w-px px-3 py-2 text-center">Actions</th>}
                  <th className="hidden md:table-cell px-3 py-2 text-center">Size</th>
                  <th className="hidden md:table-cell px-3 py-2 text-center">Last Action</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Next Attempt</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Last Attempt</th>
                  <th className="hidden lg:table-cell px-3 py-2 text-left">Last Success</th>
                  <th className="hidden lg:table-cell px-3 py-2 text-left">Last Failure</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {filtered.map(job => {
                  const lastActionStatus = (job.last_action_status || '').trim();
                  const mirrorUsage = usageByName.get(job.id);
                  const sizeText = mirrorUsage ? formatBytes(mirrorUsage.totalBytes) : null;
                  return (
                    <tr
                      key={`${job.namespace}/${job.id}`}
                      onClick={() => onMirrorClick(job.id)}
                      onKeyDown={event => handleRowKeyDown(event, job.id)}
                      tabIndex={0}
                      className="group cursor-pointer bg-background transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60"
                    >
                      <td className="px-3 py-2 align-top">
                        <div className="font-mono text-sm">{job.id}</div>
                        <div className="mt-0.5 text-[11px] text-muted-foreground md:hidden">
                          <span className="uppercase tracking-wide">Next </span>
                          <RelativeTime date={job.next_attempt_at} variant="countdown" />
                        </div>
                        <div className="mt-0.5 flex items-center gap-1 text-[11px] text-muted-foreground md:hidden">
                          <span className="uppercase tracking-wide">Last Action</span>
                          {lastActionStatus ? (
                            <StatusBadge status={lastActionStatus} />
                          ) : (
                            <span className="font-mono">—</span>
                          )}
                        </div>
                      </td>
                      <td className="px-3 py-2 text-center align-top whitespace-nowrap">
                        <span
                          className={`px-1.5 py-0.5 rounded text-[10px] font-mono ${
                            job.kind === 'ProxyMirror'
                              ? 'bg-violet-500/15 text-violet-400'
                              : 'bg-primary/10 text-primary'
                          }`}
                        >
                          {job.kind || 'Mirror'}
                        </span>
                      </td>
                      <td className="px-3 py-2 text-center align-top">
                        <ConditionBadges conditions={job.conditions} />
                      </td>
                      <td className="px-3 py-2 text-center align-top">
                        {job.sync_phase ? <StatusBadge status={job.sync_phase} /> : <span className="text-muted-foreground">—</span>}
                        {job.kind === 'Mirror' && job.paused && <div className="mt-1 text-[10px] text-muted-foreground">Manual mode</div>}
                      </td>
                      {canAdminister && (
                        <td className="w-px px-3 py-2 align-top" onClick={event => event.stopPropagation()} onKeyDown={event => event.stopPropagation()}>
                          {job.kind === 'Mirror' && (
                            <div className="flex items-center justify-center gap-1">
                              <button type="button" className={actionButtonClass} disabled={busyActions.has(`${job.namespace}/${job.id}`)} aria-label={job.paused ? 'Resume schedule' : 'Pause schedule'} title={job.paused ? 'Resume automatic synchronization' : 'Pause automatic synchronization; manual sync remains available'} onClick={() => runAction(job, job.paused ? 'resume' : 'pause')}>
                                {job.paused ? <Play className="h-4 w-4" aria-hidden="true" /> : <Pause className="h-4 w-4" aria-hidden="true" />}
                              </button>
                              <button type="button" className={actionButtonClass} disabled={busyActions.has(`${job.namespace}/${job.id}`) || job.sync_busy} aria-label="Sync now" title={job.sync_busy ? 'A synchronization is already queued or active' : 'Request one synchronization'} onClick={() => runAction(job, 'sync')}><RefreshCw className="h-4 w-4" aria-hidden="true" /></button>
                              <button type="button" className={`${actionButtonClass} text-red-500`} aria-label="Abort sync" title="Abort the current synchronization" disabled={busyActions.has(`${job.namespace}/${job.id}`) || !job.can_abort} onClick={() => runAction(job, 'abort')}><Square className="h-4 w-4" aria-hidden="true" /></button>
                            </div>
                          )}
                        </td>
                      )}
                      <td className="hidden md:table-cell px-3 py-2 text-center align-top whitespace-nowrap">
                        {sizeText ? (
                          <span className="font-mono tabular-nums">
                            {sizeText}
                            {mirrorUsage && !mirrorUsage.complete && (
                              <span
                                className="ml-1 text-amber-500"
                                title="Partial data: some storage nodes did not respond"
                              >
                                ~
                              </span>
                            )}
                          </span>
                        ) : (
                          <span className="font-mono text-muted-foreground">—</span>
                        )}
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 text-center align-top">
                        {lastActionStatus ? (
                          <StatusBadge status={lastActionStatus} />
                        ) : (
                          <span className="font-mono text-muted-foreground">—</span>
                        )}
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.next_attempt_at} variant="countdown" />
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.last_attempt_at} variant="absolute" />
                      </td>
                      <td className="hidden lg:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.last_success_at} variant="absolute" />
                      </td>
                      <td className="hidden lg:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.last_failure_at} variant="absolute" />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
