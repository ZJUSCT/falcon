'use client';

import { useState, useEffect } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { QueueControls } from '@/components/queue-controls';
import { QueueJobControls } from '@/components/queue-job-controls';
import { apiClient } from '@/lib/api';
import { QueueItem, QueueResponse } from '@/types';
import { Clock, List, Pause } from 'lucide-react';

export function QueueView() {
  const [queueData, setQueueData] = useState<QueueResponse>({ paused: false, max_concurrency: 1, queue: [] });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchQueueUpdates = async () => {
    try {
      const data = await apiClient.getQueue();
      setQueueData(data);
      setError(null);
    } catch (err) {
      // Don't update error state for background refresh failures
      console.warn('Background queue refresh failed:', err);
    }
  };

  // Function to force immediate refresh (for queue controls)
  const forceRefresh = () => {
    fetchQueueUpdates();
  };

  useEffect(() => {
    const fetchInitialQueue = async () => {
      try {
        setLoading(true);
        const data = await apiClient.getQueue();
        setQueueData(data);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch queue');
      } finally {
        setLoading(false);
      }
    };

    // Initial load with loading state
    fetchInitialQueue();
    
    // Background updates without loading state
    const interval = setInterval(fetchQueueUpdates, 2000); // Refresh every 2 seconds

    return () => clearInterval(interval);
  }, []);

  if (loading && queueData.queue.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading queue...</div>
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

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <h2 className="text-3xl font-bold tracking-tight">Job Queue</h2>
          {queueData.paused && (
            <div className="flex items-center gap-1 px-2 py-1 text-sm bg-yellow-100 text-yellow-800 border border-yellow-200 rounded-md">
              <Pause className="h-4 w-4" />
              Paused
            </div>
          )}
          <div className="flex items-center gap-1 px-2 py-1 text-sm bg-blue-100 text-blue-800 border border-blue-200 rounded-md">
            <span className="font-mono">{queueData.max_concurrency}</span>
            <span>concurrent</span>
          </div>
        </div>
        <div className="text-sm text-muted-foreground font-mono">
          {queueData.queue.length} jobs in queue
        </div>
      </div>

      <QueueControls 
        isPaused={queueData.paused}
        maxConcurrency={queueData.max_concurrency}
        onSuccess={forceRefresh} 
      />

      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <List className="h-5 w-5" />
            <CardTitle>Pending Jobs</CardTitle>
          </div>
          <CardDescription>
            Jobs waiting to be executed in order • Hover over items to show management controls
          </CardDescription>
        </CardHeader>
        <CardContent>
          {queueData.queue.length === 0 ? (
            <div className="flex items-center justify-center h-32 text-muted-foreground">
              {queueData.paused ? 'Queue is paused' : 'No jobs in queue'}
            </div>
          ) : (
            <div className="space-y-2">
              {queueData.queue.map((jobId, index) => (
                <div
                  key={`${jobId}-${index}`}
                  className={`
                    flex items-center justify-between p-3 rounded-md border group transition-colors
                    ${queueData.paused 
                      ? 'bg-yellow-50 border-yellow-200 hover:bg-yellow-100' 
                      : 'bg-muted/50 hover:bg-muted/70'
                    }
                  `}
                >
                  <div className="flex items-center gap-3">
                    <div className={`
                      flex items-center justify-center w-8 h-8 rounded-full text-sm font-medium font-mono
                      ${queueData.paused
                        ? 'bg-yellow-600 text-yellow-50'
                        : 'bg-primary text-primary-foreground'
                      }
                    `}>
                      {index + 1}
                    </div>
                    <span className="font-mono">{jobId}</span>
                  </div>
                  <div className="flex items-center gap-3">
                    <div className="flex items-center gap-2 text-sm text-muted-foreground font-mono">
                      <Clock className="h-4 w-4" />
                      {index === 0 ? (queueData.paused ? 'Paused' : 'Next') : `Position ${index + 1}`}
                    </div>
                    <div className="opacity-0 group-hover:opacity-100 transition-opacity">
                      <QueueJobControls
                        jobId={jobId}
                        index={index}
                        totalJobs={queueData.queue.length}
                        queue={queueData.queue}
                        onSuccess={forceRefresh}
                      />
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
