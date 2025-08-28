'use client';

import { useState, useEffect } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { Action, Job, Volume } from '@/types';
import { LogViewer } from '@/components/log-viewer';
import { ArrowLeft, Container, Clock, Terminal, Code, HardDrive, Settings, AlertCircle } from 'lucide-react';
import { formatDuration2 } from '@/lib/utils';

interface ActionDetailProps {
  actionId: string;
  onBack: () => void;
  onJobClick: (jobId: string) => void;
}

export function ActionDetail({ actionId, onBack, onJobClick }: ActionDetailProps) {
  const [action, setAction] = useState<Action | null>(null);
  const [job, setJob] = useState<Job | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchActionDetail = async () => {
    try {
      // Fetch the specific action by ID
      const actions = await apiClient.getActionsByIds([actionId]);
      if (actions.length === 0) {
        setError('Action not found');
        return;
      }
      
      const actionData = actions[0];
      setAction(actionData);
      
      // Also fetch the related job for context
      const jobs = await apiClient.getJobs();
      const jobData = jobs.find(j => j.id === actionData.job_id);
      setJob(jobData || null);
      
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch action details');
    }
  };

  useEffect(() => {
    const fetchInitialData = async () => {
      try {
        setLoading(true);
        await fetchActionDetail();
      } finally {
        setLoading(false);
      }
    };

    // Initial load with loading state
    fetchInitialData();
    
    // Background updates every 5 seconds for running actions
    const interval = setInterval(fetchActionDetail, 5000);

    return () => clearInterval(interval);
  }, [actionId]);

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading action details...</div>
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
          Back
        </button>
        <div className="flex items-center justify-center h-64">
          <div className="text-destructive">Error: {error}</div>
        </div>
      </div>
    );
  }

  if (!action) {
    return (
      <div className="space-y-4">
        <button
          onClick={onBack}
          className="flex items-center gap-2 text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="h-4 w-4" />
          Back
        </button>
        <div className="flex items-center justify-center h-64">
          <div className="text-muted-foreground">Action not found</div>
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
          Back
        </button>
      </div>

      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-3xl font-bold tracking-tight font-mono">{action.id}</h1>
          <p className="text-muted-foreground">
            Action details for job <span className="font-mono font-bold">{action.job_id}</span>
          </p>
        </div>
      </div>

      <div className="grid gap-6 md:grid-cols-2">
        {/* Action Status Information */}
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <Clock className="h-5 w-5" />
              <CardTitle>Action Status</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-1 gap-4 text-sm font-mono">
              <div>
                <div className="font-medium text-muted-foreground">Status</div>
                <div><StatusBadge status={action.status} /></div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Message</div>
                <div className="font-mono text-sm break-words">
                  {action.message || 'No message'}
                </div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Updated</div>
                <div><RelativeTime date={action.updated_at} /></div>
              </div>
              {action.created_at && (
                <div>
                  <div className="font-medium text-muted-foreground">Created</div>
                  <div><RelativeTime date={action.created_at} /></div>
                </div>
              )}
              {action.started_at && (
                <div>
                  <div className="font-medium text-muted-foreground">Started</div>
                  <div><RelativeTime date={action.started_at} /></div>
                </div>
              )}
              {action.finished_at && (
                <div>
                  <div className="font-medium text-muted-foreground">Finished</div>
                  <div><RelativeTime date={action.finished_at} /></div>
                </div>
              )}
              {action.started_at && action.finished_at && new Date(action.finished_at).getTime() - new Date(action.started_at).getTime() > 0 && (
                <div>
                  <div className="font-medium text-muted-foreground">Duration</div>
                  <div>{formatDuration2(new Date(action.finished_at).getTime() - new Date(action.started_at).getTime())}</div>
                </div>
              )}
            </div>
          </CardContent>
        </Card>

        {/* Container Information */}
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <Container className="h-5 w-5" />
              <CardTitle>Container Details</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-1 gap-4 text-sm">
              <div>
                <div className="font-medium text-muted-foreground">Container ID</div>
                <div className="font-mono text-xs break-all">{action.container_id}</div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Container Name</div>
                <div className="font-mono">{action.container_name}</div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Image</div>
                <div className="font-mono text-sm break-words">{action.container_image}</div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Container Status</div>
                <div className="font-mono">{action.container_status}</div>
              </div>
              {action.container_timeout && (
                <div>
                  <div className="font-medium text-muted-foreground">Timeout</div>
                  <div className="font-mono">{action.container_timeout}</div>
                </div>
              )}
            </div>
          </CardContent>
        </Card>
      </div>

      {/* Exit Information */}
      {(action.container_exit_code !== 0 || action.container_exit_signal !== 0 || action.container_exit_reason) && (
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <AlertCircle className="h-5 w-5" />
              <CardTitle>Exit Information</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-1 md:grid-cols-3 gap-4 text-sm">
              <div>
                <div className="font-medium text-muted-foreground">Exit Code</div>
                <div className={`font-mono ${action.container_exit_code !== 0 ? 'text-red-600' : 'text-green-600'}`}>
                  {action.container_exit_code}
                </div>
              </div>
              {action.container_exit_signal !== 0 && (
                <div>
                  <div className="font-medium text-muted-foreground">Exit Signal</div>
                  <div className="font-mono text-orange-600">{action.container_exit_signal}</div>
                </div>
              )}
              {action.container_exit_reason && (
                <div>
                  <div className="font-medium text-muted-foreground">Exit Reason</div>
                  <div className="font-mono">{action.container_exit_reason}</div>
                </div>
              )}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Container Configuration */}
      {(action.container_command || action.container_env || action.container_volumes) && (
        <div className="grid gap-6 md:grid-cols-2 lg:grid-cols-3">
          {/* Command */}
          {action.container_command && action.container_command.length > 0 && (
            <Card>
              <CardHeader>
                <div className="flex items-center gap-2">
                  <Terminal className="h-5 w-5" />
                  <CardTitle>Command</CardTitle>
                </div>
              </CardHeader>
              <CardContent>
                <div className="space-y-2">
                  {action.container_command.map((cmd, index) => (
                    <div key={index} className="font-mono text-sm bg-muted p-2 rounded">
                      {cmd}
                    </div>
                  ))}
                </div>
              </CardContent>
            </Card>
          )}

          {/* Environment Variables */}
          {action.container_env && action.container_env.length > 0 && (
            <Card>
              <CardHeader>
                <div className="flex items-center gap-2">
                  <Settings className="h-5 w-5" />
                  <CardTitle>Environment</CardTitle>
                </div>
              </CardHeader>
              <CardContent>
                <div className="space-y-2 max-h-64 overflow-y-auto">
                  {action.container_env.map((env, index) => (
                    <div key={index} className="font-mono text-xs bg-muted p-2 rounded break-all">
                      {env}
                    </div>
                  ))}
                </div>
              </CardContent>
            </Card>
          )}

          {/* Volumes */}
          {action.container_volumes && action.container_volumes.length > 0 && (
            <Card>
              <CardHeader>
                <div className="flex items-center gap-2">
                  <HardDrive className="h-5 w-5" />
                  <CardTitle>Volumes</CardTitle>
                </div>
              </CardHeader>
              <CardContent>
                <div className="space-y-2">
                  {action.container_volumes.map((volume, index) => (
                    typeof volume === 'string' ? (
                      <div key={index} className="font-mono text-xs bg-muted p-2 rounded break-all">
                        {volume}
                      </div>
                    ) : (
                      <div key={index} className="font-mono text-xs bg-muted p-2 rounded break-all">
                        <div>
                          <span className="font-semibold">Src:</span> {volume.src}
                        </div>
                        <div>
                          <span className="font-semibold">Dst:</span> {volume.dst}
                        </div>
                      </div>
                    )
                  ))}
                </div>
              </CardContent>
            </Card>
          )}
        </div>
      )}

      {/* Related Job Information */}
      {job && (
        <Card 
          className="hover:shadow-md transition-shadow cursor-pointer"
          onClick={() => onJobClick(job.id)}
        >
          <CardHeader>
            <div className="flex items-center gap-2">
              <Code className="h-5 w-5" />
              <CardTitle className="hover:text-primary transition-colors">Related Job</CardTitle>
            </div>
            <CardDescription>
              Information about the job that created this action
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-2 md:grid-cols-5 gap-5 text-sm">
              <div>
                <div className="font-medium text-muted-foreground">Job ID</div>
                <div className="font-mono hover:text-primary transition-colors">{job.id}</div>
              </div>
              <div>
                <div className="font-medium text-muted-foreground">Job Status</div>
                <div><StatusBadge status={job.status} /></div>
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
                <div className="font-medium text-muted-foreground">Next Attempt</div>
                <div><RelativeTime date={job.next_attempt_at} /></div>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Log Viewer */}
      <LogViewer actionId={action.id} />
    </div>
  );
}
