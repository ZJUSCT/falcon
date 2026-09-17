// Sync log viewer. Kubernetes keeps container logs only as best-effort
// node-local files (the kubelet rotates at 10 MiB and exposes the current
// file only), so this panel shows exactly one Job — the current
// synchronization's while it runs, otherwise the newest retained Job — and
// presents the output as potentially truncated or missing. keepJobs retains
// Job/Pod objects and status history, not durable logs.

import { useEffect, useRef, useState } from 'react';
import { ArrowDown, RefreshCw } from 'lucide-react';

interface Container { name: string; init: boolean; state: 'waiting' | 'running' | 'terminated' }
interface Pod { name: string; uid: string; containers: Container[] }
interface Job { name: string; uid: string; startedAt?: string; finishedAt?: string; result?: string; current: boolean }
interface Source { job?: string; jobUID?: string; startedAt?: string; current: boolean; phase?: string; message?: string; jobs: Job[]; pods: Pod[] }
const minHeight = 160;
const maxHeight = 900;
const heightKey = 'falcon-log-height';

async function responseError(response: Response) {
  try { return (await response.json()).error || `Log request failed (${response.status})`; }
  catch { return `Log request failed (${response.status})`; }
}

export function SyncLogs({ mirrorId }: { mirrorId: string }) {
  const [source, setSource] = useState<Source | null>(null);
  const [sourceError, setSourceError] = useState('');
  const [podUID, setPodUID] = useState('');
  const [containerName, setContainerName] = useState('');
  const [lines, setLines] = useState(1000);
  const [follow, setFollow] = useState(true);
  const [timestamps, setTimestamps] = useState(true);
  const [refresh, setRefresh] = useState(0);
  const [logs, setLogs] = useState('');
  const [error, setError] = useState('');
  const [state, setState] = useState('');
  const [height, setHeight] = useState(320);
  const [atBottom, setAtBottom] = useState(true);
  const bottom = useRef(true);
  const viewport = useRef<HTMLPreElement>(null);
  const drag = useRef<{ y: number; height: number } | null>(null);
  const base = `/api/mirrors/${encodeURIComponent(mirrorId)}/logs`;
  const sourceURL = base;

  useEffect(() => {
    try { const value = Number(localStorage.getItem(heightKey)); if (value >= minHeight && value <= maxHeight) setHeight(value); } catch {}
  }, []);

  useEffect(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    setSource(null); setSourceError('');
    async function poll() {
      try {
        const response = await fetch(sourceURL, { signal: abort.signal });
        if (!response.ok) {
          throw new Error(await responseError(response));
        }
        const next: Source = await response.json();
        if (abort.signal.aborted) return;
        setSource(previous => JSON.stringify(previous) === JSON.stringify(next) ? previous : next);
        setSourceError('');
      } catch (e) {
        if (!abort.signal.aborted) setSourceError(e instanceof Error ? e.message : 'Unable to locate synchronization logs');
      } finally { if (!abort.signal.aborted) timer = setTimeout(poll, 5000); }
    }
    poll();
    return () => { abort.abort(); clearTimeout(timer); };
  }, [sourceURL, refresh]);

  // The backend picks the default Job (current synchronization's while one
  // runs, else the newest retained) and reports its UID here.
  const jobUID = source?.jobUID;
  const pod = source?.pods.find(p => p.uid === podUID) || source?.pods[0];
  const container = pod?.containers.find(c => c.name === containerName) || pod?.containers[0];
  const selectedPodName = pod?.name;
  const selectedPodUID = pod?.uid;
  const selectedContainer = container?.name;
  const containerState = container?.state;

  useEffect(() => {
    setLogs(''); setError(''); setState(''); bottom.current = true; setAtBottom(true);
    if (!jobUID || !selectedPodName || !selectedPodUID || !selectedContainer || containerState === 'waiting') return;
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function connect() {
      let buffer = '';
      let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
      setLogs(''); setError(''); setState('Connecting…');
      try {
        const query = new URLSearchParams({ jobUID: jobUID!, pod: selectedPodName!, podUID: selectedPodUID!, container: selectedContainer!, lines: String(lines), follow: String(follow), timestamps: String(timestamps) });
        const response = await fetch(`${base}/stream?${query}`, { signal: abort.signal });
        if (!response.ok) throw new Error(await responseError(response));
        if (!response.body) throw new Error('Log streaming is unavailable in this browser');
        setState(follow && containerState === 'running' ? 'Following' : 'Loading…');
        reader = response.body.getReader();
        const framing = new TextDecoder();
        const text = new TextDecoder();
        let pending = ''; let ended = false;
        const append = (chunk: string) => {
          buffer = (buffer + chunk).slice(-2 * 1024 * 1024);
          const rows = buffer.split('\n');
          const keep = lines + (buffer.endsWith('\n') ? 1 : 0);
          if (rows.length > keep) buffer = rows.slice(-keep).join('\n');
          setLogs(buffer);
        };
        while (true) {
          const result = await reader.read();
          if (abort.signal.aborted) return;
          if (result.done) break;
          pending += framing.decode(result.value, { stream: true });
          let newline;
          while ((newline = pending.indexOf('\n')) >= 0) {
            const frame = JSON.parse(pending.slice(0, newline)); pending = pending.slice(newline + 1);
            if (frame.error) throw new Error(frame.error);
            if (frame.data) append(text.decode(Uint8Array.from(atob(frame.data), c => c.charCodeAt(0)), { stream: true }));
            if (frame.end) ended = true;
          }
        }
        append(text.decode());
        if (!ended) throw new Error('Log connection interrupted');
        setState('Finished');
      } catch (e) {
        if (abort.signal.aborted) return;
        setError(e instanceof Error ? e.message : 'Unable to read logs'); setState('');
      } finally { await reader?.cancel().catch(() => undefined); }
      if (!abort.signal.aborted && follow && containerState === 'running') {
        setState('Reconnecting…'); timer = setTimeout(connect, 3000);
      }
    }
    connect();
    return () => { abort.abort(); clearTimeout(timer); };
  }, [base, jobUID, selectedPodName, selectedPodUID, selectedContainer, containerState, lines, follow, timestamps, refresh]);

  useEffect(() => {
    if (bottom.current && viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight;
  }, [logs, height]);

  function resize(value: number) {
    const next = Math.min(maxHeight, Math.max(minHeight, value)); setHeight(next);
    try { localStorage.setItem(heightKey, String(next)); } catch {}
  }
  const field = 'rounded border border-border bg-background px-2 py-1 text-xs';
  return (
    <section aria-label="Synchronization logs" className="rounded-lg border border-border bg-card overflow-hidden">
      <div className="px-4 py-3 border-b border-border space-y-2">
        <div className="flex items-center justify-between gap-2">
          <h3 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">Sync Logs</h3>
          <button type="button" aria-label="Refresh logs" title="Refresh logs" onClick={() => setRefresh(v => v + 1)} className="rounded border border-border p-1.5 hover:bg-accent"><RefreshCw className="h-4 w-4" aria-hidden="true" /></button>
        </div>
        {source?.job && <div className="text-xs text-muted-foreground break-all">{source.current ? 'Current Job' : 'Latest retained Job'}: <span className="font-mono">{source.job}</span>{source.startedAt && <> · <time dateTime={source.startedAt}>{new Date(source.startedAt).toLocaleString()}</time></>}</div>}
        {source?.message && <p className="text-xs text-muted-foreground">{source.message}{source.phase && ` Current phase: ${source.phase}.`}</p>}
        <p className="text-xs text-muted-foreground">
          Logs are best-effort: the kubelet rotates container logs and keeps the current file only,
          so retained Jobs do not guarantee readable logs — output may be truncated or missing.
        </p>
        <div className="flex flex-wrap items-center gap-3 text-xs">
          {(source?.pods.length || 0) > 1 && <label className="flex items-center gap-1">Pod<select aria-label="Log Pod" value={pod?.uid || ''} onChange={e => { setPodUID(e.target.value); setContainerName(''); }} className={`${field} max-w-64`}>{source?.pods.map(p => <option key={p.uid} value={p.uid}>{p.name}</option>)}</select></label>}
          <label className="flex items-center gap-1">Container<select aria-label="Log container" disabled={!container} value={container?.name || ''} onChange={e => setContainerName(e.target.value)} className={field}>{!container && <option value="">None</option>}{pod?.containers.map(c => <option key={c.name} value={c.name}>{c.name}{c.init ? ' (init)' : ''}</option>)}</select></label>
          <label className="flex items-center gap-1">Lines<select aria-label="Log lines" value={lines} onChange={e => setLines(Number(e.target.value))} className={field}>{[100, 1000, 2500].map(n => <option key={n}>{n}</option>)}</select></label>
          <label className="flex items-center gap-1"><input type="checkbox" checked={follow} onChange={e => setFollow(e.target.checked)} />Follow</label>
          <label className="flex items-center gap-1"><input type="checkbox" checked={timestamps} onChange={e => setTimestamps(e.target.checked)} />Timestamps</label>
          <span role="status" className="text-muted-foreground">{state}</span>
        </div>
      </div>
      {(sourceError || error) && <p role="alert" className="px-4 py-2 text-xs text-destructive">{sourceError || error}</p>}
      <div className="relative">
        <pre ref={viewport} tabIndex={0} aria-label="Sync log output" style={{ height }} onScroll={e => { const el = e.currentTarget; bottom.current = el.scrollHeight - el.clientHeight - el.scrollTop < 24; setAtBottom(bottom.current); }} className="overflow-auto whitespace-pre p-4 text-xs font-mono leading-relaxed bg-background text-foreground">{logs || (containerState === 'waiting' ? 'Waiting for this container to start…' : !source ? 'Loading log sources…' : !pod ? 'No logs available.' : state === 'Finished' ? 'This container has not written any logs.' : 'Waiting for log output…')}</pre>
        {!atBottom && <button type="button" onClick={() => { bottom.current = true; setAtBottom(true); if (viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight; }} className="absolute bottom-2 right-2 flex items-center gap-1 rounded border border-border bg-card px-2 py-1 text-xs"><ArrowDown className="h-3 w-3" />Latest</button>}
      </div>
      <div role="separator" aria-label="Resize log panel" aria-orientation="horizontal" aria-valuenow={height} aria-valuemin={minHeight} aria-valuemax={maxHeight} tabIndex={0}
        onPointerDown={e => { drag.current = { y: e.clientY, height }; e.currentTarget.setPointerCapture(e.pointerId); }}
        onPointerMove={e => { if (drag.current) resize(drag.current.height + e.clientY - drag.current.y); }}
        onPointerUp={() => { drag.current = null; }} onLostPointerCapture={() => { drag.current = null; }}
        onKeyDown={e => { if (e.key === 'ArrowUp' || e.key === 'ArrowDown') { e.preventDefault(); resize(height + (e.key === 'ArrowDown' ? 24 : -24)); } }}
        className="h-3 touch-none cursor-row-resize border-t border-border flex items-center justify-center hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-primary"><span className="h-0.5 w-10 rounded bg-muted-foreground/50" /></div>
    </section>
  );
}
