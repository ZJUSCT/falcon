'use client';

import React, { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import { RelativeTime } from '@/components/relative-time';
import { StatusBadge } from '@/components/status-badge';
import { apiClient } from '@/lib/api';
import { Job, Action, Worker, QueueResponse } from '@/types';
import { ZoomIn, ZoomOut, RotateCcw, Move } from 'lucide-react';

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

const stepping_radius = 12;

export function OverviewView({ onNavigateToJob }: OverviewViewProps = {}) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [queue, setQueue] = useState<QueueResponse | null>(null);
  const [activeActions, setActiveActions] = useState<Action[]>([]);
  const [recentActions, setRecentActions] = useState<Action[]>([]);
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

  const fetchAll = async () => {
    try {
      const [jobsData, workersData, queueData, actionsData, recentData] = await Promise.all([
        apiClient.getJobs(),
        apiClient.getWorkers(),
        apiClient.getQueue(),
        apiClient.getActions(),
        apiClient.getRecentActions(100),
      ]);
      setJobs(jobsData);
      setWorkers(workersData);
      setQueue(queueData);
      setActiveActions(actionsData);
      setRecentActions(recentData);
      setError(null);
    } catch (err) {
      console.warn('Background refresh failed:', err);
    }
  };

  useEffect(() => {
    const fetchInitial = async () => {
      try {
        setLoading(true);
        const [jobsData, workersData, queueData, actionsData, recentData] = await Promise.all([
          apiClient.getJobs(),
          apiClient.getWorkers(),
          apiClient.getQueue(),
          apiClient.getActions(),
          apiClient.getRecentActions(100),
        ]);
        setJobs(jobsData);
        setWorkers(workersData);
        setQueue(queueData);
        setActiveActions(actionsData);
        setRecentActions(recentData);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch data');
      } finally {
        setLoading(false);
      }
    };

    fetchInitial();
    const interval = setInterval(fetchAll, 5000);
    return () => clearInterval(interval);
  }, []);

  // Responsive clock dimensions - enlarged for main view
  const getClockDimensions = (): ClockDimensions => {
    // Much larger sizing for prominent display
    const size = Math.min(700, Math.max(400, window?.innerWidth > 1200 ? 650 : window?.innerWidth > 768 ? 550 : 400));
    const radius = size * 0.32; // Larger radius for better visibility
    const eventRadius = radius + size * 0.08;

    return {
      size,
      radius,
      eventRadius,
      centerX: size / 2,
      centerY: size / 2
    };
  };

  const [clockDims, setClockDims] = useState<ClockDimensions>(() => {
    // Default dimensions for SSR, matching new larger calculations
    return {
      size: 550,
      radius: 176, // 550 * 0.32
      eventRadius: 220, // 176 + 550 * 0.08
      centerX: 275,
      centerY: 275
    };
  });

  // Update dimensions on mount and resize
  useEffect(() => {
    const updateDimensions = () => {
      setClockDims(getClockDimensions());
    };

    updateDimensions();
    window.addEventListener('resize', updateDimensions);
    return () => window.removeEventListener('resize', updateDimensions);
  }, []);

  // Calculate events within ±12 hours
  const timeEvents = useMemo(() => {
    const events: TimeEvent[] = [];
    const now = currentTime;
    const twelveHoursAgo = new Date(now.getTime() - 12 * 60 * 60 * 1000);
    const twelveHoursLater = new Date(now.getTime() + 12 * 60 * 60 * 1000);

    jobs.forEach(job => {
      // Next attempt (skip for running jobs, show all scheduled regardless of time)
      if (job.next_attempt_at && job.next_attempt_at !== '0001-01-01T00:00:00Z' && job.status !== 'Running') {
        const nextAttempt = new Date(job.next_attempt_at);
        // Show all scheduled jobs regardless of time, others within 12h window
        if (job.status === 'Scheduled' || (nextAttempt >= twelveHoursAgo && nextAttempt <= twelveHoursLater)) {
          events.push({
            time: nextAttempt,
            type: 'nextAttempt',
            jobId: job.id,
            jobStatus: job.status
          });
        }
      }

      // Last success
      if (job.last_success_at && job.last_success_at !== '0001-01-01T00:00:00Z') {
        const lastSuccess = new Date(job.last_success_at);
        if (lastSuccess >= twelveHoursAgo && lastSuccess <= twelveHoursLater) {
          events.push({
            time: lastSuccess,
            type: 'lastSuccess',
            jobId: job.id,
            jobStatus: job.status
          });
        }
      }

      // Last failure
      if (job.last_failure_at && job.last_failure_at !== '0001-01-01T00:00:00Z') {
        const lastFailure = new Date(job.last_failure_at);
        if (lastFailure >= twelveHoursAgo && lastFailure <= twelveHoursLater) {
          events.push({
            time: lastFailure,
            type: 'lastFailure',
            jobId: job.id,
            jobStatus: job.status
          });
        }
      }

      // Last attempt (only for running jobs - show all running regardless of time)
      if (job.last_attempt_at && job.last_attempt_at !== '0001-01-01T00:00:00Z' && job.status === 'Running') {
        const lastAttempt = new Date(job.last_attempt_at);
        events.push({
          time: lastAttempt,
          type: 'lastAttempt',
          jobId: job.id,
          jobStatus: job.status
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

  // Shorten job name for display
  const shortenJobName = (jobId: string) => {
    return jobId;
  };



  const calculateLabelPositions = useMemo(() => {
    if (timeEvents.length === 0) return [];

    const sortedEvents = [...timeEvents].sort((a, b) => a.time.getTime() - b.time.getTime());

    const buckets: Array<Array<{
      event: TimeEvent;
      radiusLevel: number;
      x: number;
      y: number;
      width: number;
      height: number;
      angle: number;
    }>> = Array(360).fill(null).map(() => []);

    const BASE_RADIUS = clockDims.eventRadius + 45;
    const CHAR_WIDTH = 8;

    const getLabelWidth = (jobId: string): number => {
      return CHAR_WIDTH + jobId.length * CHAR_WIDTH;
    };

    const circleRadius = 12;

    const getBodyRectangle = (angle: number, radius: number, labelWidth: number): { x: number, y: number, width: number, height: number } => {
      const rad = (angle - 90) * Math.PI / 180;
      return {
        x: Math.cos(rad) * radius - circleRadius,
        y: Math.sin(rad) * radius - circleRadius,
        width: labelWidth + circleRadius * 2,
        height: circleRadius * 2
      };
    };

    const getBodyRectangleDegrees = (
      rect: BodyRectangle,
      angle: number,
    ): [number, number] => {
      const rad = (angle - 90) * Math.PI / 180;

      const x = rect.x;
      const y = rect.y;
      const width = rect.width;
      const height = rect.height;

      const corners: Array<[number, number]> = [
        [x, y],
        [x + width, y],
        [x, y + height],
        [x + width, y + height],
      ];

      const norm = (d: number) => ((d % 360) + 360) % 360;
      const toUiDeg = (px: number, py: number) =>
        norm(Math.atan2(py, px) * 180 / Math.PI + 90);

      const angs = corners.map(([px, py]) => toUiDeg(px, py)).sort((a, b) => a - b);

      let maxGap = -1;
      let idx = -1;
      for (let i = 0; i < angs.length; i++) {
        const a = angs[i];
        const b = i === angs.length - 1 ? angs[0] + 360 : angs[i + 1];
        const gap = b - a;
        if (gap > maxGap) {
          maxGap = gap;
          idx = i;
        }
      }

      const start = angs[(idx + 1) % angs.length];
      const end = angs[idx];

      return [Math.floor(start), Math.ceil(end)];
    };

    type BodyRectangle = ReturnType<typeof getBodyRectangle>;

    const doLabelsOverlap = (rect1: BodyRectangle, rect2: BodyRectangle): boolean => {

      if (rect1.x < rect2.x + rect2.width &&
        rect1.x + rect1.width > rect2.x &&
        rect1.y < rect2.y + rect2.height &&
        rect1.y + rect1.height > rect2.y) {
        return true;
      }

      return false;
    };


    sortedEvents.forEach((event) => {
      const angle = timeToAngle(event.time);
      const labelWidth = getLabelWidth(event.jobId);

      let radiusLevel = 0;
      let hasOverlap = true;

      let currentRadius = BASE_RADIUS;

      let bodyRectangle = { x: 0, y: 0, width: 0, height: 0 };
      let bodyRectangleDegrees: [number, number] = [0, 0];

      while (hasOverlap) {
        hasOverlap = false;

        currentRadius = BASE_RADIUS + radiusLevel * stepping_radius;

        bodyRectangle = getBodyRectangle(angle, currentRadius, labelWidth);
        bodyRectangleDegrees = getBodyRectangleDegrees(bodyRectangle, angle);

        const exactBucketIndex = Math.floor(angle) % 360;
        const surroundingBuckets = [];

        for (let offset = -bodyRectangleDegrees[1]; offset <= bodyRectangleDegrees[1]; offset++) {
          const checkBucketIndex = (exactBucketIndex + offset + 360) % 360;
          surroundingBuckets.push(checkBucketIndex);
        }

        const uniqueBuckets = Array.from(new Set(surroundingBuckets));

        for (const checkBucketIndex of uniqueBuckets) {
          const bucket = buckets[checkBucketIndex];

          for (const existingLabel of bucket) {
            const existingBodyRectangle = { x: existingLabel.x, y: existingLabel.y, width: existingLabel.width, height: existingLabel.height };

            if (doLabelsOverlap(bodyRectangle, existingBodyRectangle)) {
              hasOverlap = true;
              break;
            }
          }

          if (hasOverlap) break;
        }

        if (hasOverlap) {
          radiusLevel++;
        }
      }

      if (bodyRectangleDegrees[0] <= bodyRectangleDegrees[1]) {
      for (let i = bodyRectangleDegrees[0]; i <= bodyRectangleDegrees[1]; i++) {
          buckets[i % 360].push({ event, radiusLevel, x: bodyRectangle.x, y: bodyRectangle.y, width: bodyRectangle.width, height: bodyRectangle.height, angle: angle });
        }
      } else {
        for (let i = bodyRectangleDegrees[0]; i < 360; i++) {
          buckets[i % 360].push({ event, radiusLevel, x: bodyRectangle.x, y: bodyRectangle.y, width: bodyRectangle.width, height: bodyRectangle.height, angle: angle });
        }
        for (let i = 0; i <= bodyRectangleDegrees[1]; i++) {
          buckets[i % 360].push({ event, radiusLevel, x: bodyRectangle.x, y: bodyRectangle.y, width: bodyRectangle.width, height: bodyRectangle.height, angle: angle });
        }
      }
    });

    // Convert back to the original format
    const positions = timeEvents.map((event, index) => {
      const angle = timeToAngle(event.time);
      const exactBucketIndex = Math.floor(angle) % 360;
      const bucket = buckets[exactBucketIndex];

      // Find the radius level for this event
      const existingLabel = bucket.find(label =>
        label.event.jobId === event.jobId && label.event.type === event.type
      );
      const radiusLevel = existingLabel ? existingLabel.radiusLevel : 0;

      return {
        ...event,
        index,
        angle,
        radiusLevel
      };
    });

    return positions;
  }, [timeEvents, currentTime, clockDims.eventRadius]);

  // Derived stats
  const workersOnline = workers.filter(w => w.status === 'Online').length;
  const runningJobs = jobs.filter(j => j.status === 'Running').length;
  const queueDepth = queue?.queue?.length ?? 0;
  const succeeded24h = recentActions.filter(a => a.status === 'Succeeded').length;
  const failed24h = recentActions.filter(a => a.status === 'Failed').length;
  const runningActions = activeActions.filter(a => a.status === 'Running');
  const recentFailures = recentActions.filter(a => a.status === 'Failed').slice(0, 10);

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
        return jobStatus === 'Scheduled' ? '#8b5cf6' : '#eab308'; // purple-500 for Scheduled, yellow-500 for others
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
    <div className="p-6 space-y-6">
      {/* A. Page header + LIVE indicator */}
      <div>
        <div className="flex items-center gap-2 mb-1">
          <span className="w-2 h-2 rounded-full bg-green-500 animate-pulse-dot" />
          <span className="text-green-500 text-[10px] font-semibold tracking-widest uppercase">LIVE</span>
        </div>
        <h1 className="text-lg font-bold">Overview</h1>
        <p className="text-xs text-muted-foreground">Cluster health at a glance</p>
      </div>

      {/* B. Stats cards row */}
      <div className="flex gap-4 overflow-x-auto pb-2">
        <div className="min-w-[140px] rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Workers Online</div>
          <div className="text-xl font-bold tabular-nums text-green-500">{workersOnline}<span className="text-xs text-muted-foreground font-normal">/{workers.length}</span></div>
        </div>
        <div className="min-w-[140px] rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Running</div>
          <div className="text-xl font-bold tabular-nums text-blue-500">{runningJobs}</div>
        </div>
        <div className="min-w-[140px] rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Queue Depth</div>
          <div className="text-xl font-bold tabular-nums text-amber-500">{queueDepth}</div>
        </div>
        <div className="min-w-[140px] rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Succeeded (24h)</div>
          <div className="text-xl font-bold tabular-nums text-green-500">{succeeded24h}</div>
        </div>
        <div className="min-w-[140px] rounded-lg border bg-card p-3">
          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Failed (24h)</div>
          <div className="text-xl font-bold tabular-nums text-red-500">{failed24h}</div>
        </div>
      </div>

      {/* C. Schedule Timeline (Clock) */}
      <div className="rounded-lg border bg-card">
        <div className="px-4 py-3 border-b flex justify-between items-center">
          <span className="text-sm font-semibold">Schedule Timeline</span>
          <div className="flex gap-3 text-xs text-muted-foreground">
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-purple-500 inline-block" /> Scheduled</span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-yellow-500 inline-block" /> Next</span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-green-500 inline-block" /> Success</span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-red-500 inline-block" /> Failure</span>
            <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-blue-500 inline-block" /> Running</span>
          </div>
        </div>
        <div className="flex justify-center p-4 sm:p-8" style={{ overscrollBehavior: 'contain' }}>
          <div
            ref={canvasRef}
            className="relative overflow-hidden border rounded-lg bg-muted/20 overscroll-none touch-none select-none"
            style={{
              width: '100%',
              height: clockDims.size + 300,
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
                width={clockDims.size + 400}
                height={clockDims.size + 400}
                className="drop-shadow-lg"
                viewBox={`0 0 ${clockDims.size + 400} ${clockDims.size + 400}`}
                style={{ overflow: 'visible' }}
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
                  const levelOffset = eventPos.radiusLevel * stepping_radius; // 30px between levels for larger clock
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
                          x: e.clientX + 15,
                          y: e.clientY - 10
                        });
                      }}
                      onMouseMove={(e: React.MouseEvent) => {
                        if (hoveredEvent) {
                          setTooltipPosition({
                            x: e.clientX + 15,
                            y: e.clientY - 10
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
                    >
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
                      <text
                        x={labelX + (isHovered ? 18 : 15)} // Offset to the right of the larger circle
                        y={labelY}
                        textAnchor="start"
                        dominantBaseline="central"
                        className={`fill-current text-sm font-mono transition-all duration-200 ${isHovered ? 'text-foreground opacity-100' : 'text-muted-foreground opacity-80'
                          }`}
                        style={{
                          fontSize: isHovered ? '16px' : '12px',
                          fontWeight: isHovered ? 'bold' : 'normal'
                        }}
                      >
                        {shortenJobName(eventPos.jobId)}
                      </text>

                      {/* Debug: Show collision detection rectangle
                      <rect
                        x={eventX - 12} // circleRadius
                        y={eventY - 12} // circleRadius
                        width={eventPos.jobId.length * 8 + 32} // labelWidth + circleRadius * 2
                        height={24} // circleRadius * 2
                        fill="none"
                        stroke="red"
                        strokeWidth="1"
                        strokeDasharray="3,3"
                        opacity="0.5"
                      /> */}


                    </g>
                  );
                })}
              </svg>
            </div>
          </div>
        </div>

        {/* Canvas Controls */}
        <div className="flex justify-center gap-2 pb-4">
          <button
            onClick={handleZoomIn}
            className="flex items-center gap-1 px-3 py-1 text-sm bg-primary text-primary-foreground rounded-md hover:bg-primary/90 transition-colors"
            title="Zoom In"
          >
            <ZoomIn className="h-4 w-4" />
            Zoom In
          </button>
          <button
            onClick={handleZoomOut}
            className="flex items-center gap-1 px-3 py-1 text-sm bg-primary text-primary-foreground rounded-md hover:bg-primary/90 transition-colors"
            title="Zoom Out"
          >
            <ZoomOut className="h-4 w-4" />
            Zoom Out
          </button>
          <button
            onClick={handleReset}
            className="flex items-center gap-1 px-3 py-1 text-sm bg-secondary text-secondary-foreground rounded-md hover:bg-secondary/90 transition-colors"
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

      {/* D. Workers grid */}
      <div>
        <h3 className="text-[10px] uppercase tracking-widest text-muted-foreground font-semibold mb-3">Workers</h3>
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {workers.map((worker) => (
            <div key={worker.name} className="rounded-lg border bg-card p-4 space-y-2">
              <div className="flex items-center gap-2">
                <span className={`w-2 h-2 rounded-full ${worker.status === 'Online' ? 'bg-green-500' : 'bg-red-500'}`} />
                <span className="text-sm font-medium truncate">{worker.name}</span>
              </div>
              {worker.labels && Object.keys(worker.labels).length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {Object.entries(worker.labels).map(([k, v]) => (
                    <span key={k} className="inline-flex items-center px-2 py-0.5 rounded-full text-[10px] font-medium bg-primary/10 text-primary">
                      {k}={v}
                    </span>
                  ))}
                </div>
              )}
              <div className="text-xs text-muted-foreground">
                Running: {worker.running_actions?.length ?? 0}
              </div>
            </div>
          ))}
          {workers.length === 0 && (
            <div className="col-span-full text-sm text-muted-foreground text-center py-4">No workers registered</div>
          )}
        </div>
      </div>

      {/* E. Currently Running + Recent Failures */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Currently Running */}
        <div className="rounded-lg border bg-card">
          <div className="px-4 py-3 border-b">
            <span className="text-sm font-semibold">Currently Running</span>
          </div>
          <div className="p-4 space-y-1 max-h-64 overflow-y-auto">
            {runningActions.length > 0 ? runningActions.map((action) => (
              <div
                key={action.id}
                className="flex items-center justify-between text-xs cursor-pointer hover:bg-muted/50 rounded px-3 py-2"
                onClick={() => onNavigateToJob?.(action.job_id)}
              >
                <span className="font-mono text-xs truncate">
                  {action.job_id} <span className="text-muted-foreground">&rarr;</span> {action.worker_name || '?'}
                </span>
                <span className="text-xs text-muted-foreground flex-shrink-0 ml-2">
                  <RelativeTime date={action.started_at || action.created_at || action.updated_at} />
                </span>
              </div>
            )) : (
              <div className="text-sm text-muted-foreground text-center py-4">No running actions</div>
            )}
          </div>
        </div>

        {/* Recent Failures */}
        <div className="rounded-lg border bg-card">
          <div className="px-4 py-3 border-b">
            <span className="text-sm font-semibold">Recent Failures</span>
          </div>
          <div className="p-4 space-y-1 max-h-64 overflow-y-auto">
            {recentFailures.length > 0 ? recentFailures.map((action) => (
              <div
                key={action.id}
                className="flex items-center justify-between text-xs cursor-pointer hover:bg-muted/50 rounded px-3 py-2"
                onClick={() => onNavigateToJob?.(action.job_id)}
              >
                <span className="font-mono text-xs truncate text-red-500">
                  {action.job_id}
                </span>
                <span className="text-xs text-muted-foreground flex-shrink-0 ml-2">
                  <RelativeTime date={action.finished_at || action.updated_at} />
                </span>
              </div>
            )) : (
              <div className="text-sm text-muted-foreground text-center py-4">No recent failures</div>
            )}
          </div>
        </div>
      </div>

      {/* Floating Tooltip */}
      {hoveredEvent && tooltipPosition && (
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
