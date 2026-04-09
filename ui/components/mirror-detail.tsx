'use client';

import { useState, useEffect } from 'react';
import type { KeyboardEvent } from 'react';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { TriggerButton } from '@/components/trigger-button';
import { apiClient } from '@/lib/api';
import { Job, Repo, Action, ZFSDatasetInfo } from '@/types';
import { ArrowLeft } from 'lucide-react';
import { formatDuration2, formatBytes } from '@/lib/utils';

interface MirrorDetailProps {
  mirrorId: string;
  onBack: () => void;
  onActionClick: (id: string) => void;
}

export function MirrorDetail({ mirrorId, onBack, onActionClick }: MirrorDetailProps) {
  const [repo, setRepo] = useState<Repo | null>(null);
  const [job, setJob] = useState<Job | null>(null);
  const [actions, setActions] = useState<Action[]>([]);
  const [datasetInfo, setDatasetInfo] = useState<ZFSDatasetInfo | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchUpdates = async () => {
    try {
      const [jobs, actionsData] = await Promise.all([
        apiClient.getJobs(),
        apiClient.getActionsByRepo(mirrorId, 50),
      ]);
      const jobData = jobs.find(j => j.id === mirrorId);
      if (jobData) setJob(jobData);
      const sorted = actionsData.sort((a, b) => {
        const aT = new Date(a.started_at || a.created_at || 0).getTime();
        const bT = new Date(b.started_at || b.created_at || 0).getTime();
        return bT - aT;
      });
      setActions(sorted);
      setError(null);
    } catch (err) {
      console.warn('Background mirror detail refresh failed:', err);
    }
    try {
      const reports = await apiClient.getZFSReports();
      for (const report of reports) {
        const ds = report.datasets.find(d => d.repo_id === mirrorId);
        if (ds) { setDatasetInfo(ds); break; }
      }
    } catch { /* ZFS data is optional */ }
  };

  const forceRefresh = () => {
    fetchUpdates();
  };

  useEffect(() => {
    const fetchInitial = async () => {
      try {
        setLoading(true);
        const [repos, jobs, actionsData, zfsReports] = await Promise.all([
          apiClient.getRepos(),
          apiClient.getJobs(),
          apiClient.getActionsByRepo(mirrorId, 50),
          apiClient.getZFSReports().catch(() => []),
        ]);
        setRepo(repos.find(r => r.id === mirrorId) || null);
        setJob(jobs.find(j => j.id === mirrorId) || null);
        const sorted = actionsData.sort((a, b) => {
          const aT = new Date(a.started_at || a.created_at || 0).getTime();
          const bT = new Date(b.started_at || b.created_at || 0).getTime();
          return bT - aT;
        });
        setActions(sorted);
        for (const report of zfsReports) {
          const ds = report.datasets.find((d: { repo_id?: string }) => d.repo_id === mirrorId);
          if (ds) { setDatasetInfo(ds); break; }
        }
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch mirror details');
      } finally {
        setLoading(false);
      }
    };

    fetchInitial();
    const interval = setInterval(fetchUpdates, 5000);
    return () => clearInterval(interval);
  }, [mirrorId]);

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading mirror details...</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <button onClick={onBack} className="flex items-center gap-2 text-muted-foreground hover:text-foreground transition-colors">
          <ArrowLeft className="h-4 w-4" />
          Back to Mirrors
        </button>
        <div className="flex items-center justify-center h-64">
          <div className="text-destructive">Error: {error}</div>
        </div>
      </div>
    );
  }

  const nodeSelector = repo?.sync.nodeSelector;
  const nodeSelectorEntries = nodeSelector ? Object.entries(nodeSelector) : [];

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, id: string) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onActionClick(id);
    }
  };

  return (
    <div className="p-6 space-y-6">
      <button onClick={onBack} className="flex items-center gap-2 text-xs text-muted-foreground hover:text-foreground transition-colors">
        <ArrowLeft className="h-4 w-4" />
        Back to Mirrors
      </button>

      <div className="flex items-center justify-between">
        <h1 className="text-lg font-bold font-mono">{mirrorId}</h1>
        <div className="flex items-center gap-4">
          {job && (
            <>
              <TriggerButton jobId={job.id} jobStatus={job.status} onSuccess={forceRefresh} />
              <StatusBadge status={job.status} />
            </>
          )}
        </div>
      </div>

      {/* Info Section */}
      {repo && (
        <div className="rounded-lg border border-border bg-card p-4 space-y-3">
          <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Configuration</h3>
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 text-sm">
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Upstream</div>
              <div className="font-mono text-sm break-all">{repo.info.upstream || '--'}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Image</div>
              <div className="font-mono text-sm break-all">{repo.sync.image}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Interval</div>
              <div className="font-mono">{repo.sync.interval.value}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Timeout</div>
              <div className="font-mono">{repo.sync.timeout}</div>
            </div>
            {repo.sync.node && (
              <div>
                <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Node</div>
                <div className="font-mono">{repo.sync.node}</div>
              </div>
            )}
            {nodeSelectorEntries.length > 0 && (
              <div>
                <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Node Selector</div>
                <div className="space-y-1">
                  {nodeSelectorEntries.map(([k, v]) => (
                    <div key={k} className="font-mono text-xs">
                      <span className="text-muted-foreground">{k}:</span> {v}
                    </div>
                  ))}
                </div>
              </div>
            )}
          </div>
        </div>
      )}

      {/* Job Status */}
      {job && (
        <div className="rounded-lg border border-border bg-card p-4 space-y-3">
          <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Job Status</h3>
          <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4 text-sm">
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Status</div>
              <StatusBadge status={job.status} />
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
          </div>
        </div>
      )}

      {/* Storage Info (from ZFS) */}
      {datasetInfo && (
        <div className="rounded-lg border border-border bg-card p-4 space-y-3">
          <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Storage (ZFS)</h3>
          <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4 text-sm">
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Dataset</div>
              <div className="font-mono text-xs">{datasetInfo.name}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Used</div>
              <div className="font-mono">{formatBytes(datasetInfo.used)}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Referenced</div>
              <div className="font-mono">{formatBytes(datasetInfo.referenced)}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Available</div>
              <div className="font-mono">{formatBytes(datasetInfo.available)}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Compression</div>
              <div className="font-mono">{datasetInfo.compression} ({datasetInfo.compressratio})</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Logical Used</div>
              <div className="font-mono">{formatBytes(datasetInfo.logicalused)}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Snapshots</div>
              <div className="font-mono">{formatBytes(datasetInfo.usedbysnapshots)}</div>
            </div>
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Written</div>
              <div className="font-mono">{formatBytes(datasetInfo.written)}</div>
            </div>
            {datasetInfo.quota > 0 && (
              <div>
                <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Quota</div>
                <div className="font-mono">{formatBytes(datasetInfo.quota)}</div>
              </div>
            )}
            <div>
              <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Record Size</div>
              <div className="font-mono">{formatBytes(datasetInfo.recordsize)}</div>
            </div>
          </div>
        </div>
      )}

      {/* Action History */}
      <div className="space-y-3">
        <div className="flex items-center justify-between">
          <h3 className="text-lg font-semibold tracking-tight">Action History</h3>
          <span className="text-xs text-muted-foreground font-mono">{actions.length} actions</span>
        </div>

        {actions.length === 0 ? (
          <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
            No actions found for this mirror.
          </div>
        ) : (
          <div className="rounded-lg border border-border bg-card overflow-hidden">
            <div className="overflow-x-auto">
              <table className="w-full text-xs sm:text-sm">
                <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                  <tr>
                    <th className="px-3 py-3 text-left">ID</th>
                    <th className="px-3 py-3 text-center">Status</th>
                    <th className="hidden md:table-cell px-3 py-3 text-left">Worker</th>
                    <th className="hidden md:table-cell px-3 py-3 text-left">Started</th>
                    <th className="hidden lg:table-cell px-3 py-3 text-left">Duration</th>
                    <th className="hidden lg:table-cell px-3 py-3 text-left">Exit Code</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {actions.map(action => {
                    const duration =
                      action.started_at && action.finished_at
                        ? new Date(action.finished_at).getTime() - new Date(action.started_at).getTime()
                        : null;

                    return (
                      <tr
                        key={action.id}
                        onClick={() => onActionClick(action.id)}
                        onKeyDown={event => handleRowKeyDown(event, action.id)}
                        tabIndex={0}
                        className="group cursor-pointer bg-background transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60"
                      >
                        <td className="px-3 py-3 align-top">
                          <div className="font-mono text-xs truncate max-w-[200px]">{action.id}</div>
                          <div className="mt-1 text-[11px] text-muted-foreground md:hidden">
                            <RelativeTime date={action.started_at || action.created_at || ''} variant="compact" />
                          </div>
                        </td>
                        <td className="px-3 py-3 text-center align-top">
                          <StatusBadge status={action.status} />
                        </td>
                        <td className="hidden md:table-cell px-3 py-3 align-top">
                          <span className="font-mono text-xs text-muted-foreground">{action.worker_name || '--'}</span>
                        </td>
                        <td className="hidden md:table-cell px-3 py-3 align-top">
                          {action.started_at ? (
                            <RelativeTime date={action.started_at} variant="compact" />
                          ) : (
                            <span className="font-mono text-muted-foreground text-xs">--</span>
                          )}
                        </td>
                        <td className="hidden lg:table-cell px-3 py-3 align-top">
                          <span className="font-mono text-xs">
                            {duration && duration > 0 ? formatDuration2(duration) : '--'}
                          </span>
                        </td>
                        <td className="hidden lg:table-cell px-3 py-3 align-top">
                          <span className={`font-mono text-xs ${action.container_exit_code !== 0 ? 'text-red-500' : ''}`}>
                            {action.container_exit_code}
                          </span>
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
    </div>
  );
}
