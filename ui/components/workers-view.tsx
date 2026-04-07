'use client';

import { useState, useEffect } from 'react';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { Worker } from '@/types';
import { Trash2 } from 'lucide-react';

const labelColors = [
  'bg-blue-500/20 text-blue-300 border-blue-500/30',
  'bg-purple-500/20 text-purple-300 border-purple-500/30',
  'bg-emerald-500/20 text-emerald-300 border-emerald-500/30',
  'bg-amber-500/20 text-amber-300 border-amber-500/30',
  'bg-pink-500/20 text-pink-300 border-pink-500/30',
  'bg-cyan-500/20 text-cyan-300 border-cyan-500/30',
  'bg-orange-500/20 text-orange-300 border-orange-500/30',
];

function getLabelColor(key: string): string {
  let hash = 0;
  for (let i = 0; i < key.length; i++) {
    hash = ((hash << 5) - hash) + key.charCodeAt(i);
    hash |= 0;
  }
  return labelColors[Math.abs(hash) % labelColors.length];
}

export function WorkersView() {
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);

  const fetchUpdates = async () => {
    try {
      const data = await apiClient.getWorkers();
      setWorkers(data);
      setError(null);
    } catch (err) {
      console.warn('Background workers refresh failed:', err);
    }
  };

  useEffect(() => {
    const fetchInitial = async () => {
      try {
        setLoading(true);
        const data = await apiClient.getWorkers();
        setWorkers(data);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch workers');
      } finally {
        setLoading(false);
      }
    };

    fetchInitial();
    const interval = setInterval(fetchUpdates, 10000);
    return () => clearInterval(interval);
  }, []);

  const handleRemove = async (name: string, e: React.MouseEvent) => {
    e.stopPropagation();
    if (!confirm(`Remove offline worker "${name}"? This cannot be undone.`)) return;
    try {
      setRemoving(name);
      await apiClient.removeWorker(name);
      await fetchUpdates();
    } catch (err) {
      console.error('Failed to remove worker:', err);
    } finally {
      setRemoving(null);
    }
  };

  if (loading && workers.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading workers...</div>
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
        <h2 className="text-lg font-bold">Workers</h2>
        <p className="text-xs text-muted-foreground">Connected sync workers and their status</p>
      </div>

      {workers.length === 0 ? (
        <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
          No workers registered.
        </div>
      ) : (
        <div className="grid gap-4 grid-cols-1 md:grid-cols-2 xl:grid-cols-3">
          {workers.map(worker => {
            const labels = worker.labels ? Object.entries(worker.labels) : [];
            const vars = worker.vars ? Object.entries(worker.vars) : [];
            const runningCount = worker.running_actions?.length || 0;

            return (
              <div
                key={worker.name}
                className="rounded-lg border border-border bg-card p-4 space-y-3"
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <div
                      className={`h-2.5 w-2.5 rounded-full ${
                        worker.status === 'Online' ? 'bg-green-500' : 'bg-red-500'
                      }`}
                    />
                    <span className="font-mono font-bold text-sm">{worker.name}</span>
                  </div>
                  <StatusBadge status={worker.status} />
                </div>

                <div className="text-xs text-muted-foreground font-mono">{worker.addr}</div>

                {labels.length > 0 && (
                  <div className="flex flex-wrap gap-1.5">
                    {labels.map(([k, v]) => (
                      <span
                        key={k}
                        className={`inline-flex items-center px-2 py-0.5 text-[11px] font-mono rounded-full border ${getLabelColor(k)}`}
                      >
                        {k}={v}
                      </span>
                    ))}
                  </div>
                )}

                {vars.length > 0 && (
                  <div>
                    <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Variables</div>
                    <div className="space-y-0.5">
                      {vars.map(([k, v]) => (
                        <div key={k} className="font-mono text-xs text-muted-foreground">
                          <span className="text-foreground/70">${k}</span> = {v}
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                <div className="flex items-center justify-between text-xs">
                  <div>
                    <span className="text-muted-foreground">Running: </span>
                    <span className="font-mono">{runningCount > 0 ? runningCount : 'none'}</span>
                  </div>
                  <div>
                    <span className="text-muted-foreground">Heartbeat: </span>
                    <RelativeTime date={worker.last_heartbeat} variant="compact" className="text-xs" />
                  </div>
                </div>

                {worker.status === 'Offline' && (
                  <button
                    onClick={e => handleRemove(worker.name, e)}
                    disabled={removing === worker.name}
                    className="w-full flex items-center justify-center gap-2 px-3 py-1.5 text-xs rounded-md border border-red-500/30 text-red-400 hover:bg-red-500/10 transition-colors disabled:opacity-50"
                  >
                    <Trash2 className="h-3 w-3" />
                    {removing === worker.name ? 'Removing...' : 'Remove Worker'}
                  </button>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
