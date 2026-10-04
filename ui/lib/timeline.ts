import { Job, mirrorMode, displaySyncPhase, isZeroTime } from '@/types';

export interface TimelineEvent {
  id: string;
  mirror: string;
  kind: 'active' | 'planned';
  phase: string;
  start: number;
  end: number;
  durationKnown: boolean;
  overdue: boolean;
}

const minute = 60_000;
export const hour = 60 * minute;

function timestamp(value: string): number | undefined {
  const time = Date.parse(value);
  return !isZeroTime(value) && Number.isFinite(time) ? time : undefined;
}

export function syncDuration(job: Job): number | undefined {
  const seconds = job.last_sync_duration_seconds;
  return seconds !== undefined && Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : undefined;
}

export function timelineEvents(jobs: Job[], now: number): TimelineEvent[] {
  const events: TimelineEvent[] = [];
  for (const job of jobs) {
    if (mirrorMode(job) !== 'sync') continue;
    const phase = displaySyncPhase(job);
    if (phase && ['Syncing', 'Snapshotting', 'Cancelling'].includes(phase)) {
      const start = timestamp(job.last_attempt_at);
      if (start !== undefined && start <= now) events.push({
        id: `${job.namespace}/${job.id}/active`, mirror: job.id, kind: 'active', phase,
        start, end: now, durationKnown: true, overdue: false,
      });
      continue;
    }
    if (job.paused || !phase) continue;
    // A queued manual sync may not have a next automatic schedule yet.
    const start = phase === 'Pending' ? timestamp(job.last_attempt_at) ?? timestamp(job.next_attempt_at) : timestamp(job.next_attempt_at);
    if (start === undefined || start > now + 48 * hour) continue;
    const duration = syncDuration(job);
    events.push({
      id: `${job.namespace}/${job.id}/planned`, mirror: job.id, kind: 'planned', phase,
      start, end: start + (duration ?? 15 * minute), durationKnown: duration !== undefined,
      overdue: start < now,
    });
  }
  return events;
}

export function timelineBounds(events: TimelineEvent[], now: number): { start: number; end: number } {
  const first = Math.min(now, ...events.map(event => event.start));
  const last = Math.max(now + hour, ...events.map(event => event.end));
  return {
    start: Math.floor(first / (hour / 4)) * hour / 4,
    end: Math.min(now + 48 * hour, Math.ceil(last / (hour / 4)) * hour / 4),
  };
}

export interface TimelineBar extends TimelineEvent {
  left: number;
  width: number;
  labelWidth: number;
  track: number;
  clippedStart: boolean;
  clippedEnd: boolean;
}

// Reserve label space as well as the bar's time interval. First-fit reuses
// earlier tracks whenever a gap is free; bar widths keep their time scale.
export function packTimeline(events: TimelineEvent[], start: number, end: number, width: number, reserveLabels = true): TimelineBar[] {
  if (!(end > start) || width <= 0) return [];
  const visible = events.filter(event => event.end >= start && event.start < end)
    .sort((a, b) => a.start - b.start || a.id.localeCompare(b.id));
  const trackEnds: number[] = [];
  return visible.map(event => {
    const left = Math.max(0, (event.start - start) / (end - start) * width);
    const right = Math.min(width, (event.end - start) / (end - start) * width);
    const barWidth = Math.min(width - left, Math.max(3, right - left));
    const labelWidth = Math.min(width - left, Math.max(barWidth, (reserveLabels ? Math.min(180, event.mirror.length * 7.3 + 20) : 0)));
    let track = trackEnds.findIndex(trackEnd => trackEnd + (reserveLabels ? 8 : 1) <= left);
    if (track < 0) track = trackEnds.length;
    trackEnds[track] = left + labelWidth;
    return { ...event, left, width: barWidth, labelWidth, track, clippedStart: event.start < start, clippedEnd: event.end > end };
  });
}

export function durationLabel(ms: number): string {
  const minutes = Math.max(1, Math.round(ms / minute));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h${minutes % 60 ? ` ${minutes % 60}m` : ''}`;
}
