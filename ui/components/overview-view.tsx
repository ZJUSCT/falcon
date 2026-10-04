'use client';

// Overview: the 24-hour sync activity clock wheel.
//
// The clock wheel is the visual signature of the legacy admin UI and
// is kept as-is conceptually: it is a 24-hour round dial (0° at the top =
// midnight, 15° per hour) with a hand showing the current time. Every sync
// event of every job is drawn at the angle of its wall-clock time:
//
//   - next attempt  (yellow, dashed link; jobs whose next_attempt_at is set
//     and that are not Running right now)
//   - last success  (green, last_success_at within a ±12h window)
//   - last failure  (red, last_failure_at within a ±12h window)
//   - running sync  (blue, last_attempt_at of currently Running jobs)
//
// Markers are separated on concentric radial lanes. Only labels with free
// space are drawn; every event retains hover details and mirror navigation. Data source: GET /api/jobs (the controller's legacy-compatible
// job list; see internal/webapi/jobs.go for the field semantics).

import React, { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import { RelativeTime } from '@/components/relative-time';
import { StatusBadge } from '@/components/status-badge';
import { apiClient } from '@/lib/api';
import { Job, displaySyncPhase, mirrorMode } from '@/types';
import { isZeroTime } from '@/types';
import { SyncTimeline } from '@/components/sync-timeline';
import { ZoomIn, ZoomOut, RotateCcw, Move, Clock3, GanttChart } from 'lucide-react';

interface TimeEvent {
  time: Date;
  type: 'nextAttempt' | 'lastSuccess' | 'lastFailure' | 'lastAttempt';
  jobId: string;
  jobStatus: string;
}

interface ClockDimensions {
  size: number;
  radius: number;
  eventRadius: number;
  centerX: number;
  centerY: number;
}

interface OverviewViewProps {
  onNavigateToJob?: (jobId: string) => void;
}

const stepping_radius = 24;

export function OverviewView({ onNavigateToJob }: OverviewViewProps = {}) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [view, setView] = useState<'clock' | 'timeline'>('timeline');
  useEffect(() => {
    try { if (localStorage.getItem('falcon-overview-view') === 'clock') setView('clock'); } catch { /* Session-only preference. */ }
  }, []);
  const changeView = (next: 'clock' | 'timeline') => {
    setView(next);
    try { localStorage.setItem('falcon-overview-view', next); } catch { /* Session-only preference. */ }
  };
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [currentTime, setCurrentTime] = useState(new Date());
  const [hoveredEvent, setHoveredEvent] = useState<TimeEvent | null>(null);
  const [tooltipPosition, setTooltipPosition] = useState<{ x: number; y: number } | null>(null);

  // Canvas state for zoom and pan
  const [scale, setScale] = useState(1);
  const [translateX, setTranslateX] = useState(0);
  const [translateY, setTranslateY] = useState(0);
  const [isDragging, setIsDragging] = useState(false);
  const [dragStart, setDragStart] = useState({ x: 0, y: 0 });
  const canvasRef = useRef<HTMLDivElement>(null);

  // Update current time every second
  useEffect(() => {
    const interval = setInterval(() => {
      setCurrentTime(new Date());
    }, 1000);
    return () => clearInterval(interval);
  }, []);

  // Canvas interaction handlers
  const handleZoomIn = useCallback(() => {
    setScale(prev => Math.min(prev * 1.2, 3));
  }, []);

  const handleZoomOut = useCallback(() => {
    setScale(prev => Math.max(prev / 1.2, 0.3));
  }, []);

  const handleReset = useCallback(() => {
    setScale(1);
    setTranslateX(0);
    setTranslateY(0);
  }, []);

  const handleMouseDown = useCallback((e: React.MouseEvent) => {
    if (e.button === 0) {
      setIsDragging(true);
      setDragStart({ x: e.clientX - translateX, y: e.clientY - translateY });
    }
  }, [translateX, translateY]);

  const handleMouseMove = useCallback((e: React.MouseEvent) => {
    if (isDragging) {
      setTranslateX(e.clientX - dragStart.x);
      setTranslateY(e.clientY - dragStart.y);
    }
  }, [isDragging, dragStart]);

  const handleMouseUp = useCallback(() => {
    setIsDragging(false);
  }, []);

  // Touch drag handlers (no zoom)
  const handleTouchStart = useCallback((e: React.TouchEvent) => {
    if (e.touches.length !== 1) return;
    e.preventDefault();
    const t = e.touches[0];
    setIsDragging(true);
    setDragStart({ x: t.clientX - translateX, y: t.clientY - translateY });
  }, [translateX, translateY]);

  const handleTouchMove = useCallback((e: React.TouchEvent) => {
    if (!isDragging || e.touches.length !== 1) return;
    e.preventDefault();
    const t = e.touches[0];
    setTranslateX(t.clientX - dragStart.x);
    setTranslateY(t.clientY - dragStart.y);
  }, [isDragging, dragStart]);

  const handleTouchEnd = useCallback(() => {
    setIsDragging(false);
  }, []);

  const fetchJobs = async () => {
    const jobsData = await apiClient.getJobs();
    setJobs(jobsData);
  };

  useEffect(() => {
    const fetchInitial = async () => {
      try {
        setLoading(true);
        await fetchJobs();
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch data');
      } finally {
        setLoading(false);
      }
    };

    fetchInitial();
    const interval = setInterval(() => {
      fetchJobs().catch(err => console.warn('Background refresh failed:', err));
    }, 5000);
    return () => clearInterval(interval);
  }, []);

  // Keep a stable SVG coordinate system. The SVG itself scales to the
  // available canvas, so the wheel remains usable on both mobile and desktop
  // without making the page scroll.
  const [clockDims] = useState<ClockDimensions>(() => {
    // Coordinate dimensions are intentionally independent of the viewport;
    // CSS scales the complete SVG into the responsive canvas below.
    return {
      size: 550,
      radius: 176, // 550 * 0.32
      eventRadius: 220, // 176 + 550 * 0.08
      centerX: 275,
      centerY: 275
    };
  });

  // Calculate events within ±12 hours
  const timeEvents = useMemo(() => {
    const events: TimeEvent[] = [];
    const now = currentTime;
    const twelveHoursAgo = new Date(now.getTime() - 12 * 60 * 60 * 1000);
    const twelveHoursLater = new Date(now.getTime() + 12 * 60 * 60 * 1000);

    jobs.forEach(job => {
      if (mirrorMode(job) !== 'sync') return;
      const syncActive = job.sync_phase === 'Syncing' || job.sync_phase === 'Cancelling';
      // Scheduled attempts only run when the schedule is enabled and no sync is active.
      if (!isZeroTime(job.next_attempt_at) && !syncActive && !job.paused) {
        const nextAttempt = new Date(job.next_attempt_at);
        // Only show upcoming attempts within the ±12-hour activity window.
        if (nextAttempt >= twelveHoursAgo && nextAttempt <= twelveHoursLater) {
          events.push({
            time: nextAttempt,
            type: 'nextAttempt',
            jobId: job.id,
            jobStatus: displaySyncPhase(job) ?? 'Waiting'
          });
        }
      }

      // Last success
      if (!isZeroTime(job.last_success_at)) {
        const lastSuccess = new Date(job.last_success_at);
        if (lastSuccess >= twelveHoursAgo && lastSuccess <= twelveHoursLater) {
          events.push({
            time: lastSuccess,
            type: 'lastSuccess',
            jobId: job.id,
            jobStatus: displaySyncPhase(job) ?? 'Waiting'
          });
        }
      }

      // Last failure
      if (!isZeroTime(job.last_failure_at)) {
        const lastFailure = new Date(job.last_failure_at);
        if (lastFailure >= twelveHoursAgo && lastFailure <= twelveHoursLater) {
          events.push({
            time: lastFailure,
            type: 'lastFailure',
            jobId: job.id,
            jobStatus: displaySyncPhase(job) ?? 'Waiting'
          });
        }
      }

      // Last attempt (only for running jobs - show all running regardless of time)
      if (!isZeroTime(job.last_attempt_at) && syncActive) {
        const lastAttempt = new Date(job.last_attempt_at);
        events.push({
          time: lastAttempt,
          type: 'lastAttempt',
          jobId: job.id,
          jobStatus: displaySyncPhase(job) ?? 'Waiting'
        });
      }
    });

    return events;
  }, [jobs, currentTime]);

  // Convert time to angle (0-360 degrees, 0 = 0 o'clock/midnight)
  const timeToAngle = (time: Date) => {
    const hours = time.getHours(); // 0-23 hours
    const minutes = time.getMinutes();
    return (hours * 15) + (minutes * 0.25); // 15 degrees per hour, 0.25 degrees per minute
  };

  // Get current time angle
  const currentAngle = timeToAngle(currentTime);

  // Keep exact time angles. Separate markers radially, then label only those
  // with enough room. Dense events remain individually available on hover.
  const calculateLabelPositions = useMemo(() => {
    const positions: Array<TimeEvent & {
      index: number; angle: number; radiusLevel: number;
      x: number; y: number; left: boolean; showLabel: boolean;
    }> = [];
    const sortedEvents = [...timeEvents].sort((a, b) => a.time.getTime() - b.time.getTime() || a.jobId.localeCompare(b.jobId));
    for (let index = 0; index < sortedEvents.length; index++) {
      const event = sortedEvents[index];
      const angle = timeToAngle(event.time);
      const radian = (angle - 90) * Math.PI / 180;
      let radiusLevel = 0;
      let x: number, y: number;
      do {
        const radius = clockDims.eventRadius + 45 + radiusLevel * stepping_radius;
        x = Math.cos(radian) * radius;
        y = Math.sin(radian) * radius;
        if (!positions.some(other => Math.hypot(other.x - x, other.y - y) < 24)) break;
        radiusLevel++;
      } while (true);
      positions.push({ ...event, index, angle, radiusLevel, x, y, left: x < 0, showLabel: false });
    }
    const labels: Array<{ x: number; y: number; width: number }> = [];
    for (const position of positions) {
      const width = position.jobId.length * 7.5 + 4;
      const x = position.left ? position.x - 16 - width : position.x + 16;
      const y = position.y - 9;
      const overlapsMarker = positions.some(other => other.x + 12 > x && other.x - 12 < x + width && other.y + 12 > y && other.y - 12 < y + 18);
      const overlapsLabel = labels.some(other => other.x < x + width && other.x + other.width > x && other.y < y + 18 && other.y + 18 > y);
      position.showLabel = !overlapsMarker && !overlapsLabel;
      if (position.showLabel) labels.push({ x, y, width });
    }
    return positions;
  }, [timeEvents, clockDims.eventRadius]);

  // Fit the complete layout, including labels, rather than clipping outward
  // lanes against a fixed viewBox. Zoom and Reset are relative to this fit.
  const clockViewBox = useMemo(() => {
    let minX = -220, minY = -220, maxX = 220, maxY = 220;
    for (const point of calculateLabelPositions) {
      const labelWidth = point.showLabel ? point.jobId.length * 7.5 + 24 : 14;
      minX = Math.min(minX, point.x - (point.left ? labelWidth : 14));
      maxX = Math.max(maxX, point.x + (point.left ? 14 : labelWidth));
      minY = Math.min(minY, point.y - 14);
      maxY = Math.max(maxY, point.y + 14);
    }
    const center = clockDims.centerX + 200;
    return `${center + minX - 24} ${center + minY - 24} ${maxX - minX + 48} ${maxY - minY + 48}`;
  }, [calculateLabelPositions, clockDims.centerX]);

  // Sync activity and schedule controls are independent of publication conditions.
  const totalJobs = jobs.length;
  const runningJobs = jobs.filter(j => j.sync_phase === 'Syncing' || j.sync_phase === 'Cancelling').length;
  const waitingJobs = jobs.filter(j => displaySyncPhase(j) === 'Waiting').length;
  const pausedJobs = jobs.filter(j => displaySyncPhase(j) === 'Paused').length;
  const failedJobs = jobs.filter(j => j.last_action_status === 'Failed').length;

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading overview...</div>
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

  const getEventColor = (type: TimeEvent['type'], jobStatus?: string) => {
    switch (type) {
      case 'nextAttempt':
        return '#eab308'; // yellow-500
      case 'lastSuccess': return '#22c55e'; // green-500
      case 'lastFailure': return '#ef4444'; // red-500
      case 'lastAttempt': return '#3b82f6'; // blue-500 (only for running jobs now)
      default: return '#6b7280'; // gray-500
    }
  };

  const getEventName = (type: TimeEvent['type']) => {
    switch (type) {
      case 'nextAttempt': return 'Next Attempt';
      case 'lastSuccess': return 'Last Success';
      case 'lastFailure': return 'Last Failure';
      case 'lastAttempt': return 'Last Attempt';
      default: return 'Unknown';
    }
  };

  return (
    <div className="h-full min-h-0 p-4 sm:p-6 flex flex-col gap-4 overflow-hidden">
      {/* Page title */}
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-2">
        <h1 className="text-lg font-bold">Sync Activity</h1>
        <div role="group" aria-label="Overview display style" className="flex rounded-lg border border-border bg-card p-1">
          {([['timeline', 'Timeline', GanttChart], ['clock', 'Clock', Clock3]] as const).map(([id, label, Icon]) => (
            <button key={id} type="button" aria-pressed={view === id} onClick={() => changeView(id)}
              className={`inline-flex items-center gap-1.5 rounded px-3 py-1.5 text-xs ${view === id ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-accent'}`}>
              <Icon className="h-3.5 w-3.5" aria-hidden="true" />{label}
            </button>
          ))}
        </div>
      </div>

      {view === 'timeline' ? <SyncTimeline jobs={jobs} now={currentTime} onNavigateToJob={onNavigateToJob} /> : <>


      {/* B. 24-hour sync activity clock */}
      <div className="rounded-lg border bg-card flex-1 min-h-0 flex flex-col overflow-hidden">
        <div className="px-4 py-3 border-b flex flex-wrap justify-between items-center gap-x-4 gap-y-2 shrink-0">
          <span className="text-xs text-muted-foreground">Total mirrors: <span className="font-semibold tabular-nums text-foreground">{totalJobs}</span></span>
          <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-1 text-xs text-muted-foreground">
            <span className="uppercase tracking-wide text-[10px]">Status:</span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-blue-500 inline-block" /> Active syncs <span className="font-semibold tabular-nums text-foreground">{runningJobs}</span></span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-yellow-500 inline-block" /> Waiting <span className="font-semibold tabular-nums text-foreground">{waitingJobs}</span></span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-orange-500 inline-block" /> Paused <span className="font-semibold tabular-nums text-foreground">{pausedJobs}</span></span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-red-500 inline-block" /> Last Sync Failed <span className="font-semibold tabular-nums text-foreground">{failedJobs}</span></span>
          </div>
        </div>
        <div className="flex-1 min-h-0 flex justify-center p-2 sm:p-4" style={{ overscrollBehavior: 'contain' }}>
          <div
            ref={canvasRef}
            className="relative overflow-hidden border rounded-lg bg-muted/20 overscroll-none touch-none select-none"
            style={{
              width: '100%',
              height: '100%',
              cursor: isDragging ? 'grabbing' : 'grab',
              overscrollBehavior: 'contain'
            }}
            onMouseDown={handleMouseDown}
            onMouseMove={handleMouseMove}
            onMouseUp={handleMouseUp}
            onMouseLeave={handleMouseUp}
            onTouchStart={handleTouchStart}
            onTouchMove={handleTouchMove}
            onTouchEnd={handleTouchEnd}
            onTouchCancel={handleTouchEnd}
          >
            <div
              className="transition-transform duration-200 ease-out"
              style={{
                transform: `translate(${translateX}px, ${translateY}px) scale(${scale})`,
                transformOrigin: 'center center',
                width: '100%',
                height: '100%',
                display: 'flex',
                justifyContent: 'center',
                alignItems: 'center'
              }}
            >
              <svg
                width="100%"
                height="100%"
                className="drop-shadow-lg max-w-full max-h-full"
                viewBox={clockViewBox}
                aria-label="24-hour synchronization events"
              >
                {/* Background gradient */}
                <defs>
                  <radialGradient id="clockGradient" cx="50%" cy="50%" r="50%">
                    <stop offset="0%" stopColor="hsl(var(--card))" />
                    <stop offset="100%" stopColor="hsl(var(--muted))" />
                  </radialGradient>
                  <filter id="shadow" x="-50%" y="-50%" width="200%" height="200%">
                    <feDropShadow dx="0" dy="1" stdDeviation="2" floodOpacity="0.1" />
                  </filter>
                </defs>

                {/* Clock background */}
                <circle
                  cx={clockDims.centerX + 200}
                  cy={clockDims.centerY + 200}
                  r={clockDims.radius + 12}
                  fill="url(#clockGradient)"
                  filter="url(#shadow)"
                />

                {/* Clock face */}
                <circle
                  cx={clockDims.centerX + 200}
                  cy={clockDims.centerY + 200}
                  r={clockDims.radius}
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="4"
                  className="text-border opacity-60"
                />

                {/* Major hour markers and labels (24h) */}
                {[0, 6, 12, 18].map((hour) => {
                  const angle = hour * 15 - 90; // -90 to start from top, 15 degrees per hour
                  const radian = (angle * Math.PI) / 180;
                  const labelX = clockDims.centerX + 200 + Math.cos(radian) * (clockDims.radius - 30);
                  const labelY = clockDims.centerY + 200 + Math.sin(radian) * (clockDims.radius - 30);
                  const markerInnerX = clockDims.centerX + 200 + Math.cos(radian) * (clockDims.radius - 20);
                  const markerInnerY = clockDims.centerY + 200 + Math.sin(radian) * (clockDims.radius - 20);
                  const markerOuterX = clockDims.centerX + 200 + Math.cos(radian) * clockDims.radius;
                  const markerOuterY = clockDims.centerY + 200 + Math.sin(radian) * clockDims.radius;

                  return (
                    <g key={hour}>
                      {/* Hour marker */}
                      <line
                        x1={markerInnerX}
                        y1={markerInnerY}
                        x2={markerOuterX}
                        y2={markerOuterY}
                        stroke="currentColor"
                        strokeWidth="4"
                        className="text-foreground"
                        strokeLinecap="round"
                      />
                      {/* Hour label */}
                      <text
                        x={labelX}
                        y={labelY}
                        textAnchor="middle"
                        dominantBaseline="central"
                        className="text-lg font-bold fill-current"
                        style={{ fontSize: clockDims.size > 500 ? '20px' : '16px' }}
                      >
                        {hour.toString().padStart(2, '0')}
                      </text>
                    </g>
                  );
                })}

                {/* Minor hour markers (24h) */}
                {Array.from({ length: 24 }, (_, i) => i).filter(h => ![0, 6, 12, 18].includes(h)).map((hour) => {
                  const angle = hour * 15 - 90; // 15 degrees per hour
                  const radian = (angle * Math.PI) / 180;
                  const markerInnerX = clockDims.centerX + 200 + Math.cos(radian) * (clockDims.radius - 10);
                  const markerInnerY = clockDims.centerY + 200 + Math.sin(radian) * (clockDims.radius - 10);
                  const markerOuterX = clockDims.centerX + 200 + Math.cos(radian) * clockDims.radius;
                  const markerOuterY = clockDims.centerY + 200 + Math.sin(radian) * clockDims.radius;

                  // Different stroke width for intermediate major hours (3, 9, 15, 21)
                  const isMidHour = [3, 9, 15, 21].includes(hour);

                  return (
                    <g key={hour}>
                      <line
                        x1={markerInnerX}
                        y1={markerInnerY}
                        x2={markerOuterX}
                        y2={markerOuterY}
                        stroke="currentColor"
                        strokeWidth={isMidHour ? "3" : "1"}
                        className="text-muted-foreground opacity-50"
                      />
                      {/* Optional: show small labels for intermediate major hours */}
                      {isMidHour && clockDims.size > 500 && (
                        <text
                          x={clockDims.centerX + 200 + Math.cos(radian) * (clockDims.radius - 25)}
                          y={clockDims.centerY + 200 + Math.sin(radian) * (clockDims.radius - 25)}
                          textAnchor="middle"
                          dominantBaseline="central"
                          className="text-xs fill-current text-muted-foreground"
                          style={{ fontSize: '12px' }}
                        >
                          {hour.toString().padStart(2, '0')}
                        </text>
                      )}
                    </g>
                  );
                })}

                {/* Current time hand */}
                <g>
                  <line
                    x1={clockDims.centerX + 200}
                    y1={clockDims.centerY + 200}
                    x2={clockDims.centerX + 200 + Math.cos((currentAngle - 90) * Math.PI / 180) * (clockDims.radius - 60)}
                    y2={clockDims.centerY + 200 + Math.sin((currentAngle - 90) * Math.PI / 180) * (clockDims.radius - 60)}
                    stroke="currentColor"
                    strokeWidth="6"
                    className="text-primary"
                    strokeLinecap="round"
                    filter="url(#shadow)"
                  />

                  {/* Center time display background */}
                  <circle
                    cx={clockDims.centerX + 200}
                    cy={clockDims.centerY + 200}
                    r="35"
                    fill="hsl(var(--card))"
                    filter="url(#shadow)"
                    stroke="hsl(var(--border))"
                    strokeWidth="2"
                  />

                  {/* Current time text */}
                  <text
                    x={clockDims.centerX + 200}
                    y={clockDims.centerY + 200 - 8}
                    textAnchor="middle"
                    dominantBaseline="central"
                    className="fill-current text-foreground font-bold"
                    style={{ fontSize: clockDims.size > 500 ? '16px' : '12px' }}
                  >
                    {currentTime.toLocaleTimeString('en-US', {
                      hour: '2-digit',
                      minute: '2-digit',
                      hour12: false
                    })}
                  </text>

                  {/* Date text */}
                  <text
                    x={clockDims.centerX + 200}
                    y={clockDims.centerY + 200 + 10}
                    textAnchor="middle"
                    dominantBaseline="central"
                    className="fill-current text-muted-foreground"
                    style={{ fontSize: clockDims.size > 500 ? '10px' : '8px' }}
                  >
                    {currentTime.toLocaleDateString('en-US', {
                      month: 'short',
                      day: 'numeric'
                    })}
                  </text>

                  {/* Center dot */}
                  <circle
                    cx={clockDims.centerX + 200}
                    cy={clockDims.centerY + 200}
                    r="3"
                    fill="currentColor"
                    className="text-primary"
                  />
                </g>

                {/* Job events */}
                {calculateLabelPositions.map((eventPos) => {
                  const angle = eventPos.angle - 90; // -90 to start from top
                  const radian = (angle * Math.PI) / 180;

                  // Calculate label position with radius level offset (primary position)
                  const baseLabelRadius = clockDims.eventRadius + 45;
                  const levelOffset = eventPos.radiusLevel * stepping_radius; // 24px between levels
                  const labelRadius = baseLabelRadius + levelOffset;
                  const labelX = clockDims.centerX + 200 + Math.cos(radian) * labelRadius;
                  const labelY = clockDims.centerY + 200 + Math.sin(radian) * labelRadius;

                  // Calculate event position (aligned with label)
                  const eventX = labelX;
                  const eventY = labelY;

                  const isHovered = hoveredEvent?.jobId === eventPos.jobId && hoveredEvent?.type === eventPos.type;

                  return (
                    <g
                      key={`${eventPos.jobId}-${eventPos.type}-${eventPos.index}`}
                      onMouseEnter={(e: React.MouseEvent) => {
                        setHoveredEvent(eventPos);
                        setTooltipPosition({
                          x: Math.max(8, Math.min(e.clientX + 15, window.innerWidth - 300)),
                          y: Math.max(8, Math.min(e.clientY - 10, window.innerHeight - 170))
                        });
                      }}
                      onMouseMove={(e: React.MouseEvent) => {
                        if (hoveredEvent) {
                          setTooltipPosition({
                            x: Math.max(8, Math.min(e.clientX + 15, window.innerWidth - 300)),
                            y: Math.max(8, Math.min(e.clientY - 10, window.innerHeight - 170))
                          });
                        }
                      }}
                      onMouseLeave={() => {
                        setHoveredEvent(null);
                        setTooltipPosition(null);
                      }}
                      onClick={() => {
                        if (onNavigateToJob) {
                          onNavigateToJob(eventPos.jobId);
                        }
                      }}
                      className="cursor-pointer"
                      role="button"
                      tabIndex={0}
                      aria-label={`${eventPos.jobId}: ${getEventName(eventPos.type)}`}
                      onKeyDown={event => {
                        if (event.key === 'Enter' || event.key === ' ') {
                          event.preventDefault();
                          onNavigateToJob?.(eventPos.jobId);
                        }
                      }}
                    >
                      <title>{`${eventPos.jobId}: ${getEventName(eventPos.type)} — ${eventPos.time.toLocaleString()}`}</title>
                      {/* Event line to clock */}
                      <line
                        x1={clockDims.centerX + 200 + Math.cos(radian) * clockDims.radius}
                        y1={clockDims.centerY + 200 + Math.sin(radian) * clockDims.radius}
                        x2={eventX}
                        y2={eventY}
                        stroke={getEventColor(eventPos.type, eventPos.jobStatus)}
                        strokeWidth={isHovered ? "4" : "3"}
                        className={isHovered ? "opacity-80" : "opacity-50"}
                        strokeDasharray={eventPos.type === 'nextAttempt' ? "6,3" : "none"}
                      />

                      {/* Event circle */}
                      <circle
                        cx={eventX}
                        cy={eventY}
                        r={isHovered ? "12" : "9"}
                        fill={getEventColor(eventPos.type, eventPos.jobStatus)}
                        stroke="white"
                        strokeWidth="3"
                        filter="url(#shadow)"
                        className="transition-all duration-200"
                      />

                      {/* Job name label with level-based styling - positioned next to event circle */}
                      {eventPos.showLabel && <text
                        x={labelX + (eventPos.left ? -16 : 16)}
                        y={labelY}
                        textAnchor={eventPos.left ? "end" : "start"}
                        dominantBaseline="central"
                        className={`fill-current text-sm font-mono transition-all duration-200 ${isHovered ? 'text-foreground opacity-100' : 'text-muted-foreground opacity-80'
                          }`}
                        style={{
                          fontSize: '12px',
                          fontWeight: isHovered ? 'bold' : 'normal'
                        }}
                      >
                        {eventPos.jobId}
                      </text>}
                    </g>
                  );
                })}
              </svg>
            </div>
          </div>
        </div>

        <p className="px-4 pb-2 text-center text-xs text-muted-foreground">Hover over an event for details; click to open its mirror. Drag to pan.</p>

        {/* Canvas Controls */}
        <div className="shrink-0 flex flex-wrap items-center justify-center gap-2 pb-4">
          <button
            onClick={handleZoomIn}
            className="flex items-center gap-1 px-3 py-1 text-sm bg-secondary text-secondary-foreground rounded-md hover:bg-secondary/80 transition-colors"
            title="Zoom In"
          >
            <ZoomIn className="h-4 w-4" />
            Zoom In
          </button>
          <button
            onClick={handleZoomOut}
            className="flex items-center gap-1 px-3 py-1 text-sm bg-secondary text-secondary-foreground rounded-md hover:bg-secondary/80 transition-colors"
            title="Zoom Out"
          >
            <ZoomOut className="h-4 w-4" />
            Zoom Out
          </button>
          <button
            onClick={handleReset}
            className="flex items-center gap-1 px-3 py-1 text-sm bg-secondary text-secondary-foreground rounded-md hover:bg-secondary/80 transition-colors"
            title="Reset View"
          >
            <RotateCcw className="h-4 w-4" />
            Reset
          </button>
          <div className="flex items-center gap-1 px-3 py-1 text-sm bg-muted text-muted-foreground rounded-md">
            <Move className="h-4 w-4" />
            {Math.round(scale * 100)}%
          </div>
        </div>
      </div>

      </>}

      {/* Floating Tooltip */}
      {view === 'clock' && hoveredEvent && tooltipPosition && (
        <div
          className="fixed z-50 pointer-events-none"
          style={{
            left: tooltipPosition.x,
            top: tooltipPosition.y,
          }}
        >
          <div className="bg-popover text-popover-foreground p-3 rounded-lg shadow-lg border border-border max-w-xs animate-in fade-in-0 zoom-in-95 duration-200">
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <div
                  className="w-3 h-3 rounded-full"
                  style={{ backgroundColor: getEventColor(hoveredEvent.type, hoveredEvent.jobStatus) }}
                />
                <span className="font-mono text-sm font-medium">{hoveredEvent.jobId}</span>
              </div>
              <div className="text-sm space-y-1">
                <div className="font-medium">{getEventName(hoveredEvent.type)}</div>
                <div className="text-muted-foreground font-mono text-xs">
                  <RelativeTime date={hoveredEvent.time.toISOString()} />
                </div>
                <div className="flex items-center gap-1">
                  <StatusBadge status={hoveredEvent.jobStatus} />
                </div>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
