'use client';

// Storage (ZFS) — read-only per-node ZFS inventory. This is the successor
// of the legacy Storage panel (which went away with the old mutation API);
// the new one is purely observational. Data source: GET /api/storage
// (30s poll; the same agent-backed usage aggregation that powers the
// Mirrors "Size" column). Degradations: when the feature is not deployed
// (404) or the fetch fails, the hook keeps returning null and the page
// renders a single hint card instead of erroring; `complete: false` adds
// an amber banner listing the per-agent errors.
//
// Layout: one section per storage node (agent order preserved), one card
// per ZFS pool — health badge + capacity numbers/bar in the header, and a
// datasets table below. Dataset rows expand in place (chevron, click or
// Enter/Space) to reveal the snapshots of that dataset; multiple rows can
// be expanded at the same time.

import { useState, Fragment } from 'react';
import type { KeyboardEvent } from 'react';
import { Badge } from '@/components/ui/badge';
import { RelativeTime } from '@/components/relative-time';
import { useStorage } from '@/lib/hooks';
import { cn, formatBytes } from '@/lib/utils';
import { StoragePool, StorageDataset, StorageSnapshot } from '@/types';
import { ChevronRight } from 'lucide-react';

// ZFS pool health → badge palette, in the spirit of status-badge's
// getStatusColor. ONLINE is green, DEGRADED yellow, the fatal states
// (FAULTED/OFFLINE/UNAVAIL) red; anything else — including "" (unknown,
// rendered as UNKNOWN) — degrades to the neutral gray.
function getPoolHealthColor(health: string): string {
  switch (health) {
    case 'ONLINE':
      return 'text-green-500 bg-green-500/15 border-green-500/30 hover:bg-green-500/25';
    case 'DEGRADED':
      return 'text-yellow-500 bg-yellow-500/15 border-yellow-500/30 hover:bg-yellow-500/25';
    case 'FAULTED':
    case 'OFFLINE':
    case 'UNAVAIL':
      return 'text-red-500 bg-red-500/15 border-red-500/30 hover:bg-red-500/25';
    default:
      return 'text-muted-foreground bg-muted border-border hover:bg-muted';
  }
}

function PoolHealthBadge({ health }: { health: string }) {
  return (
    <Badge className={`border font-mono ${getPoolHealthColor(health)}`}>
      {health || 'UNKNOWN'}
    </Badge>
  );
}

// "tank/pvc-xxxx@snapshot-xxxx" → "snapshot-xxxx"; the full name stays
// available as the cell title.
function snapshotLeafName(name: string): string {
  const at = name.lastIndexOf('@');
  return at >= 0 ? name.slice(at + 1) : name;
}

// Logical (pre-compression) used ÷ on-disk used, e.g. "1.07x". Only shown
// when both sides are known and non-zero — 0 means unknown in the contract.
function compressionRatio(dataset: StorageDataset): string | null {
  if (dataset.logicalUsedBytes > 0 && dataset.usedBytes > 0) {
    return `${(dataset.logicalUsedBytes / dataset.usedBytes).toFixed(2)}x`;
  }
  return null;
}

function capacityBarColor(percent: number): string {
  if (percent >= 90) return 'bg-red-500';
  if (percent >= 80) return 'bg-yellow-500';
  return 'bg-green-500';
}

