'use client';

import { useState, useEffect } from 'react';
import type { KeyboardEvent } from 'react';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { TriggerButton } from '@/components/trigger-button';
import { apiClient } from '@/lib/api';
import { Job, Repo, Action, Worker, ZFSWorkerReport } from '@/types';
import { formatBytes } from '@/lib/utils';
import { Search, Trash2 } from 'lucide-react';

function matchWorker(worker: Worker, repo: Repo): boolean {
  if (repo.sync.node && worker.name !== repo.sync.node) return false;
  const sel = repo.sync.nodeSelector;
  if (sel) {
    for (const [k, v] of Object.entries(sel)) {
      if (!worker.labels || worker.labels[k] !== v) return false;
    }
  }
  return true;
}

interface MirrorsViewProps {
  onMirrorClick: (id: string) => void;
}

interface MirrorRow {
  id: string;
  repo: Repo | null;
  job: Job | null;
}

export function MirrorsView({ onMirrorClick }: MirrorsViewProps) {
  const [mirrors, setMirrors] = useState<MirrorRow[]>([]);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [zfsReports, setZfsReports] = useState<ZFSWorkerReport[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState<string>('All');

  const buildMirrors = (repos: Repo[], jobs: Job[]): MirrorRow[] => {
    const jobMap = new Map(jobs.map(j => [j.id, j]));
    const repoMap = new Map(repos.map(r => [r.id, r]));
    const allIds = new Set([...repos.map(r => r.id), ...jobs.map(j => j.id)]);

    return Array.from(allIds)
      .map(id => ({
        id,
        repo: repoMap.get(id) || null,
        job: jobMap.get(id) || null,
      }))
      .sort((a, b) => {
        const statusPriority: Record<string, number> = {
          'Running': 0,
          'Scheduled': 1,
          'Waiting': 2,
          'Paused': 3,
          'Orphan': 4,
        };
        const pA = statusPriority[a.job?.status || ''] ?? 4;
        const pB = statusPriority[b.job?.status || ''] ?? 4;
        if (pA !== pB) return pA - pB;

        const dateA = a.job?.next_attempt_at || '';
        const dateB = b.job?.next_attempt_at || '';
        const zeroDate = '0001-01-01T00:00:00Z';
        if (dateA === zeroDate && dateB !== zeroDate) return 1;
        if (dateB === zeroDate && dateA !== zeroDate) return -1;
        if (dateA === zeroDate && dateB === zeroDate) return 0;
        return new Date(dateA).getTime() - new Date(dateB).getTime();
      });
  };

  const fetchUpdates = async () => {
    try {
      const [repos, jobs, workersData, reportsData] = await Promise.all([
        apiClient.getRepos(),
        apiClient.getJobs(),
        apiClient.getWorkers(),
        apiClient.getZFSReports().catch(() => []),
      ]);
      setMirrors(buildMirrors(repos, jobs));
      setWorkers(workersData);
      setZfsReports(reportsData);
      setError(null);
    } catch (err) {
      console.warn('Background mirrors refresh failed:', err);
    }
  };

  const forceRefresh = () => {
    fetchUpdates();
  };

  const handleDeleteOrphan = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation();
    if (!confirm(`Delete orphan job "${id}"? Action history will be preserved in the database but the job will be removed from the list.`)) return;
    try {
      await apiClient.deleteJob(id);
      await fetchUpdates();
    } catch (err) {
      console.error('Failed to delete orphan job:', err);
    }
  };

  useEffect(() => {
    const fetchInitial = async () => {
      try {
        setLoading(true);
        const [repos, jobs, workersData, reportsData] = await Promise.all([
          apiClient.getRepos(),
          apiClient.getJobs(),
          apiClient.getWorkers(),
          apiClient.getZFSReports().catch(() => []),
        ]);
        setMirrors(buildMirrors(repos, jobs));
        setWorkers(workersData);
        setZfsReports(reportsData);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch mirrors');
      } finally {
        setLoading(false);
      }
    };

    fetchInitial();
    const interval = setInterval(fetchUpdates, 5000);
    return () => clearInterval(interval);
  }, []);

  if (loading && mirrors.length === 0) {
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

  const filtered = mirrors.filter(m => {
    if (search && !m.id.toLowerCase().includes(search.toLowerCase())) return false;
    if (statusFilter !== 'All') {
      const jobStatus = m.job?.status || '';
      const actionStatus = m.job?.last_action_status || '';
      if (statusFilter === 'Failed') {
        return actionStatus === 'Failed';
      }
      return jobStatus === statusFilter;
    }
    return true;
  });

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, id: string) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onMirrorClick(id);
    }
  };

  const jobsByStatus = mirrors.reduce((acc, m) => {
    const s = m.job?.status || 'Unknown';
    acc[s] = (acc[s] || 0) + 1;
    return acc;
  }, {} as Record<string, number>);

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-bold">Mirrors</h2>
        <span className="text-xs text-muted-foreground font-mono">{mirrors.length} total</span>
      </div>

      <div className="grid gap-3 grid-cols-2 md:grid-cols-5">
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Running</div>
          <div className="text-xl font-bold tabular-nums text-blue-500 mt-1">{jobsByStatus.Running || 0}</div>
        </div>
        <div className="rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Scheduled</div>
          <div className="text-xl font-bold tabular-nums text-purple-500 mt-1">{jobsByStatus.Scheduled || 0}</div>
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
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground">Orphan</div>
          <div className="text-xl font-bold tabular-nums text-muted-foreground mt-1">{jobsByStatus.Orphan || 0}</div>
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
          <option value="Scheduled">Scheduled</option>
          <option value="Paused">Paused</option>
          <option value="Failed">Failed</option>
          <option value="Orphan">Orphan</option>
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
                  <th className="px-3 py-2 text-center">Status</th>
                  <th className="hidden md:table-cell px-3 py-2 text-center">Last Action</th>
                  <th className="hidden lg:table-cell px-2 py-2 text-left">Worker</th>
                  <th className="hidden lg:table-cell px-2 py-2 text-right">Size</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Next Attempt</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Last Attempt</th>
                  <th className="hidden xl:table-cell px-3 py-2 text-left">Last Success</th>
                  <th className="hidden xl:table-cell px-3 py-2 text-left">Last Failure</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {filtered.map(m => {
                  const job = m.job;
                  const lastActionStatus = (job?.last_action_status || '').trim();
                  const matched = m.repo ? workers.filter(w => matchWorker(w, m.repo!)) : [];
                  // Find size from ZFS reports
                  let repoSize = 0;
                  for (const report of zfsReports) {
                    const ds = report.datasets?.find(d => d.repo_id === m.id);
                    if (ds) { repoSize = ds.referenced; break; }
                  }

                  return (
                    <tr
                      key={m.id}
                      onClick={() => onMirrorClick(m.id)}
                      onKeyDown={event => handleRowKeyDown(event, m.id)}
                      tabIndex={0}
                      className="group cursor-pointer bg-background transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60"
                    >
                      <td className="px-3 py-2 align-top">
                        <div className="font-mono text-sm">{m.id}</div>
                        <div className="mt-0.5 text-[11px] text-muted-foreground md:hidden">
                          {job ? (
                            <>
                              <span className="uppercase tracking-wide">Next </span>
                              <RelativeTime date={job.next_attempt_at} variant="compact" />
                            </>
                          ) : (
                            <span>No job</span>
                          )}
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
                      <td className="px-3 py-2 text-center align-top">
                        <div className="flex items-center gap-2">
                          {job ? (
                            <StatusBadge status={job.status} />
                          ) : (
                            <span className="font-mono text-muted-foreground">--</span>
                          )}
                          {job && job.status !== 'Orphan' && (
                            <TriggerButton
                              jobId={job.id}
                              jobStatus={job.status}
                              variant="icon"
                              size="sm"
                              onSuccess={forceRefresh}
                            />
                          )}
                          {job && job.status === 'Orphan' && (
                            <button
                              onClick={e => handleDeleteOrphan(m.id, e)}
                              className="p-1 rounded text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-colors"
                              title="Delete orphan job"
                            >
                              <Trash2 className="h-3.5 w-3.5" />
                            </button>
                          )}
                        </div>
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 text-center align-top">
                        {lastActionStatus ? (
                          <StatusBadge status={lastActionStatus} />
                        ) : (
                          <StatusBadge status="Unknown" />
                        )}
                      </td>
                      <td className="hidden lg:table-cell px-2 py-2 align-top whitespace-nowrap">
                        {matched.length > 0 ? (
                          <div className="flex flex-wrap gap-0.5">
                            {matched.map(w => (
                              <span key={w.name} className={`px-1 py-0 text-[10px] rounded font-mono leading-tight ${w.status === 'Online' ? 'bg-green-500/15 text-green-400' : 'bg-muted text-muted-foreground'}`}>
                                {w.name}
                              </span>
                            ))}
                          </div>
                        ) : (
                          <span className="font-mono text-xs text-muted-foreground">--</span>
                        )}
                      </td>
                      <td className="hidden lg:table-cell px-2 py-2 align-top text-right whitespace-nowrap">
                        <span className="font-mono text-xs text-muted-foreground">
                          {repoSize > 0 ? formatBytes(repoSize) : '--'}
                        </span>
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        {job ? (
                          <RelativeTime date={job.next_attempt_at} variant="compact" />
                        ) : (
                          <span className="font-mono text-muted-foreground">--</span>
                        )}
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        {job ? (
                          <RelativeTime date={job.last_attempt_at} variant="compact" />
                        ) : (
                          <span className="font-mono text-muted-foreground">--</span>
                        )}
                      </td>
                      <td className="hidden lg:table-cell px-3 py-2 align-top">
                        {job ? (
                          <RelativeTime date={job.last_success_at} variant="compact" />
                        ) : (
                          <span className="font-mono text-muted-foreground">--</span>
                        )}
                      </td>
                      <td className="hidden xl:table-cell px-3 py-2 align-top">
                        {job ? (
                          <RelativeTime date={job.last_failure_at} variant="compact" />
                        ) : (
                          <span className="font-mono text-muted-foreground">--</span>
                        )}
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
