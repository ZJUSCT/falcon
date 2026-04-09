'use client';

import { useState, useEffect } from 'react';
import { apiClient } from '@/lib/api';
import { formatBytes } from '@/lib/utils';
import { ZFSWorkerReport, ZFSPoolInfo, ZFSDatasetInfo, ZFSSnapshotInfo } from '@/types';
import { HardDrive, Database, Camera, Plus, Trash2, RefreshCw, Loader2 } from 'lucide-react';
import { StatusBadge } from '@/components/status-badge';

interface PoolRow extends ZFSPoolInfo { worker: string }
interface DatasetRow extends ZFSDatasetInfo { worker: string }
interface SnapshotRow extends ZFSSnapshotInfo { worker: string }

export function StorageView() {
  const [reports, setReports] = useState<ZFSWorkerReport[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [snapDataset, setSnapDataset] = useState('');
  const [snapWorker, setSnapWorker] = useState('');
  const [snapName, setSnapName] = useState('');
  const [snapRecursive, setSnapRecursive] = useState(false);
  const [snapCreating, setSnapCreating] = useState(false);

  const [showCreateDataset, setShowCreateDataset] = useState(false);
  const [newDsWorker, setNewDsWorker] = useState('');
  const [newDsName, setNewDsName] = useState('');
  const [newDsCompression, setNewDsCompression] = useState('lz4');
  const [newDsQuota, setNewDsQuota] = useState('');
  const [dsCreating, setDsCreating] = useState(false);

  const [snapFilter, setSnapFilter] = useState('');

  const fetchReports = async () => {
    try {
      const data = await apiClient.getZFSReports();
      setReports(data);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch ZFS data');
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  };

  useEffect(() => {
    fetchReports();
    const interval = setInterval(fetchReports, 30000);
    return () => clearInterval(interval);
  }, []);

  const handleRefresh = async () => {
    setRefreshing(true);
    try {
      await apiClient.refreshZFS();
      // Wait a moment for workers to respond, then fetch cached reports.
      await new Promise(r => setTimeout(r, 2000));
    } catch { /* best effort */ }
    await fetchReports();
  };

  // Aggregate data from all workers
  const pools: PoolRow[] = reports.flatMap(r =>
    (r.pools || []).map(p => ({ ...p, worker: r.worker_name }))
  );
  const datasets: DatasetRow[] = reports.flatMap(r =>
    (r.datasets || []).map(d => ({ ...d, worker: r.worker_name }))
  );
  const snapshots: SnapshotRow[] = reports.flatMap(r =>
    (r.snapshots || []).map(s => ({ ...s, worker: r.worker_name }))
  );
  const filteredSnapshots = snapFilter
    ? snapshots.filter(s => s.dataset.includes(snapFilter) || s.snap_name.includes(snapFilter) || s.worker.includes(snapFilter))
    : snapshots;

  const workerNames = reports.map(r => r.worker_name);
  const multiWorker = workerNames.length > 1;

  const handleCreateSnapshot = async () => {
    if (!snapDataset || !snapName || !snapWorker) return;
    setSnapCreating(true);
    try {
      await apiClient.createZFSSnapshot(snapWorker, snapDataset, snapName, snapRecursive);
      setSnapName('');
      setSnapDataset('');
      setSnapWorker('');
      await fetchReports();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to create snapshot');
    } finally {
      setSnapCreating(false);
    }
  };

  const handleDestroySnapshot = async (worker: string, snapshot: string) => {
    if (!confirm(`Destroy snapshot "${snapshot}" on ${worker}? This cannot be undone.`)) return;
    try {
      await apiClient.destroyZFSSnapshot(worker, snapshot);
      await fetchReports();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to destroy snapshot');
    }
  };

  const handleCreateDataset = async () => {
    if (!newDsName || !newDsWorker) return;
    setDsCreating(true);
    const props: Record<string, string> = {};
    if (newDsCompression) props.compression = newDsCompression;
    if (newDsQuota) props.quota = newDsQuota;
    try {
      await apiClient.createZFSDataset(newDsWorker, newDsName, props);
      setNewDsName('');
      setNewDsQuota('');
      setShowCreateDataset(false);
      await fetchReports();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to create dataset');
    } finally {
      setDsCreating(false);
    }
  };

  const openSnapForm = (worker: string, dataset: string) => {
    setSnapWorker(worker);
    setSnapDataset(dataset);
  };

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-bold">Storage (ZFS)</h2>
          <p className="text-xs text-muted-foreground">
            {workerNames.length > 0
              ? `${workerNames.length} worker${workerNames.length > 1 ? 's' : ''}: ${workerNames.join(', ')}`
              : 'No ZFS reports available'}
          </p>
        </div>
        <button onClick={handleRefresh} disabled={refreshing}
          className="p-2 rounded-md border border-border hover:bg-muted transition-colors disabled:opacity-50">
          <RefreshCw className={`h-4 w-4 ${refreshing ? 'animate-spin' : ''}`} />
        </button>
      </div>

      {error && (
        <div className="rounded-lg border border-red-500/30 bg-red-500/10 p-4 text-sm text-red-400">{error}</div>
      )}

      {loading ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-muted-foreground">Loading ZFS data...</div>
        </div>
      ) : reports.length === 0 ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-muted-foreground">No ZFS reports received from workers yet</div>
        </div>
      ) : (
        <>
          {/* Pool Overview */}
          <div className="space-y-3">
            <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground flex items-center gap-2">
              <HardDrive className="h-4 w-4" /> Pools
            </h3>
            <div className="grid gap-4 grid-cols-1 md:grid-cols-2 xl:grid-cols-3">
              {pools.map(pool => {
                const usedPct = pool.size > 0 ? (pool.allocated / pool.size) * 100 : 0;
                return (
                  <div key={`${pool.worker}/${pool.name}`} className="rounded-lg border border-border bg-card p-4 space-y-3">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <span className="font-mono font-bold text-sm">{pool.name}</span>
                        {multiWorker && (
                          <span className="px-1.5 py-0.5 text-[10px] rounded bg-muted text-muted-foreground font-mono">{pool.worker}</span>
                        )}
                      </div>
                      <StatusBadge status={pool.health === 'ONLINE' ? 'Online' : pool.health} />
                    </div>
                    <div className="space-y-1">
                      <div className="flex justify-between text-xs text-muted-foreground">
                        <span>{formatBytes(pool.allocated)} used</span>
                        <span>{formatBytes(pool.free)} free</span>
                      </div>
                      <div className="w-full bg-muted rounded-full h-2">
                        <div
                          className={`h-2 rounded-full transition-all ${usedPct > 85 ? 'bg-red-500' : usedPct > 70 ? 'bg-yellow-500' : 'bg-green-500'}`}
                          style={{ width: `${Math.min(usedPct, 100)}%` }}
                        />
                      </div>
                      <div className="text-xs text-muted-foreground text-right">{formatBytes(pool.size)} total</div>
                    </div>
                    <div className="grid grid-cols-3 gap-2 text-xs">
                      <div>
                        <div className="text-muted-foreground">Frag</div>
                        <div className="font-mono">{pool.fragmentation || '--'}%</div>
                      </div>
                      <div>
                        <div className="text-muted-foreground">Dedup</div>
                        <div className="font-mono">{pool.dedup}</div>
                      </div>
                      <div>
                        <div className="text-muted-foreground">Capacity</div>
                        <div className="font-mono">{pool.capacity_pct || '--'}%</div>
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>

          {/* Datasets */}
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground flex items-center gap-2">
                <Database className="h-4 w-4" /> Datasets
                <span className="text-xs font-normal">({datasets.length})</span>
              </h3>
              <button onClick={() => { setShowCreateDataset(!showCreateDataset); if (!newDsWorker && workerNames.length > 0) setNewDsWorker(workerNames[0]); }}
                className="flex items-center gap-1 px-2 py-1 text-xs rounded-md border border-border hover:bg-muted transition-colors">
                <Plus className="h-3 w-3" /> Create
              </button>
            </div>

            {showCreateDataset && (
              <div className="rounded-lg border border-border bg-card p-4 space-y-3">
                <h4 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Create Dataset</h4>
                <div className="grid grid-cols-1 sm:grid-cols-5 gap-3">
                  {multiWorker && (
                    <select value={newDsWorker} onChange={e => setNewDsWorker(e.target.value)}
                      className="px-3 py-1.5 text-xs rounded-md border border-border bg-background text-foreground">
                      {workerNames.map(n => <option key={n} value={n}>{n}</option>)}
                    </select>
                  )}
                  <input placeholder="Dataset name (e.g. tank/mirrors/newrepo)"
                    value={newDsName} onChange={e => setNewDsName(e.target.value)}
                    className={`${multiWorker ? 'col-span-1' : 'col-span-2'} px-3 py-1.5 text-xs rounded-md border border-border bg-background text-foreground`} />
                  <select value={newDsCompression} onChange={e => setNewDsCompression(e.target.value)}
                    className="px-3 py-1.5 text-xs rounded-md border border-border bg-background text-foreground">
                    <option value="lz4">lz4</option>
                    <option value="zstd">zstd</option>
                    <option value="gzip">gzip</option>
                    <option value="off">off</option>
                  </select>
                  <input placeholder="Quota (e.g. 500G)" value={newDsQuota} onChange={e => setNewDsQuota(e.target.value)}
                    className="px-3 py-1.5 text-xs rounded-md border border-border bg-background text-foreground" />
                </div>
                <button onClick={handleCreateDataset} disabled={dsCreating || !newDsName}
                  className="px-3 py-1.5 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50">
                  {dsCreating ? <Loader2 className="h-3 w-3 animate-spin inline" /> : 'Create'}
                </button>
              </div>
            )}

            <div className="rounded-lg border border-border bg-card overflow-hidden">
              <div className="overflow-x-auto">
                <table className="w-full text-xs">
                  <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                    <tr>
                      <th className="px-3 py-2 text-left">Dataset</th>
                      {multiWorker && <th className="px-3 py-2 text-left">Worker</th>}
                      <th className="px-3 py-2 text-left">Repo</th>
                      <th className="px-3 py-2 text-right">Used</th>
                      <th className="hidden md:table-cell px-3 py-2 text-right">Referenced</th>
                      <th className="hidden md:table-cell px-3 py-2 text-right">Available</th>
                      <th className="hidden lg:table-cell px-3 py-2 text-right">Logical</th>
                      <th className="hidden lg:table-cell px-3 py-2 text-center">Compress</th>
                      <th className="hidden lg:table-cell px-3 py-2 text-center">Ratio</th>
                      <th className="hidden xl:table-cell px-3 py-2 text-right">Snapshots</th>
                      <th className="hidden xl:table-cell px-3 py-2 text-right">Written</th>
                      <th className="px-3 py-2 text-center">Actions</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border">
                    {datasets.map(ds => (
                      <tr key={`${ds.worker}/${ds.name}`} className="bg-background hover:bg-muted/40 transition-colors">
                        <td className="px-3 py-2 font-mono text-xs">{ds.name}</td>
                        {multiWorker && (
                          <td className="px-3 py-2">
                            <span className="px-1.5 py-0.5 text-[10px] rounded bg-muted text-muted-foreground font-mono">{ds.worker}</span>
                          </td>
                        )}
                        <td className="px-3 py-2">
                          {ds.repo_id ? (
                            <span className="px-1.5 py-0.5 text-[11px] rounded bg-primary/15 text-primary font-mono">{ds.repo_id}</span>
                          ) : <span className="text-muted-foreground">--</span>}
                        </td>
                        <td className="px-3 py-2 text-right font-mono">{formatBytes(ds.used)}</td>
                        <td className="hidden md:table-cell px-3 py-2 text-right font-mono">{formatBytes(ds.referenced)}</td>
                        <td className="hidden md:table-cell px-3 py-2 text-right font-mono">{formatBytes(ds.available)}</td>
                        <td className="hidden lg:table-cell px-3 py-2 text-right font-mono">{formatBytes(ds.logicalused)}</td>
                        <td className="hidden lg:table-cell px-3 py-2 text-center font-mono">{ds.compression}</td>
                        <td className="hidden lg:table-cell px-3 py-2 text-center font-mono">{ds.compressratio}</td>
                        <td className="hidden xl:table-cell px-3 py-2 text-right font-mono">{formatBytes(ds.usedbysnapshots)}</td>
                        <td className="hidden xl:table-cell px-3 py-2 text-right font-mono">{formatBytes(ds.written)}</td>
                        <td className="px-3 py-2 text-center">
                          <button onClick={() => openSnapForm(ds.worker, ds.name)}
                            className="p-1 rounded text-muted-foreground hover:text-primary hover:bg-primary/10 transition-colors" title="Create snapshot">
                            <Camera className="h-3.5 w-3.5" />
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          </div>

          {/* Snapshots */}
          <div className="space-y-3">
            <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground flex items-center gap-2">
              <Camera className="h-4 w-4" /> Snapshots
              <span className="text-xs font-normal">({snapshots.length})</span>
            </h3>

            {snapDataset && (
              <div className="rounded-lg border border-border bg-card p-4 space-y-3">
                <h4 className="text-xs font-semibold">
                  Create snapshot for <span className="font-mono text-primary">{snapDataset}</span>
                  {multiWorker && <span className="text-muted-foreground"> on {snapWorker}</span>}
                </h4>
                <div className="flex items-center gap-3">
                  <input placeholder="Snapshot name (e.g. manual-backup)"
                    value={snapName} onChange={e => setSnapName(e.target.value)}
                    className="flex-1 px-3 py-1.5 text-xs rounded-md border border-border bg-background text-foreground" />
                  <label className="flex items-center gap-1 text-xs text-muted-foreground">
                    <input type="checkbox" checked={snapRecursive} onChange={e => setSnapRecursive(e.target.checked)} />
                    Recursive
                  </label>
                  <button onClick={handleCreateSnapshot} disabled={snapCreating || !snapName}
                    className="px-3 py-1.5 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50">
                    {snapCreating ? <Loader2 className="h-3 w-3 animate-spin inline" /> : 'Create'}
                  </button>
                  <button onClick={() => { setSnapDataset(''); setSnapWorker(''); }}
                    className="px-2 py-1.5 text-xs text-muted-foreground hover:text-foreground">Cancel</button>
                </div>
              </div>
            )}

            <input placeholder="Filter snapshots..." value={snapFilter} onChange={e => setSnapFilter(e.target.value)}
              className="w-full px-3 py-1.5 text-xs rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground" />

            <div className="rounded-lg border border-border bg-card overflow-hidden">
              <div className="overflow-x-auto">
                <table className="w-full text-xs">
                  <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                    <tr>
                      <th className="px-3 py-2 text-left">Snapshot</th>
                      <th className="hidden md:table-cell px-3 py-2 text-left">Dataset</th>
                      {multiWorker && <th className="px-3 py-2 text-left">Worker</th>}
                      <th className="px-3 py-2 text-right">Used</th>
                      <th className="hidden md:table-cell px-3 py-2 text-right">Referenced</th>
                      <th className="hidden lg:table-cell px-3 py-2 text-left">Created</th>
                      <th className="px-3 py-2 text-center">Actions</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border">
                    {filteredSnapshots.map(snap => (
                      <tr key={`${snap.worker}/${snap.name}`} className="bg-background hover:bg-muted/40 transition-colors">
                        <td className="px-3 py-2 font-mono">{snap.snap_name}</td>
                        <td className="hidden md:table-cell px-3 py-2 font-mono text-muted-foreground">{snap.dataset}</td>
                        {multiWorker && (
                          <td className="px-3 py-2">
                            <span className="px-1.5 py-0.5 text-[10px] rounded bg-muted text-muted-foreground font-mono">{snap.worker}</span>
                          </td>
                        )}
                        <td className="px-3 py-2 text-right font-mono">{formatBytes(snap.used)}</td>
                        <td className="hidden md:table-cell px-3 py-2 text-right font-mono">{formatBytes(snap.referenced)}</td>
                        <td className="hidden lg:table-cell px-3 py-2 text-muted-foreground">
                          {snap.creation > 0 ? new Date(snap.creation * 1000).toLocaleString() : '--'}
                        </td>
                        <td className="px-3 py-2 text-center">
                          <button onClick={() => handleDestroySnapshot(snap.worker, snap.name)}
                            className="p-1 rounded text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-colors" title="Destroy snapshot">
                            <Trash2 className="h-3.5 w-3.5" />
                          </button>
                        </td>
                      </tr>
                    ))}
                    {filteredSnapshots.length === 0 && (
                      <tr><td colSpan={multiWorker ? 7 : 6} className="px-3 py-6 text-center text-muted-foreground">No snapshots</td></tr>
                    )}
                  </tbody>
                </table>
              </div>
            </div>
          </div>

          {/* Report timestamps */}
          <div className="text-xs text-muted-foreground text-right space-x-3">
            {reports.map(r => (
              <span key={r.worker_name}>
                {r.worker_name}: {r.timestamp > 0 ? new Date(r.timestamp * 1000).toLocaleString() : '--'}
              </span>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
