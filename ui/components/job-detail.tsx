'use client';

import { useState, useEffect } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { TriggerButton } from '@/components/trigger-button';
import { apiClient } from '@/lib/api';
import { Job, Action, Repo } from '@/types';
import { ArrowLeft, Container, Clock, Terminal, Globe, HardDrive, Settings } from 'lucide-react';
import { formatDuration2 } from '@/lib/utils';

interface JobDetailProps {
  jobId: string;
  onBack: () => void;
  onActionClick: (actionId: string) => void;
}

export function JobDetail({ jobId, onBack, onActionClick }: JobDetailProps) {
  const [job, setJob] = useState<Job | null>(null);
  const [repo, setRepo] = useState<Repo | null>(null);
  const [actions, setActions] = useState<Action[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchUpdates = async () => {
    try {
      // Only refresh job status and actions, not repo config
      const [jobs, actionsData] = await Promise.all([
        apiClient.getJobs(),
        apiClient.getActionsByRepo(jobId, 10)
      ]);
      
      const jobData = jobs.find(j => j.id === jobId);
      if (jobData) {
        setJob(jobData);
      }
      
      // Sort actions by started_at descending (most recent first)
      const sortedActions = actionsData.sort((a, b) => {
        const aTime = new Date(a.started_at || a.created_at || 0).getTime();
        const bTime = new Date(b.started_at || b.created_at || 0).getTime();
        return bTime - aTime; // Descending order (newest first)
      });
      setActions(sortedActions);
      setError(null);
    } catch (err) {
      // Don't update error state for background refresh failures
      console.warn('Background refresh failed:', err);
    }
  };

  // Function to force immediate refresh (for trigger button)
  const forceRefresh = () => {
    fetchUpdates();
  };

  useEffect(() => {
    const fetchInitialData = async () => {
      try {
        setLoading(true);
        
        // Fetch jobs and find the specific job
        const jobs = await apiClient.getJobs();
        const jobData = jobs.find(j => j.id === jobId);
        
        if (!jobData) {
          setError('Job not found');
          return;
        }
        
        setJob(jobData);
        
        // Fetch repos and find the corresponding repo (only once)
        const repos = await apiClient.getRepos();
        const repoData = repos.find(r => r.id === jobId);
        setRepo(repoData || null);
        
        // Fetch last 10 actions for this job
        const actionsData = await apiClient.getActionsByRepo(jobId, 10);
        // Sort actions by started_at descending (most recent first)
        const sortedActions = actionsData.sort((a, b) => {
          const aTime = new Date(a.started_at || a.created_at || 0).getTime();
          const bTime = new Date(b.started_at || b.created_at || 0).getTime();
          return bTime - aTime; // Descending order (newest first)
        });
        setActions(sortedActions);
        
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch job details');
      } finally {
        setLoading(false);
      }
    };

    // Initial load with loading state
    fetchInitialData();
    
    // Background updates without loading state
    const interval = setInterval(fetchUpdates, 3000); // Refresh every 3 seconds

    return () => clearInterval(interval);
  }, [jobId]);

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading job details...</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <button
          onClick={onBack}
          className="flex items-center gap-2 text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="h-4 w-4" />
          Back to Jobs
        </button>
        <div className="flex items-center justify-center h-64">
          <div className="text-destructive">Error: {error}</div>
        </div>
      </div>
    );
  }

  if (!job) {
    return (
      <div className="space-y-4">
        <button
          onClick={onBack}
          className="flex items-center gap-2 text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="h-4 w-4" />
          Back to Jobs
        </button>
        <div className="flex items-center justify-center h-64">
          <div className="text-muted-foreground">Job not found</div>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <button
          onClick={onBack}
          className="flex items-center gap-2 text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="h-4 w-4" />
          Back to Jobs
        </button>
      </div>

      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-4xl font-bold tracking-tight font-mono">{job.id}</h1>
        </div>
        <div className="flex items-center gap-4">
          <TriggerButton 
            jobId={job.id} 
            jobStatus={job.status} 
            onSuccess={forceRefresh}
          />
          <StatusBadge status={job.status} />
        </div>
      </div>

      <div className="grid gap-6 md:grid-cols-2">
        {/* Job Status Information */}
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <Clock className="h-5 w-5" />
              <CardTitle>Job Status</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-2 gap-4 text-sm">
              <div>
                <div className="font-medium text-muted-foreground">Status</div>
                <div><StatusBadge status={job.status} /></div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Updated</div>
                <div><RelativeTime date={job.updated_at} /></div>
              </div>
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

        {/* Repository Configuration */}
        {repo && (
          <Card>
            <CardHeader>
              <div className="flex items-center gap-2">
                <Settings className="h-5 w-5" />
                <CardTitle>Repository Configuration</CardTitle>
              </div>
            </CardHeader>
            <CardContent className="space-y-4">
              <div>
                <div className="font-medium text-muted-foreground">Name</div>
                <div className="font-mono">{repo.info.name.en || repo.info.name.zh || 'N/A'}</div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Description</div>
                <div className="text-sm">{repo.info.description.en || repo.info.description.zh || 'No description'}</div>
              </div>
              <div className="space-y-2">
                <div className="flex items-center gap-2">
                  <Globe className="h-4 w-4 text-muted-foreground" />
                  <span className="font-medium text-muted-foreground">Upstream</span>
                </div>
                <div className="font-mono text-sm break-all">{repo.info.upstream}</div>
              </div>
              <div className="space-y-2">
                <div className="flex items-center gap-2">
                  <HardDrive className="h-4 w-4 text-muted-foreground" />
                  <span className="font-medium text-muted-foreground">Docker Image</span>
                </div>
                <div className="font-mono text-sm">{repo.sync.image}</div>
              </div>
              <div className="grid grid-cols-2 gap-4 text-sm">
                <div>
                  <div className="font-medium text-muted-foreground">Sync Interval</div>
                  <div className="font-mono">{repo.sync.interval.value}</div>
                </div>
                <div>
                  <div className="font-medium text-muted-foreground">Timeout</div>
                  <div className="font-mono">{repo.sync.timeout}</div>
                </div>
              </div>
            </CardContent>
          </Card>
        )}
      </div>

      {/* Recent Actions */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <Container className="h-5 w-5" />
              <CardTitle>Recent Actions</CardTitle>
            </div>
            <div className="text-sm text-muted-foreground font-mono">
              {actions.length} actions shown
            </div>
          </div>
          <CardDescription>
            Last 10 sync attempts for this repository
          </CardDescription>
        </CardHeader>
        <CardContent>
          {actions.length === 0 ? (
            <div className="flex items-center justify-center h-32 text-muted-foreground">
              No actions found
            </div>
          ) : (
            <div className="space-y-4">
              {actions.map((action) => (
                <div 
                  key={action.id} 
                  className="border rounded-lg p-4 space-y-3 hover:bg-muted/50 cursor-pointer transition-colors"
                  onClick={() => onActionClick(action.id)}
                >
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-2">
                      <StatusBadge status={action.status} />
                      <span className="font-mono text-sm text-muted-foreground hover:text-primary transition-colors">{action.id}</span>
                    </div>
                  </div>
                  <div className="grid grid-cols-1 md:grid-cols-3 gap-4 text-sm">
                    <div className="space-y-1">
                      <div className="flex items-center gap-2">
                        <Container className="h-4 w-4 text-muted-foreground" />
                        <span className="font-medium text-muted-foreground">Container</span>
                      </div>
                      <div className="pl-6">
                        <div className="font-mono text-xs">{action.container_name}</div>
                        <div className="text-muted-foreground font-mono text-xs">{action.container_image}</div>
                      </div>
                    </div>
                    
                    <div className="space-y-1">
                      <div className="flex items-center gap-2">
                        <Terminal className="h-4 w-4 text-muted-foreground" />
                        <span className="font-medium text-muted-foreground">Exit Status</span>
                      </div>
                      <div className="pl-6">
                        <div className="font-mono text-xs">{action.container_status}</div>
                        {action.container_exit_code !== 0 && (
                          <div className="text-destructive font-mono text-xs">
                            Exit code: {action.container_exit_code}
                          </div>
                        )}
                      </div>
                    </div>

                    <div className="space-y-1">
                      <div className="flex items-center gap-2">
                        <Clock className="h-4 w-4 text-muted-foreground" />
                        <span className="font-medium text-muted-foreground">Timing</span>
                      </div>
                      <div className="pl-6 text-xs">
                        {action.started_at && (
                          <div>Started: <RelativeTime date={action.started_at} /></div>
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
                  </div>

                  {action.message && (
                    <div className="pt-2 border-t">
                      <div className="text-sm text-muted-foreground font-mono">{action.message}</div>
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
