'use client';

// Mirror list — read-only port of the legacy Mirrors view.
//
// Dropped relative to the legacy component: TriggerButton (manual sync),
// orphan-job deletion, worker matching. None of these have a backend
// anymore — the controller serves a strictly read-only API. The legacy ZFS
// size column returns as "Size", backed by the usage aggregation.
// Data sources: GET /api/jobs (5s poll) and GET /api/usage (30s poll;
// silently absent — the Size column shows `—` — when the usage feature is
// not deployed on the backend).

import { useState, useEffect, useMemo } from 'react';
import type { KeyboardEvent } from 'react';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { useUsage } from '@/lib/hooks';
import { formatBytes } from '@/lib/utils';
import { Job } from '@/types';
import { Search } from 'lucide-react';

interface MirrorsViewProps {
  onMirrorClick: (id: string) => void;
}

// Sort order mirrors the legacy view: active work first, then by next
// scheduled attempt (jobs with no schedule last).
const statusPriority: Record<string, number> = {
  Running: 0,
  Waiting: 2,
  Paused: 3,
};

function compareJobs(a: Job, b: Job): number {
  const pA = statusPriority[a.status] ?? 4;
  const pB = statusPriority[b.status] ?? 4;
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
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState<string>('All');
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
    if (statusFilter !== 'All') {
      if (statusFilter === 'Failed') {
        return job.last_action_status === 'Failed';
      }
      return job.status === statusFilter;
    }
    return true;
  });

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, id: string) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onMirrorClick(id);
    }
  };

  const jobsByStatus = jobs.reduce((acc, j) => {
    const s = j.status || 'Unknown';
    acc[s] = (acc[s] || 0) + 1;
    return acc;
  }, {} as Record<string, number>);

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-bold">Mirrors</h2>
        <span className="text-xs text-muted-foreground font-mono">{jobs.length} total</span>
      </div>

      <div className="grid gap-3 grid-cols-2 md:grid-cols-4">
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Running</div>
          <div className="text-xl font-bold tabular-nums text-blue-500 mt-1">{jobsByStatus.Running || 0}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Waiting</div>
          <div className="text-xl font-bold tabular-nums text-yellow-500 mt-1">{jobsByStatus.Waiting || 0}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Paused</div>
          <div className="text-xl font-bold tabular-nums text-orange-500 mt-1">{jobsByStatus.Paused || 0}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Last Sync Failed</div>
          <div className="text-xl font-bold tabular-nums text-red-500 mt-1">
            {jobs.filter(j => j.last_action_status === 'Failed').length}
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
        <select
          value={statusFilter}
          onChange={e => setStatusFilter(e.target.value)}
          className="px-3 py-1.5 text-xs rounded-md border border-border bg-card text-foreground focus:outline-none focus:ring-2 focus:ring-primary/60"
        >
          <option value="All">All Statuses</option>
          <option value="Running">Running</option>
          <option value="Waiting">Waiting</option>
          <option value="Paused">Paused</option>
          <option value="Failed">Failed</option>
        </select>
      </div>

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
                  <th className="px-3 py-2 text-center">Status</th>
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
                        <StatusBadge status={job.status} />
                        {job.phase && job.phase !== job.status && (
                          <div className="mt-1 text-[10px] font-mono text-muted-foreground">{job.phase}</div>
                        )}
                      </td>
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