// Snapshot rows of one expanded dataset (indented sub-table).
function SnapshotList({ snapshots }: { snapshots: StorageSnapshot[] }) {
  return (
    <div className="px-3 py-2 sm:px-6">
      <div className="mb-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">
        Snapshots ({snapshots.length})
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead>
            <tr className="text-[10px] uppercase tracking-wide text-muted-foreground">
              <th className="py-1 pr-3 text-left font-semibold">Snapshot</th>
              <th className="py-1 pr-3 text-right font-semibold">Written</th>
              <th className="py-1 pr-3 text-right font-semibold">Referenced</th>
              <th className="py-1 text-right font-semibold">Created</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border/60">
            {snapshots.map(snapshot => (
              <tr key={snapshot.name}>
                <td className="py-1.5 pr-3">
                  <span className="font-mono" title={snapshot.name}>
                    {snapshotLeafName(snapshot.name)}
                  </span>
                </td>
                <td className="py-1.5 pr-3 text-right font-mono tabular-nums whitespace-nowrap">
                  {formatBytes(snapshot.writtenBytes) ?? '—'}
                </td>
                <td className="py-1.5 pr-3 text-right font-mono tabular-nums whitespace-nowrap">
                  {formatBytes(snapshot.referencedBytes) ?? '—'}
                </td>
                <td className="py-1.5 text-right whitespace-nowrap">
                  <RelativeTime date={new Date(snapshot.createdAt * 1000).toISOString()} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// One ZFS pool: header (name + health), capacity summary with a bar, and
// the datasets table whose rows expand into SnapshotList. Expansion state
// is local and keyed by dataset name (full dataset paths are unique within
// a pool), so several rows can stay open at once and survive re-polls.
function PoolCard({ pool }: { pool: StoragePool }) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const toggleDataset = (datasetName: string) => {
    setExpanded(prev => ({ ...prev, [datasetName]: !prev[datasetName] }));
  };

  const handleRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, datasetName: string, expandable: boolean) => {
    if (!expandable) return;
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      toggleDataset(datasetName);
    }
  };

  // 0 = unknown sentinel handling per the API contract: a pool without a
  // known size shows "unknown" instead of a bar; unknown capacity /
  // fragmentation percents render as "—". The bar itself is allocated/size
  // (capacityPercent is only used for the textual percent).
  const sizeText = pool.sizeBytes > 0 ? formatBytes(pool.sizeBytes) : null;
  const barPercent = pool.sizeBytes > 0
    ? Math.min(100, (pool.allocatedBytes / pool.sizeBytes) * 100)
    : null;

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card">
      {/* Pool header */}
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b px-4 py-3">
        <div className="flex items-center gap-2">
          <span className="font-mono text-sm font-semibold">{pool.name}</span>
          <PoolHealthBadge health={pool.health} />
        </div>
        <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
          {pool.datasets.length} {pool.datasets.length === 1 ? 'dataset' : 'datasets'}
        </span>
      </div>

      {/* Capacity summary */}
      <div className="space-y-2 border-b px-4 py-3">
        <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 font-mono text-xs tabular-nums">
          <span>
            {formatBytes(pool.allocatedBytes) ?? '—'} used
            <span className="text-muted-foreground"> / {sizeText ?? 'unknown'}</span>
          </span>
          <span className="text-muted-foreground">
            {formatBytes(pool.freeBytes) ?? '—'} free ·{' '}
            {pool.capacityPercent > 0 ? `${pool.capacityPercent}%` : '—'} capacity · frag{' '}
            {pool.fragmentationPercent > 0 ? `${pool.fragmentationPercent}%` : '—'}
          </span>
        </div>
        {barPercent !== null ? (
          <div
            className="h-1.5 overflow-hidden rounded-full bg-muted"
            title={`${formatBytes(pool.allocatedBytes)} of ${sizeText}`}
          >
            <div
              className={cn('h-full rounded-full transition-all duration-300', capacityBarColor(barPercent))}
              style={{ width: `${barPercent}%` }}
            />
          </div>
        ) : (
          <div className="font-mono text-[11px] text-muted-foreground">Pool size unknown</div>
        )}
      </div>

      {/* Datasets */}
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            <tr>
              <th className="px-3 py-2 text-left">Dataset</th>
              <th className="hidden lg:table-cell px-3 py-2 text-left">Mirror</th>
              <th className="px-3 py-2 text-right">Used</th>
              <th className="hidden md:table-cell px-3 py-2 text-right">Referenced</th>
              <th className="hidden md:table-cell px-3 py-2 text-right">Written</th>
              <th className="hidden lg:table-cell px-3 py-2 text-right">Logical</th>
              <th className="hidden lg:table-cell px-3 py-2 text-right">Ratio</th>
              <th className="px-3 py-2 text-right">Snapshots</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {pool.datasets.map(dataset => {
              const isExpanded = !!expanded[dataset.name];
              const expandable = dataset.snapshots.length > 0;
              const ratio = compressionRatio(dataset);
              return (
                <Fragment key={dataset.name}>
                  <tr
                    onClick={expandable ? () => toggleDataset(dataset.name) : undefined}
                    onKeyDown={event => handleRowKeyDown(event, dataset.name, expandable)}
                    tabIndex={expandable ? 0 : -1}
                    className={cn(
                      'bg-background transition-colors',
                      expandable && 'cursor-pointer hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60'
                    )}
                  >
                    <td className="px-3 py-2 align-top">
                      <div className="flex items-center gap-1.5">
                        <ChevronRight
                          className={cn(
                            'h-3.5 w-3.5 flex-shrink-0 text-muted-foreground transition-transform',
                            isExpanded && 'rotate-90',
                            !expandable && 'invisible'
                          )}
                        />
                        <span className="max-w-[16ch] truncate font-mono sm:max-w-[24ch] lg:max-w-[32ch]" title={dataset.name}>
                          {dataset.name}
                        </span>
                      </div>
                    </td>
                    <td className="hidden lg:table-cell px-3 py-2 align-top whitespace-nowrap">
                      {dataset.mirror ? (
                        <span className="font-mono">{dataset.mirror}</span>
                      ) : (
                        <span className="font-mono text-muted-foreground">—</span>
                      )}
                    </td>
                    <td className="px-3 py-2 text-right align-top font-mono tabular-nums whitespace-nowrap">
                      {formatBytes(dataset.usedBytes) ?? '—'}
                    </td>
                    <td className="hidden md:table-cell px-3 py-2 text-right align-top font-mono tabular-nums whitespace-nowrap">
                      {formatBytes(dataset.referencedBytes) ?? '—'}
                    </td>
                    <td className="hidden md:table-cell px-3 py-2 text-right align-top font-mono tabular-nums whitespace-nowrap">
                      {formatBytes(dataset.writtenBytes) ?? '—'}
                    </td>
                    <td className="hidden lg:table-cell px-3 py-2 text-right align-top font-mono tabular-nums whitespace-nowrap">
                      {dataset.logicalUsedBytes > 0 ? formatBytes(dataset.logicalUsedBytes) : '—'}
                    </td>
                    <td className="hidden lg:table-cell px-3 py-2 text-right align-top font-mono tabular-nums whitespace-nowrap">
                      {ratio ?? '—'}
                    </td>
                    <td className="px-3 py-2 text-right align-top font-mono tabular-nums">
                      {dataset.snapshots.length}
                    </td>
                  </tr>
                  {isExpanded && (
                    <tr>
                      <td colSpan={8} className="border-b border-border bg-muted/20 p-0">
                        <SnapshotList snapshots={dataset.snapshots} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
            {pool.datasets.length === 0 && (
              <tr>
                <td colSpan={8} className="px-3 py-3 text-muted-foreground">
                  No datasets reported.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export function StorageView() {
  const storage = useStorage();

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-bold">Storage (ZFS)</h2>
        {storage && (
          <span className="text-xs text-muted-foreground">
            updated <RelativeTime date={storage.generatedAt} className="font-mono" />
          </span>
        )}
      </div>

      {storage === null ? (
        // 404 (usage aggregation / zfs-agent not deployed) or a failed
        // fetch — both leave the hook at null; the page keeps polling.
        <div className="space-y-1 rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
          <div className="font-semibold text-foreground">Storage data unavailable</div>
          <p className="text-xs leading-relaxed">
            The /api/storage endpoint did not answer — either the request is still in
            flight, or the usage aggregation (zfs-agent) is not deployed on the backend.
            The rest of the dashboard is unaffected; this page retries every 30 seconds.
          </p>
        </div>
      ) : (
        <>
          {!storage.complete && (
            <div className="space-y-1.5 rounded-lg border border-amber-500/40 bg-amber-500/10 p-3">
              <div className="text-xs font-semibold text-amber-500">
                Partial data: some storage nodes did not respond (or no ready agent) — values may be incomplete.
              </div>
              {storage.errors.length > 0 && (
                <ul className="list-inside list-disc space-y-0.5 font-mono text-[11px] text-amber-500/90">
                  {storage.errors.map((error, index) => (
                    <li key={`${index}-${error}`}>{error}</li>
                  ))}
                </ul>
              )}
            </div>
          )}

          {storage.nodes.length === 0 ? (
            <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
              No storage nodes reported.
            </div>
          ) : (
            storage.nodes.map(node => (
              <section key={node.node} className="space-y-3">
                <div className="flex items-baseline gap-2">
                  <h3 className="font-mono text-sm font-bold">{node.node}</h3>
                  <span className="font-mono text-xs text-muted-foreground">
                    {node.pools.length} {node.pools.length === 1 ? 'pool' : 'pools'}
                  </span>
                </div>
                {node.pools.length === 0 ? (
                  <div className="rounded-lg border border-border bg-card p-4 text-xs text-muted-foreground">
                    No pools reported for this node.
                  </div>
                ) : (
                  node.pools.map(pool => <PoolCard key={pool.name} pool={pool} />)
                )}
              </section>
            ))
          )}
        </>
      )}
    </div>
  );
}
