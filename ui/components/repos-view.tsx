'use client';

import { useState, useEffect } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { apiClient } from '@/lib/api';
import { Repo } from '@/types';
import { Globe, HardDrive, Clock } from 'lucide-react';

export function ReposView() {
  const [repos, setRepos] = useState<Repo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const fetchInitialRepos = async () => {
      try {
        setLoading(true);
        const data = await apiClient.getRepos();
        setRepos(data);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch repos');
      } finally {
        setLoading(false);
      }
    };

    const fetchRepoUpdates = async () => {
      try {
        const data = await apiClient.getRepos();
        setRepos(data);
        setError(null);
      } catch (err) {
        // Don't update error state for background refresh failures
        console.warn('Background repos refresh failed:', err);
      }
    };

    // Initial load with loading state
    fetchInitialRepos();
    
    // Background updates without loading state (repos change infrequently)
    const interval = setInterval(fetchRepoUpdates, 30000); // Refresh every 30 seconds

    return () => clearInterval(interval);
  }, []);

  if (loading && repos.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading repositories...</div>
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
        <h2 className="text-3xl font-bold tracking-tight">Repositories</h2>
        <div className="text-sm text-muted-foreground font-mono">
          {repos.length} repositories
        </div>
      </div>
      
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        {repos.map((repo) => (
          <Card key={repo.id} className="hover:shadow-md transition-shadow">
            <CardHeader className="pb-2">
              <div className="flex items-center justify-between">
                <CardTitle className="text-lg font-mono">{repo.id}</CardTitle>
                <StatusBadge status={repo.info.type} />
              </div>
              <CardDescription className="line-clamp-2">
                {repo.info.description.en || repo.info.description.zh || 'No description'}
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="space-y-2 text-sm">
                <div className="flex items-center gap-2">
                  <Globe className="h-4 w-4 text-muted-foreground" />
                  <span className="truncate font-mono text-sm">{repo.info.upstream}</span>
                </div>
                <div className="flex items-center gap-2">
                  <HardDrive className="h-4 w-4 text-muted-foreground" />
                  <span className="truncate font-mono text-sm">{repo.sync.image}</span>
                </div>
                <div className="flex items-center gap-2">
                  <Clock className="h-4 w-4 text-muted-foreground" />
                  <span className="font-mono">Every {repo.sync.interval.value}</span>
                </div>
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
