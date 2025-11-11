'use client';

import { useState, useEffect } from 'react';
import type { KeyboardEvent } from 'react';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { TriggerButton } from '@/components/trigger-button';
import { apiClient } from '@/lib/api';
import { Job } from '@/types';
import { Clock, CheckCircle, XCircle, AlertCircle } from 'lucide-react';

interface JobsViewProps {
  onJobClick: (jobId: string) => void;
}

export function JobsView({ onJobClick }: JobsViewProps) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const sortJobs = (data: Job[]) => {
    return data.sort((a, b) => {
      // Status priority: Running > Scheduled > Waiting > Orphan
      const statusPriority = {
        'Running': 0,
        'Scheduled': 1,
        'Waiting': 2,
        'Orphan': 3
      } as const;
      
      const priorityA = statusPriority[a.status as keyof typeof statusPriority] ?? 4;
      const priorityB = statusPriority[b.status as keyof typeof statusPriority] ?? 4;
      
      // First sort by status priority
      if (priorityA !== priorityB) {
        return priorityA - priorityB;
      }
      
      // Within same status, sort by next_attempt_at (earliest first)
      const dateA = new Date(a.next_attempt_at);
      const dateB = new Date(b.next_attempt_at);
      
      // Handle "Never" cases (0001-01-01T00:00:00Z) - put them at the end
      if (a.next_attempt_at === '0001-01-01T00:00:00Z' && b.next_attempt_at !== '0001-01-01T00:00:00Z') {
        return 1;
      }
      if (b.next_attempt_at === '0001-01-01T00:00:00Z' && a.next_attempt_at !== '0001-01-01T00:00:00Z') {
        return -1;
      }
      if (a.next_attempt_at === '0001-01-01T00:00:00Z' && b.next_attempt_at === '0001-01-01T00:00:00Z') {
        return 0;
      }
      
      return dateA.getTime() - dateB.getTime();
    });
  };

  const fetchJobUpdates = async () => {
    try {
      const data = await apiClient.getJobs();
      const sortedJobs = sortJobs(data);
      setJobs(sortedJobs);
      setError(null);
    } catch (err) {
      // Don't update error state for background refresh failures
      console.warn('Background jobs refresh failed:', err);
    }
  };

  // Function to force immediate refresh (for trigger button)
  const forceRefresh = () => {
    fetchJobUpdates();
  };

  useEffect(() => {
    const fetchInitialJobs = async () => {
      try {
        setLoading(true);
        const data = await apiClient.getJobs();
        const sortedJobs = sortJobs(data);
        setJobs(sortedJobs);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch jobs');
      } finally {
        setLoading(false);
      }
    };

    // Initial load with loading state
    fetchInitialJobs();
    
    // Background updates without loading state
    const interval = setInterval(fetchJobUpdates, 3000); // Refresh every 3 seconds

    return () => clearInterval(interval);
  }, []);

  if (loading && jobs.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading jobs...</div>
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

  const jobsByStatus = jobs.reduce((acc, job) => {
    acc[job.status] = (acc[job.status] || 0) + 1;
    return acc;
  }, {} as Record<string, number>);

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, jobId: string) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onJobClick(jobId);
    }
  };

  return (
    <div className="space-y-3 sm:space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-2xl sm:text-3xl font-bold tracking-tight">Jobs</h2>
        <div className="text-xs sm:text-sm text-muted-foreground font-mono">
          {jobs.length} total jobs
        </div>
      </div>

      <div className="grid gap-3 sm:gap-4 md:grid-cols-2 xl:grid-cols-4">
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Running</CardTitle>
            <AlertCircle className="h-4 w-4 text-blue-600" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold font-mono">{jobsByStatus.Running || 0}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Waiting</CardTitle>
            <Clock className="h-4 w-4 text-yellow-600" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold font-mono">{jobsByStatus.Waiting || 0}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Scheduled</CardTitle>
            <CheckCircle className="h-4 w-4 text-purple-600" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold font-mono">{jobsByStatus.Scheduled || 0}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">Orphan</CardTitle>
            <XCircle className="h-4 w-4 text-gray-600" />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold font-mono">{jobsByStatus.Orphan || 0}</div>
          </CardContent>
        </Card>
      </div>
      
      {jobs.length === 0 ? (
        <Card>
          <CardContent className="p-6 text-sm text-muted-foreground">
            No jobs available.
          </CardContent>
        </Card>
      ) : (
        <Card className="overflow-hidden">
          <CardContent className="p-0">
            <div className="overflow-x-auto">
              <table className="w-full text-xs sm:text-sm">
                <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                  <tr>
                    <th className="px-3 py-2 text-center">Job</th>
                    <th className="px-3 py-2 text-center">Status</th>
                    <th className="hidden md:table-cell px-3 py-2 text-center">Last Action</th>
                    <th className="hidden md:table-cell px-3 py-2 text-left">Next Attempt</th>
                    <th className="hidden md:table-cell px-3 py-2 text-left">Last Attempt</th>
                    <th className="hidden lg:table-cell px-3 py-2 text-left">Last Success</th>
                    <th className="hidden xl:table-cell px-3 py-2 text-left">Last Failure</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {jobs.map((job) => {
                    const latestStatus = (job.last_action_status || '').trim();
                    const hasLatestStatus = latestStatus.length > 0;
                    return (
                    <tr
                      key={job.id}
                      onClick={() => onJobClick(job.id)}
                      onKeyDown={(event) => handleRowKeyDown(event, job.id)}
                      tabIndex={0}
                      className="group cursor-pointer bg-background transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60"
                    >
                      <td className="px-3 py-2 align-top">
                        <div className="font-mono text-sm sm:text-base">{job.id}</div>
                        <div className="mt-1 text-[11px] text-muted-foreground md:hidden">
                          <span className="uppercase tracking-wide">Updated </span>
                          <RelativeTime date={job.updated_at} variant="compact" />
                        </div>
                        <div className="mt-1 text-[11px] text-muted-foreground md:hidden">
                          <span className="uppercase tracking-wide">Next </span>
                          <RelativeTime date={job.next_attempt_at} variant="compact" />
                        </div>
                        <div className="mt-1 flex items-center gap-1 text-[11px] text-muted-foreground md:hidden">
                          <span className="uppercase tracking-wide">Last Action</span>
                          {hasLatestStatus ? (
                            <StatusBadge status={latestStatus} />
                          ) : (
                            <span className="font-mono text-muted-foreground">—</span>
                          )}
                        </div>
                      </td>
                      <td className="px-3 text-center py-2 align-top">
                        <div className="flex items-center gap-2">
                          <StatusBadge status={job.status} />
                          <TriggerButton
                            jobId={job.id}
                            jobStatus={job.status}
                            variant="icon"
                            size="sm"
                            onSuccess={forceRefresh}
                          />
                        </div>
                      </td>
                      <td className="hidden text-center md:table-cell px-3 py-2 align-top">
                        {hasLatestStatus ? (
                          <StatusBadge status={latestStatus} />
                        ) : (
                          <StatusBadge status="Unknown" />
                        )}
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.next_attempt_at} variant="compact" />
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.last_attempt_at} variant="compact" />
                      </td>
                      <td className="hidden lg:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.last_success_at} variant="compact" />
                      </td>
                      <td className="hidden xl:table-cell px-3 py-2 align-top">
                        <RelativeTime date={job.last_failure_at} variant="compact" />
                      </td>
                    </tr>
                  );
                  })}
                </tbody>
              </table>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
