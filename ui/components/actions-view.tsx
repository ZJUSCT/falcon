'use client';

import { useState, useEffect } from 'react';
import type { KeyboardEvent } from 'react';
import { Card, CardContent } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { Action } from '@/types';
import { formatDuration2 } from '@/lib/utils';

interface ActionsViewProps {
  onActionClick: (actionId: string) => void;
}

export function ActionsView({ onActionClick }: ActionsViewProps) {
  const [activeActions, setActiveActions] = useState<Action[]>([]);
  const [recentActions, setRecentActions] = useState<Action[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const fetchInitialActions = async () => {
      try {
        setLoading(true);
        const [active, recent] = await Promise.all([
          apiClient.getActions(),
          apiClient.getRecentActions(20)
        ]);
        
        // Sort actions by started_at descending (most recent first)
        const sortedActive = active.sort((a, b) => {
          const aTime = new Date(a.started_at || a.created_at || 0).getTime();
          const bTime = new Date(b.started_at || b.created_at || 0).getTime();
          return bTime - aTime; // Descending order (newest first)
        });
        const sortedRecent = recent.sort((a, b) => {
          const aTime = new Date(a.started_at || a.created_at || 0).getTime();
          const bTime = new Date(b.started_at || b.created_at || 0).getTime();
          return bTime - aTime; // Descending order (newest first)
        });
        
        setActiveActions(sortedActive);
        setRecentActions(sortedRecent);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch actions');
      } finally {
        setLoading(false);
      }
    };

    const fetchActionUpdates = async () => {
      try {
        const [active, recent] = await Promise.all([
          apiClient.getActions(),
          apiClient.getRecentActions(20)
        ]);
        
        // Sort actions by started_at descending (most recent first)
        const sortedActive = active.sort((a, b) => {
          const aTime = new Date(a.started_at || a.created_at || 0).getTime();
          const bTime = new Date(b.started_at || b.created_at || 0).getTime();
          return bTime - aTime; // Descending order (newest first)
        });
        const sortedRecent = recent.sort((a, b) => {
          const aTime = new Date(a.started_at || a.created_at || 0).getTime();
          const bTime = new Date(b.started_at || b.created_at || 0).getTime();
          return bTime - aTime; // Descending order (newest first)
        });
        
        setActiveActions(sortedActive);
        setRecentActions(sortedRecent);
        setError(null);
      } catch (err) {
        // Don't update error state for background refresh failures
        console.warn('Background actions refresh failed:', err);
      }
    };

    // Initial load with loading state
    fetchInitialActions();

    // Background updates without loading state
    const interval = setInterval(fetchActionUpdates, 2000); // Refresh every 2 seconds

    return () => clearInterval(interval);
  }, []);

  if (loading && activeActions.length === 0 && recentActions.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading actions...</div>
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

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, actionId: string) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onActionClick(actionId);
    }
  };

  const getDurationLabel = (action: Action) => {
    if (!action.started_at) {
      return '—';
    }

    const start = new Date(action.started_at).getTime();
    if (Number.isNaN(start)) {
      return '—';
    }

    const end = action.finished_at ? new Date(action.finished_at).getTime() : Date.now();
    if (Number.isNaN(end) || end <= start) {
      return '—';
    }

    return formatDuration2(end - start);
  };

  const renderActionsTable = (actions: Action[], emptyLabel: string) => {
    if (actions.length === 0) {
      return (
        <Card>
          <CardContent className="p-6 text-sm text-muted-foreground">
            {emptyLabel}
          </CardContent>
        </Card>
      );
    }

    return (
      <Card className="overflow-hidden">
        <CardContent className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-xs sm:text-sm">
              <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                <tr>
                  <th className="px-3 py-2 text-left">Action</th>
                  <th className="px-3 py-2 text-left">Status</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Worker</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Updated</th>
                  <th className="hidden lg:table-cell px-3 py-2 text-left">Timing</th>
                  <th className="hidden xl:table-cell px-3 py-2 text-left">Container</th>
                  <th className="hidden 2xl:table-cell px-3 py-2 text-left">Message</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {actions.map((action) => (
                  <tr
                    key={action.id}
                    onClick={() => onActionClick(action.id)}
                    onKeyDown={(event) => handleRowKeyDown(event, action.id)}
                    tabIndex={0}
                    className="group cursor-pointer bg-background transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60"
                  >
                    <td className="px-3 py-2 align-top">
                      <div className="font-mono text-sm sm:text-base">{action.job_id}</div>
                      <div className="text-[11px] text-muted-foreground font-mono truncate">{action.id}</div>
                      <div className="mt-1 text-[11px] text-muted-foreground md:hidden">
                        <span className="uppercase tracking-wide">Updated </span>
                        <RelativeTime date={action.updated_at} variant="compact" />
                      </div>
                    </td>
                    <td className="px-3 py-2 align-top">
                      <div className="flex flex-wrap items-center gap-2">
                        <StatusBadge status={action.status} />
                        <span className="font-mono text-[11px] text-muted-foreground">{action.container_status}</span>
                      </div>
                      {action.container_exit_code !== 0 && (
                        <div className="mt-1 text-[11px] font-mono text-destructive">
                          exit {action.container_exit_code}{action.container_exit_reason ? ` • ${action.container_exit_reason}` : ''}
                        </div>
                      )}
                    </td>
                    <td className="hidden md:table-cell px-3 py-2 align-top">
                      <span className="font-mono text-xs text-muted-foreground">{action.worker_name || '\u2014'}</span>
                    </td>
                    <td className="hidden md:table-cell px-3 py-2 align-top">
                      <RelativeTime date={action.updated_at} variant="compact" />
                    </td>
                    <td className="hidden lg:table-cell px-3 py-2 align-top text-[11px] leading-relaxed text-muted-foreground">
                      {action.started_at ? (
                        <div>
                          <span className="uppercase tracking-wide">Started </span>
                          <RelativeTime date={action.started_at} variant="compact" />
                        </div>
                      ) : (
                        <div className="uppercase tracking-wide">No start</div>
                      )}
                      {action.finished_at ? (
                        <div>
                          <span className="uppercase tracking-wide">Finished </span>
                          <RelativeTime date={action.finished_at} variant="compact" />
                        </div>
                      ) : action.started_at ? (
                        <div className="uppercase tracking-wide">Running…</div>
                      ) : null}
                      <div>
                        <span className="uppercase tracking-wide">Duration </span>
                        <span className="font-mono text-muted-foreground">{getDurationLabel(action)}</span>
                      </div>
                    </td>
                    <td className="hidden xl:table-cell px-3 py-2 align-top text-[11px] text-muted-foreground">
                      <div className="font-mono text-xs text-foreground truncate">{action.container_name || '—'}</div>
                      <div className="font-mono truncate">{action.container_image}</div>
                    </td>
                    <td className="hidden 2xl:table-cell px-3 py-2 align-top">
                      <div className="font-mono text-xs text-muted-foreground truncate">
                        {action.message || '—'}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </CardContent>
      </Card>
    );
  };

  return (
    <div className="space-y-4 sm:space-y-6">
      <div className="flex items-center justify-between">
        <h2 className="text-2xl sm:text-3xl font-bold tracking-tight">Actions</h2>
        <div className="text-xs sm:text-sm text-muted-foreground font-mono">
          {activeActions.length} active • {recentActions.length} recent
        </div>
      </div>

      {/* Active Actions Section */}
      <div className="space-y-4">
        <div className="flex items-center justify-between">
          <h3 className="text-xl font-semibold tracking-tight">Currently Running</h3>
          <div className="text-sm text-muted-foreground font-mono">
            {activeActions.length} active actions
          </div>
        </div>

        {renderActionsTable(activeActions, 'No currently running actions')}
      </div>

      {/* Recent Actions Section */}
      <div className="space-y-4">
        <div className="flex items-center justify-between">
          <h3 className="text-xl font-semibold tracking-tight">Recent Activity</h3>
          <div className="text-sm text-muted-foreground font-mono">
            Last {recentActions.length} actions
          </div>
        </div>

        {renderActionsTable(recentActions, 'No recent actions found')}
      </div>
    </div>
  );
}
