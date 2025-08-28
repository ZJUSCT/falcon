'use client';

import { useState, useEffect } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { Action } from '@/types';
import { Container, Clock, Terminal } from 'lucide-react';
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
        setActiveActions(active);
        setRecentActions(recent);
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
        setActiveActions(active);
        setRecentActions(recent);
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

  const renderActionCard = (action: Action) => (
    <Card
      key={action.id}
      className="hover:shadow-md transition-shadow cursor-pointer"
      onClick={() => onActionClick(action.id)}
    >
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg hover:text-primary transition-colors">
            Action for <span className="font-mono">{action.job_id}</span>
          </CardTitle>
          <StatusBadge status={action.status} />
        </div>
        <CardDescription>
          Updated <RelativeTime date={action.updated_at} />
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4 text-sm">
          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Container className="h-4 w-4 text-muted-foreground" />
              <span className="font-medium">Container</span>
            </div>
            <div className="pl-6">
              <div className="font-mono text-xs">{action.container_name}</div>
              <div className="text-muted-foreground font-mono text-xs">{action.container_image}</div>
            </div>
          </div>

          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Terminal className="h-4 w-4 text-muted-foreground" />
              <span className="font-medium">Status</span>
            </div>
            <div className="pl-6">
              <div className="font-mono text-xs">{action.container_status}</div>
              {action.container_exit_code !== 0 && (
                <div className="text-destructive font-mono text-xs">
                  Exit code: {action.container_exit_code}
                </div>
              )}
              {action.container_exit_code !== 0 && action.container_exit_reason && (
                <div className="text-destructive font-mono text-xs">
                  Exit reason: {action.container_exit_reason}
                </div>
              )}
            </div>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Clock className="h-4 w-4 text-muted-foreground" />
              <span className="font-medium">Timing</span>
            </div>
            <div className="pl-6 text-xs">
              {action.started_at && (
                <div>Started:  <RelativeTime date={action.started_at} /></div>
              )}
              {action.finished_at && (
                <div>Finished: <RelativeTime date={action.finished_at} /></div>
              )}
              {/* duration */}
              {action.started_at && action.finished_at && new Date(action.finished_at).getTime() - new Date(action.started_at).getTime() > 0 && (
                <div>Duration: {formatDuration2(new Date(action.finished_at).getTime() - new Date(action.started_at).getTime())}</div>
              )}
            </div>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Clock className="h-4 w-4 text-muted-foreground" />
              <span className="font-medium">Details</span>
            </div>
            <div className="pl-6">
              <div className="font-mono text-xs">id: {action.id}</div>
              {action.message && (
                <div className="text-muted-foreground mt-1 font-mono text-xs">{action.message}</div>
              )}
              {action.container_name && (
                <div className="text-muted-foreground mt-1 font-mono text-xs">
                  container_name: {action.container_name}
                </div>
              )}
            </div>
          </div>
        </div>
      </CardContent>
    </Card>
  );

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h2 className="text-3xl font-bold tracking-tight">Actions</h2>
        <div className="text-sm text-muted-foreground font-mono">
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

        {activeActions.length === 0 ? (
          <Card>
            <CardContent className="flex items-center justify-center h-24">
              <div className="text-muted-foreground">No currently running actions</div>
            </CardContent>
          </Card>
        ) : (
          <div className="grid gap-4">
            {activeActions.map(renderActionCard)}
          </div>
        )}
      </div>

      {/* Recent Actions Section */}
      <div className="space-y-4">
        <div className="flex items-center justify-between">
          <h3 className="text-xl font-semibold tracking-tight">Recent Activity</h3>
          <div className="text-sm text-muted-foreground font-mono">
            Last {recentActions.length} actions
          </div>
        </div>

        {recentActions.length === 0 ? (
          <Card>
            <CardContent className="flex items-center justify-center h-24">
              <div className="text-muted-foreground">No recent actions found</div>
            </CardContent>
          </Card>
        ) : (
          <div className="grid gap-4">
            {recentActions.map(renderActionCard)}
          </div>
        )}
      </div>
    </div>
  );
}
