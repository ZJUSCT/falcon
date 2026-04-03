'use client';

import { useState, useEffect } from 'react';
import { apiClient } from '@/lib/api';
import { Repo } from '@/types';

export function ConfigsView() {
  const [repos, setRepos] = useState<Repo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const fetchRepos = async () => {
      try {
        setLoading(true);
        const data = await apiClient.getRepos();
        setRepos(data);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch configs');
      } finally {
        setLoading(false);
      }
    };

    fetchRepos();
  }, []);

  if (loading && repos.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading configs...</div>
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
    <div className="p-6 space-y-6">
      <div>
        <h2 className="text-lg font-bold">Configs</h2>
        <p className="text-xs text-muted-foreground">Read-only repository configuration details</p>
      </div>

      {repos.length === 0 ? (
        <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
          No repository configs found.
        </div>
      ) : (
        <div className="space-y-4">
          {repos.map(repo => {
            const nodeSelectorEntries = repo.sync.nodeSelector ? Object.entries(repo.sync.nodeSelector) : [];

            return (
              <div
                key={repo.id}
                className="rounded-lg border border-border bg-card p-4 space-y-3"
              >
                <div className="flex items-center justify-between">
                  <h3 className="font-mono font-bold text-base">{repo.id}</h3>
                  <span className="text-xs text-muted-foreground font-mono">{repo.info.type}</span>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 text-sm">
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Image</div>
                    <div className="font-mono text-xs break-all">{repo.sync.image}</div>
                  </div>
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Interval</div>
                    <div className="font-mono text-xs">{repo.sync.interval.value}</div>
                  </div>
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Timeout</div>
                    <div className="font-mono text-xs">{repo.sync.timeout}</div>
                  </div>
                  {repo.sync.node && (
                    <div>
                      <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Node</div>
                      <div className="font-mono text-xs">{repo.sync.node}</div>
                    </div>
                  )}
                  {nodeSelectorEntries.length > 0 && (
                    <div>
                      <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Node Selector</div>
                      <div className="space-y-0.5">
                        {nodeSelectorEntries.map(([k, v]) => (
                          <div key={k} className="font-mono text-xs">
                            <span className="text-muted-foreground">{k}:</span> {v}
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </div>

                {repo.sync.command && repo.sync.command.length > 0 && (
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Command</div>
                    <div className="font-mono text-xs bg-muted/40 rounded p-2 break-all">
                      {repo.sync.command.join(' ')}
                    </div>
                  </div>
                )}

                {repo.sync.volumes && repo.sync.volumes.length > 0 && (
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Volumes</div>
                    <div className="space-y-1">
                      {repo.sync.volumes.map((vol, i) => (
                        <div key={i} className="font-mono text-xs bg-muted/40 rounded px-2 py-1">
                          {vol.src} &rarr; {vol.dst}
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                {repo.sync.environments && repo.sync.environments.length > 0 && (
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Environment</div>
                    <div className="space-y-1 max-h-32 overflow-y-auto">
                      {repo.sync.environments.map((env, i) => (
                        <div key={i} className="font-mono text-xs bg-muted/40 rounded px-2 py-1 break-all">
                          {env}
                        </div>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
