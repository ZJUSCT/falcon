'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import type { PointerEvent as ReactPointerEvent } from 'react';
import { LocateFixed, Maximize2, Search } from 'lucide-react';
import { Job } from '@/types';
import { durationLabel, hour, packTimeline, timelineBounds, timelineEvents } from '@/lib/timeline';

interface Props { jobs: Job[]; now: Date; onNavigateToJob?: (id: string) => void }
interface Window { start: number; end: number }
const timeLabel = (time: number) => new Date(time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
const dateLabel = (time: number) => new Date(time).toLocaleDateString([], { month: 'short', day: 'numeric' });
const pattern = { backgroundImage: 'repeating-linear-gradient(135deg, transparent, transparent 5px, currentColor 5px, currentColor 6px)' };

function constrain(view: Window, bounds: Window): Window {
  const span = Math.min(bounds.end - bounds.start, Math.max(5 * 60_000, view.end - view.start));
  const start = Math.max(bounds.start, Math.min(bounds.end - span, view.start));
  return { start, end: start + span };
}

export function SyncTimeline({ jobs, now, onNavigateToJob }: Props) {
  const [selection, setSelection] = useState<Window | null>(null);
  const [search, setSearch] = useState('');
  const [size, setSize] = useState({ width: 1000, height: 600 });
  const [selected, setSelected] = useState<string | null>(null);
  const viewport = useRef<HTMLDivElement>(null);
  const minimap = useRef<HTMLDivElement>(null);
  const nowMs = now.getTime();
  const events = useMemo(() => timelineEvents(jobs, nowMs), [jobs, nowMs]);
  const bounds = useMemo(() => timelineBounds(events, nowMs), [events, nowMs]);
  const fullSpan = bounds.end - bounds.start;
  const width = Math.max(200, size.width - 32);
  const initialSpan = Math.min(fullSpan, width / 36 * hour);
  const view = constrain(selection ?? { start: nowMs - initialSpan / 4, end: nowMs + initialSpan * 3 / 4 }, bounds);
  const { start, end } = view;
  const span = end - start;
  const matching = events.filter(event => event.mirror.toLowerCase().includes(search.toLowerCase()));
  const bars = packTimeline(matching, start, end, width);
  const miniature = packTimeline(matching, bounds.start, bounds.end, width, false);
  const miniTracks = Math.max(1, ...miniature.map(bar => bar.track + 1));
  const requiredTracks = Math.max(0, ...bars.map(bar => bar.track + 1));
  const tracks = Math.max(requiredTracks, Math.floor((size.height - 44) / 36), 4);
  const rowHeight = Math.max(36, (size.height - 44) / tracks);
  const nowX = (nowMs - start) / span * width;
  const focused = bars.find(bar => bar.id === selected);
  const active = bars.filter(bar => bar.kind === 'active').length;
  const outside = matching.length - bars.length;
  const unknown = bars.filter(bar => !bar.durationKnown).length;
  const desiredStep = span / Math.max(2, Math.floor(width / 85));
  const step = [5 / 60, 15 / 60, .5, 1, 2, 3, 6, 12, 24, 48, 168].map(h => h * hour).find(value => value >= desiredStep) ?? Math.ceil(desiredStep / hour) * hour;
  const ticks: number[] = [];
  for (let tick = Math.ceil(start / step) * step; tick <= end; tick += step) ticks.push(tick);
  const latest = useRef({ view, bounds, width });
  latest.current = { view, bounds, width };

  useEffect(() => {
    const element = viewport.current;
    if (!element) return;
    const observer = new ResizeObserver(() => setSize({ width: element.clientWidth, height: element.clientHeight }));
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  const zoom = (factor: number, anchor: number) => {
    const { view, bounds } = latest.current;
    const oldSpan = view.end - view.start;
    const nextSpan = Math.max(5 * 60_000, Math.min(bounds.end - bounds.start, oldSpan * factor));
    const fraction = Math.max(0, Math.min(1, (anchor - view.start) / oldSpan));
    setSelection(constrain({ start: anchor - nextSpan * fraction, end: anchor + nextSpan * (1 - fraction) }, bounds));
  };

  // Native non-passive listeners keep wheel navigation inside the timeline.
  useEffect(() => {
    const chart = viewport.current, map = minimap.current;
    if (!chart || !map) return;
    const pan = (event: WheelEvent) => {
      if (!event.ctrlKey && !event.shiftKey && Math.abs(event.deltaX) <= Math.abs(event.deltaY)) return;
      event.preventDefault();
      const { view, bounds, width } = latest.current;
      const delta = (event.deltaY || event.deltaX) * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? width : 1);
      if (event.ctrlKey) {
        zoom(Math.exp(Math.max(-.6, Math.min(.6, delta * .002))), (view.start + view.end) / 2);
        return;
      }
      const shift = delta / width * (view.end - view.start);
      setSelection(constrain({ start: view.start + shift, end: view.end + shift }, bounds));
    };
    const scale = (event: WheelEvent) => {
      event.preventDefault();
      const { view } = latest.current;
      const delta = event.deltaY * (event.deltaMode === 1 ? 16 : 1);
      zoom(Math.exp(Math.max(-.6, Math.min(.6, delta * .002))), (view.start + view.end) / 2);
    };
    chart.addEventListener('wheel', pan, { passive: false });
    map.addEventListener('wheel', scale, { passive: false });
    return () => { chart.removeEventListener('wheel', pan); map.removeEventListener('wheel', scale); };
  }, []);

  const pointers = useRef(new Map<number, { x: number; y: number }>());
  const gesture = useRef<{ view: Window; x: number; y: number; scrollTop: number; axis?: 'x' | 'y'; distance: number; mode: 'move' | 'start' | 'end'; source: 'map' | 'chart' } | null>(null);
  const moved = useRef(false);
  const startGesture = (event: ReactPointerEvent<HTMLDivElement>, source: 'map' | 'chart') => {
    moved.current = false;
    if (event.pointerType === 'mouse' && event.button !== 0) return;
    if (event.pointerType === 'mouse' && source === 'map') event.preventDefault();
    pointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    const points = Array.from(pointers.current.values());
    let current = latest.current.view;
    const mode = (event.target as HTMLElement).dataset.handle as 'start' | 'end' | undefined;
    if (source === 'map' && points.length === 1 && !mode) {
      const rect = event.currentTarget.getBoundingClientRect();
      const position = bounds.start + (event.clientX - rect.left) / rect.width * fullSpan;
      if (position < current.start || position > current.end) {
        const half = (current.end - current.start) / 2;
        current = constrain({ start: position - half, end: position + half }, bounds);
        setSelection(current);
      }
    }
    gesture.current = { view: current, x: points.reduce((a, b) => a + b.x, 0) / points.length, y: event.clientY, scrollTop: event.currentTarget.scrollTop, distance: points.length > 1 ? Math.hypot(points[1].x - points[0].x, points[1].y - points[0].y) : 0, mode: mode ?? 'move', source };
    moved.current = false;
    if (source === 'map' || points.length > 1) event.currentTarget.setPointerCapture(event.pointerId);
  };
  const moveGesture = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!pointers.current.has(event.pointerId) || !gesture.current) return;
    pointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    const points = Array.from(pointers.current.values());
    const g = gesture.current;
    const midpoint = points.reduce((a, b) => a + b.x, 0) / points.length;
    if (Math.hypot(midpoint - g.x, event.clientY - g.y) > 3 || points.length > 1) moved.current = true;
    if (!moved.current) return;
    event.preventDefault();
    event.currentTarget.setPointerCapture(event.pointerId);
    if (points.length === 1 && g.source === 'chart') {
      if (event.pointerType === 'mouse') {
        event.currentTarget.scrollTop = g.scrollTop + g.y - event.clientY;
      } else {
        g.axis ??= Math.abs(event.clientY - g.y) > Math.abs(midpoint - g.x) ? 'y' : 'x';
        if (g.axis === 'y') { event.currentTarget.scrollTop = g.scrollTop + g.y - event.clientY; return; }
      }
    }
    const originalSpan = g.view.end - g.view.start;
    if (points.length > 1 && g.distance > 0) {
      const nextSpan = Math.max(5 * 60_000, Math.min(fullSpan, originalSpan * g.distance / Math.max(1, Math.hypot(points[1].x - points[0].x, points[1].y - points[0].y))));
      const rect = event.currentTarget.getBoundingClientRect();
      const fraction = g.source === 'chart' ? Math.max(0, Math.min(1, (g.x - rect.left) / rect.width)) : .5;
      const anchor = g.view.start + originalSpan * fraction;
      const shift = g.source === 'chart' ? -(midpoint - g.x) / rect.width * nextSpan : 0;
      setSelection(constrain({ start: anchor - nextSpan * fraction + shift, end: anchor + nextSpan * (1 - fraction) + shift }, bounds));
    } else {
      const pixels = event.currentTarget.clientWidth;
      const shift = (midpoint - g.x) / pixels * (g.source === 'map' ? fullSpan : -originalSpan);
      const next = { ...g.view };
      if (g.mode === 'start') next.start = Math.max(bounds.start, Math.min(next.end - 5 * 60_000, next.start + shift));
      else if (g.mode === 'end') next.end = Math.min(bounds.end, Math.max(next.start + 5 * 60_000, next.end + shift));
      else { next.start += shift; next.end += shift; }
      setSelection(constrain(next, bounds));
    }
  };
  const endGesture = (event: ReactPointerEvent<HTMLDivElement>) => {
    pointers.current.delete(event.pointerId);
    if (pointers.current.size === 0) gesture.current = null;
    else if (gesture.current) {
      const point = Array.from(pointers.current.values())[0];
      gesture.current = { ...gesture.current, view: latest.current.view, x: point.x, y: point.y, scrollTop: event.currentTarget.scrollTop, axis: undefined, distance: 0 };
    }
  };
  const pointerProps = (source: 'map' | 'chart') => ({
    onPointerDown: (event: ReactPointerEvent<HTMLDivElement>) => startGesture(event, source),
    onPointerMove: moveGesture, onPointerUp: endGesture, onPointerCancel: endGesture,
  });
  const control = 'inline-flex h-8 items-center justify-center gap-1 rounded border border-border bg-background px-2 text-xs hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary';

  return (
    <section aria-label="Synchronization timeline" className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border border-border bg-card">
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b p-3">
        <div className="flex flex-wrap items-center gap-3 text-xs">
          <span className="flex items-center gap-1.5"><span className="h-2.5 w-5 rounded-sm bg-blue-600" />Active <strong>{active}</strong></span>
          <span className="flex items-center gap-1.5"><span className="h-2.5 w-5 rounded-sm border border-dashed border-primary text-primary/30" style={pattern} />Planned <strong>{bars.length - active}</strong></span>
          <span className="text-muted-foreground">{requiredTracks} tracks{outside > 0 && ` · ${outside} outside view`}</span>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <label className="relative"><Search className="absolute left-2 top-2 h-4 w-4 text-muted-foreground" aria-hidden="true" />
            <input aria-label="Filter timeline mirrors" placeholder="Find a mirror…" value={search} onChange={event => setSearch(event.target.value)} className="h-8 w-36 rounded border border-border bg-background pl-7 pr-2 text-xs" />
          </label>
          <button className={control} onClick={() => setSelection(constrain({ start: nowMs - span / 4, end: nowMs + span * 3 / 4 }, bounds))}><LocateFixed className="h-3.5 w-3.5" />Now</button>
          <button className={control} onClick={() => setSelection(bounds)}><Maximize2 className="h-3.5 w-3.5" />Fit all</button>
        </div>
      </div>
      <div className="shrink-0 border-b px-4 pb-2 pt-2">
        <div className="mb-1 flex justify-between text-[10px] text-muted-foreground"><span>{dateLabel(bounds.start)} {timeLabel(bounds.start)}</span><span>Full schedule · {durationLabel(fullSpan)}</span><span>{dateLabel(bounds.end)} {timeLabel(bounds.end)}</span></div>
        <div ref={minimap} aria-label="Timeline overview selection" tabIndex={0}
          onKeyDown={event => {
            if (['ArrowLeft', 'ArrowRight'].includes(event.key)) { event.preventDefault(); const shift = span / 10 * (event.key === 'ArrowLeft' ? -1 : 1); setSelection(constrain({ start: start + shift, end: end + shift }, bounds)); }
            if (event.key === '+' || event.key === '=') { event.preventDefault(); zoom(.8, (start + end) / 2); }
            if (event.key === '-') { event.preventDefault(); zoom(1.25, (start + end) / 2); }
          }}
          {...pointerProps('map')} className="relative h-16 touch-none select-none overflow-hidden rounded border border-border bg-muted/30 outline-none focus-visible:ring-2 focus-visible:ring-primary">
          {miniature.map(bar => <span key={bar.id} className={`pointer-events-none absolute rounded-sm ${bar.kind === 'active' ? 'bg-blue-600' : 'bg-primary/40'}`} style={{ left: `${bar.left / width * 100}%`, width: `${Math.max(.15, bar.width / width * 100)}%`, top: 3 + bar.track / miniTracks * 56, height: Math.max(1, Math.min(4, 50 / miniTracks)) }} />)}
          <span className="pointer-events-none absolute inset-y-0 left-0 bg-background/65" style={{ width: `${(start - bounds.start) / fullSpan * 100}%` }} />
          <span className="pointer-events-none absolute inset-y-0 right-0 bg-background/65" style={{ width: `${(bounds.end - end) / fullSpan * 100}%` }} />
          <div className="absolute inset-y-0 cursor-grab border-2 border-primary bg-primary/5 active:cursor-grabbing" data-selection-window style={{ left: `${(start - bounds.start) / fullSpan * 100}%`, width: `${span / fullSpan * 100}%` }}>
            <span data-handle="start" className="absolute -left-1 inset-y-0 w-3 cursor-ew-resize rounded bg-primary/80" />
            <span data-handle="end" className="absolute -right-1 inset-y-0 w-3 cursor-ew-resize rounded bg-primary/80" />
          </div>
        </div>
        <div className="mt-1 flex flex-wrap justify-between gap-1 text-[10px] text-muted-foreground">
          <span>{dateLabel(start)} {timeLabel(start)} — {dateLabel(end)} {timeLabel(end)} · {durationLabel(span)} selected</span>
          <span>Scroll to browse tracks · drag to pan · overview scroll / pinch to zoom</span>
        </div>
      </div>
      <div ref={viewport} {...pointerProps('chart')} className="relative min-h-0 flex-1 cursor-grab touch-none select-none overflow-x-hidden overflow-y-auto active:cursor-grabbing" tabIndex={0} aria-label="Timeline tracks">
        <div style={{ minHeight: '100%' }}>
          <div className="sticky top-0 z-20 h-11 border-b bg-card">
            {ticks.map(tick => <span key={tick} className="absolute top-1 -translate-x-1/2 whitespace-nowrap text-[10px] text-muted-foreground" style={{ left: 16 + (tick - start) / span * width }}>
              <span className="block text-center">{timeLabel(tick)}</span>
              {(step >= 24 * hour || new Date(tick).getHours() === 0 && new Date(tick).getMinutes() === 0) && <span className="block text-center">{dateLabel(tick)}</span>}
            </span>)}
            {nowX >= 0 && nowX <= width && <span className="absolute bottom-0 -translate-x-1/2 rounded-t bg-primary px-1.5 text-[10px] font-semibold text-primary-foreground" style={{ left: 16 + nowX }}>Now</span>}
          </div>
          <div className="relative mx-4" style={{ width, height: tracks * rowHeight }}>
            {Array.from({ length: tracks }, (_, track) => <div key={track} className="absolute left-0 right-0 border-b border-border/60" style={{ top: track * rowHeight, height: rowHeight, background: track % 2 ? 'hsl(var(--muted) / 0.25)' : undefined }} />)}
            {ticks.map(tick => <div key={tick} className="pointer-events-none absolute bottom-0 top-0 border-l border-border/60" style={{ left: (tick - start) / span * width }} />)}
            {nowX >= 0 && nowX <= width && <div className="pointer-events-none absolute bottom-0 top-0 z-10 border-l-2 border-primary/70" style={{ left: nowX }} />}
            {bars.map(bar => {
              const narrow = bar.width < 100;
              const description = `${bar.mirror} · ${bar.kind === 'active' ? bar.phase : bar.overdue ? 'Overdue / queued' : 'Planned'} · ${new Date(bar.start).toLocaleString()} → ${bar.kind === 'active' ? 'now' : new Date(bar.end).toLocaleString()} · ${durationLabel(bar.end - bar.start)}${bar.kind === 'planned' ? bar.durationKnown ? ' estimated from previous sync' : ' placeholder; no previous duration' : ' elapsed'}`;
              return <button key={bar.id} type="button" data-timeline-bar={bar.id} data-track={bar.track}
                aria-label={description} title={`${description}. Open mirror details.`}
                onMouseEnter={() => setSelected(bar.id)} onMouseLeave={() => setSelected(null)} onFocus={() => setSelected(bar.id)} onBlur={() => setSelected(null)}
                onClick={event => { if (event.detail === 0 || !moved.current) onNavigateToJob?.(bar.mirror); }}
                className="absolute z-10 cursor-grab rounded text-left active:cursor-grabbing focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary focus-visible:ring-offset-2 focus-visible:ring-offset-card"
                style={{ left: bar.left, top: bar.track * rowHeight + (rowHeight - 26) / 2, width: bar.labelWidth, height: 26 }}>
                <span className={`absolute inset-y-0 left-0 overflow-hidden rounded ${bar.kind === 'active' ? 'bg-blue-600' : 'border border-dashed border-primary bg-primary/10'}`} style={{ width: bar.width }}>
                  {bar.kind === 'planned' && <span className="absolute inset-0 text-primary/20" style={pattern} />}
                </span>
                <span className={`relative block truncate px-1.5 text-[11px] leading-[26px] ${bar.kind === 'active' && !narrow ? 'font-semibold text-white' : 'text-foreground'}`}>
                  <span className={narrow ? 'rounded bg-card/95 px-1' : ''}>{bar.clippedStart && '‹ '}{bar.mirror}{bar.clippedEnd && ' ›'}</span>
                </span>
              </button>;
            })}
            {bars.length === 0 && <div className="absolute inset-x-0 top-12 text-center text-sm text-muted-foreground">No active or planned syncs in this window.</div>}
          </div>
        </div>
      </div>
      <div className="min-h-[52px] shrink-0 border-t px-3 py-2 text-xs" aria-live="polite">
        {focused ? <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <strong className="font-mono">{focused.mirror}</strong><span>{focused.kind === 'active' ? focused.phase : focused.overdue ? 'Overdue / queued' : 'Planned'}</span>
          <span className="text-muted-foreground">{dateLabel(focused.start)} {timeLabel(focused.start)} → {focused.kind === 'active' ? 'now' : `${dateLabel(focused.end)} ${timeLabel(focused.end)}`}</span>
          <span>{durationLabel(focused.end - focused.start)} {focused.kind === 'active' ? 'elapsed' : focused.durationKnown ? 'estimated · previous sync duration' : 'placeholder · no duration history'}</span>
        </div> : <p className="text-muted-foreground">Solid: active elapsed time. Patterned: estimated from the previous sync.{unknown > 0 && ` ${unknown} without history use a 15m placeholder.`} Click a bar for mirror details.</p>}
      </div>
    </section>
  );
}
