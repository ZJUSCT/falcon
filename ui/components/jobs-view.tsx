'use client';

import { useState, useEffect } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
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

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-3xl font-bold tracking-tight">Jobs</h2>
        <div className="text-sm text-muted-foreground font-mono">
          {jobs.length} total jobs
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
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
      
      <div className="grid gap-4">
        {jobs.map((job) => (
          <Card 
            key={job.id} 
            className="hover:shadow-md transition-shadow cursor-pointer"
            onClick={() => onJobClick(job.id)}
          >
            <CardHeader className="pb-2">
              <div className="flex items-center justify-between">
                <CardTitle className="text-lg font-mono hover:text-primary transition-colors">{job.id}</CardTitle>
                <div className="flex items-center gap-2">
                  <TriggerButton 
                    jobId={job.id} 
                    jobStatus={job.status} 
                    variant="icon" 
                    size="sm"
                    onSuccess={forceRefresh}
                  />
                  <StatusBadge status={job.status} />
                </div>
              </div>
              <CardDescription>
                Updated <RelativeTime date={job.updated_at} />
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
                <div>
                  <div className="font-medium text-muted-foreground">Last Success</div>
                  <div><RelativeTime date={job.last_success_at} /></div>
                </div>
                <div>
                  <div className="font-medium text-muted-foreground">Last Failure</div>
                  <div><RelativeTime date={job.last_failure_at} /></div>
                </div>
                <div>
                  <div className="font-medium text-muted-foreground">Last Attempt</div>
                  <div><RelativeTime date={job.last_attempt_at} /></div>
                </div>
                <div>
                  <div className="font-medium text-muted-foreground">Next Attempt</div>
                  <div><RelativeTime date={job.next_attempt_at} /></div>
                </div>
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
