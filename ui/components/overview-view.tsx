'use client';

import React, { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { StatusBadge } from '@/components/status-badge';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { Job } from '@/types';
import { Clock, Eye, ZoomIn, ZoomOut, RotateCcw, Move } from 'lucide-react';

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

export function OverviewView({ onNavigateToJob }: OverviewViewProps = {}) {
  const [jobs, setJobs] = useState<Job[]>([]);
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
    try {
      const data = await apiClient.getJobs();
      setJobs(data);
      setError(null);
    } catch (err) {
      console.warn('Background jobs refresh failed:', err);
    }
  };

  useEffect(() => {
    const fetchInitialJobs = async () => {
      try {
        setLoading(true);
        const data = await apiClient.getJobs();
        setJobs(data);
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch jobs');
      } finally {
        setLoading(false);
      }
    };

    fetchInitialJobs();
    const interval = setInterval(fetchJobs, 5000);
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


  const stepping_radius = 120;

  // Calculate label positions with bucket-based collision detection
  const calculateLabelPositions = useMemo(() => {
    if (timeEvents.length === 0) return [];

    // Sort events by time (oldest first)
    const sortedEvents = [...timeEvents].sort((a, b) => a.time.getTime() - b.time.getTime());

    // Create 360 buckets (one for each degree)
    const buckets: Array<Array<{ event: TimeEvent; radiusLevel: number }>> = Array(360).fill(null).map(() => []);

    const BASE_RADIUS = clockDims.eventRadius + 45; // Base radius for labels
    const CHAR_WIDTH = 32; // Approximate width per character in pixels
    const MIN_LABEL_WIDTH = 60; // Minimum label width in pixels

    const getLabelWidth = (jobId: string): number => {
      const textWidth = jobId.length * CHAR_WIDTH;
      return Math.max(textWidth, MIN_LABEL_WIDTH);
    };

    // Function to check if two labels overlap (horizontal labels with circle detection)
    const doLabelsOverlap = (angle1: number, radius1: number, labelWidth1: number, angle2: number, radius2: number, labelWidth2: number): boolean => {
      // Convert angles to radians
      const rad1 = (angle1 - 90) * Math.PI / 180; // -90 to start from top
      const rad2 = (angle2 - 90) * Math.PI / 180;
      
      // Calculate positions of the circles (event dots)
      const circleX1 = Math.cos(rad1) * radius1;
      const circleY1 = Math.sin(rad1) * radius1;
      const circleX2 = Math.cos(rad2) * radius2;
      const circleY2 = Math.sin(rad2) * radius2;
      
      // Circle radius for overlap detection
      const circleRadius = 12; // Event circle radius
            
      // Calculate positions of the text labels (horizontal, offset from circles)
      const textOffset = 15; // Distance from circle to text start
      const labelHeight = circleRadius * 2 + 20 + 10; // Circle height + padding
      
      // Calculate label rectangles (horizontal, no rotation)
      const rect1 = {
        x: circleX1 - textOffset, // Start at text offset from circle
        y: circleY1 - labelHeight / 2, // Center vertically on circle
        width: labelWidth1 + circleRadius,
        height: labelHeight
      };
      
      const rect2 = {
        x: circleX2 - textOffset, // Start at text offset from circle
        y: circleY2 - labelHeight / 2, // Center vertically on circle
        width: labelWidth2 + circleRadius,
        height: labelHeight
      };
      
      // Check rectangle-to-rectangle overlap (AABB for horizontal rectangles)
      if (rect1.x < rect2.x + rect2.width &&
          rect1.x + rect1.width > rect2.x &&
          rect1.y < rect2.y + rect2.height &&
          rect1.y + rect1.height > rect2.y) {
        return true;
      }
      
      return false;
    };

    // Function to get all buckets that a label spans (considering complete label rectangle)
    const getLabelBuckets = (angle: number, labelWidth: number): number[] => {
      const buckets: number[] = [];
      const textOffset = 15; // Distance from circle to text start
      const circleRadius = 15; // Event circle radius
      
      // Calculate the total width of the label area (10px left of circle to 10px right of text)
      const totalWidth = 10 + circleRadius + textOffset + labelWidth + 10;
      
      // Convert total width to degrees
      const labelSpanDegrees = (totalWidth / BASE_RADIUS) * (180 / Math.PI);
      const halfSpan = labelSpanDegrees / 2;
      
      // Add 10 degrees of extra span for better spacing
      const extraSpan = 10;
      const totalHalfSpan = halfSpan + extraSpan;
      
      const startAngle = (angle - totalHalfSpan + 360) % 360;
      const endAngle = (angle + totalHalfSpan) % 360;
      
      if (startAngle <= endAngle) {
        // Normal case: label spans within 0-360 range
        for (let i = Math.floor(startAngle); i <= Math.ceil(endAngle); i++) {
          buckets.push(i % 360);
        }
      } else {
        // Wrapping case: label spans across 0/360 boundary
        for (let i = Math.floor(startAngle); i < 360; i++) {
          buckets.push(i);
        }
        for (let i = 0; i <= Math.ceil(endAngle); i++) {
          buckets.push(i);
        }
      }
      
      return buckets;
    };

    // Process each event in chronological order
    sortedEvents.forEach((event) => {
      const angle = timeToAngle(event.time);
      const labelWidth = getLabelWidth(event.jobId);
      const labelBuckets = getLabelBuckets(angle, labelWidth);
      
      // Find the minimum radius level that doesn't cause overlaps
      let radiusLevel = 0;
      let hasOverlap = true;
      
      while (hasOverlap) {
        hasOverlap = false;
        const currentRadius = BASE_RADIUS + radiusLevel * stepping_radius; // 30px between levels
        
        // Check surrounding buckets for potential overlaps
        const exactBucketIndex = Math.floor(angle) % 360;
        const surroundingBuckets = [];
        
        // Check buckets within the label span plus some buffer
        for (let offset = -10; offset <= 10; offset++) {
          const checkBucketIndex = (exactBucketIndex + offset + 360) % 360;
          surroundingBuckets.push(checkBucketIndex);
        }
        
        // Remove duplicates
        const uniqueBuckets = Array.from(new Set(surroundingBuckets));
        
        for (const checkBucketIndex of uniqueBuckets) {
          const bucket = buckets[checkBucketIndex];
          
          // Check against all existing labels in this bucket
          for (const existingLabel of bucket) {
            const existingAngle = timeToAngle(existingLabel.event.time);
            const existingRadius = BASE_RADIUS + existingLabel.radiusLevel * stepping_radius;
            const existingLabelWidth = getLabelWidth(existingLabel.event.jobId);
            
            console.log(event.jobId, existingLabel.event.jobId ,angle, currentRadius, labelWidth, existingAngle, existingRadius, existingLabelWidth);
            
            if (doLabelsOverlap(angle, currentRadius, labelWidth, existingAngle, existingRadius, existingLabelWidth)) {
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
      
      // Add this label only to the bucket corresponding to its exact angle
      const exactBucketIndex = Math.floor(angle) % 360;
      buckets[exactBucketIndex].push({ event, radiusLevel });
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
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-3xl font-bold tracking-tight">Overview</h2>
          <p className="text-muted-foreground">
            Complete 24-hour job activity overview
          </p>
        </div>
        <div className="flex items-center gap-2 text-sm text-muted-foreground font-mono">
          <Clock className="h-4 w-4" />
          Current: {currentTime.toLocaleTimeString()}
        </div>
      </div>

      <div className="space-y-8">
        {/* Clock View - Now Full Width and Enlarged with Canvas Controls */}
        <Card>
          <CardHeader className="text-center">
            <div className="flex items-center justify-center gap-2">
              <Eye className="h-6 w-6" />
              <CardTitle className="text-2xl">Activity Clock</CardTitle>
            </div>
            <CardDescription className="text-base">
              Complete daily job activity timeline
            </CardDescription>
          </CardHeader>
          
          <CardContent className="flex justify-center p-4 sm:p-8" style={{ overscrollBehavior: 'contain' }}>
            <div 
              ref={canvasRef}
              className="relative overflow-hidden border rounded-lg bg-muted/20 overscroll-none touch-none select-none"
              style={{ 
                width: clockDims.size + 300, 
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
                      <feDropShadow dx="0" dy="1" stdDeviation="2" floodOpacity="0.1"/>
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
                    const levelOffset = eventPos.radiusLevel * 30; // 30px between levels for larger clock
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
                          className={`fill-current text-sm font-mono transition-all duration-200 ${
                            isHovered ? 'text-foreground opacity-100' : 'text-muted-foreground opacity-80'
                          }`}
                          style={{ 
                            fontSize: clockDims.size > 500 ? (14 - eventPos.radiusLevel * 0.5) + 'px' : (12 - eventPos.radiusLevel * 0.5) + 'px',
                            fontWeight: isHovered ? 'bold' : 'normal'
                          }}
                        >
                          {shortenJobName(eventPos.jobId)}
                        </text>
                        

                      </g>
                    );
                  })}
                </svg>
              </div>
            </div>
          </CardContent>

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

        </Card>

        {/* Information Panels Below Clock */}
        <div className="grid gap-6 lg:grid-cols-3 md:grid-cols-2">

          {/* Legend */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-lg">Legend</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="space-y-4">
                <div className="flex items-center gap-3">
                  <div className="w-5 h-5 rounded-full bg-purple-500 shadow-sm"></div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">Next Attempt (Scheduled)</div>
                    <div className="text-xs text-muted-foreground">Scheduled status jobs</div>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="w-5 h-5 rounded-full bg-yellow-500 shadow-sm"></div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">Next Attempt (Waiting)</div>
                    <div className="text-xs text-muted-foreground">Waiting status jobs</div>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="w-5 h-5 rounded-full bg-green-500 shadow-sm"></div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">Last Success</div>
                    <div className="text-xs text-muted-foreground">Completed runs</div>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="w-5 h-5 rounded-full bg-red-500 shadow-sm"></div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">Last Failure</div>
                    <div className="text-xs text-muted-foreground">Failed runs</div>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="w-5 h-5 rounded-full bg-blue-500 shadow-sm"></div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">Running Attempt</div>
                    <div className="text-xs text-muted-foreground">Currently running</div>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="w-2 h-5 bg-primary rounded shadow-sm"></div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">Current Time</div>
                    <div className="text-xs text-muted-foreground">Clock hand</div>
                  </div>
                </div>
              </div>
            </CardContent>
          </Card>

          {/* Event Summary */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-lg">Today's Summary</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="space-y-3">
                <div className="grid grid-cols-2 gap-2">
                  <div className="text-center p-2 rounded-lg bg-purple-50 border border-purple-200">
                    <div className="text-lg font-bold text-purple-700">
                      {timeEvents.filter(e => e.type === 'nextAttempt' && e.jobStatus === 'Scheduled').length}
                    </div>
                    <div className="text-xs text-purple-600 font-medium">Scheduled</div>
                  </div>
                  <div className="text-center p-2 rounded-lg bg-yellow-50 border border-yellow-200">
                    <div className="text-lg font-bold text-yellow-700">
                      {timeEvents.filter(e => e.type === 'nextAttempt' && e.jobStatus !== 'Scheduled').length}
                    </div>
                    <div className="text-xs text-yellow-600 font-medium">Other Next</div>
                  </div>
                </div>
                <div className="grid grid-cols-2 gap-2">
                  <div className="text-center p-2 rounded-lg bg-green-50 border border-green-200">
                    <div className="text-lg font-bold text-green-700">
                      {timeEvents.filter(e => e.type === 'lastSuccess').length}
                    </div>
                    <div className="text-xs text-green-600 font-medium">Success</div>
                  </div>
                  <div className="text-center p-2 rounded-lg bg-red-50 border border-red-200">
                    <div className="text-lg font-bold text-red-700">
                      {timeEvents.filter(e => e.type === 'lastFailure').length}
                    </div>
                    <div className="text-xs text-red-600 font-medium">Failures</div>
                  </div>
                </div>
                <div className="text-center p-2 rounded-lg bg-blue-50 border border-blue-200">
                  <div className="text-2xl font-bold text-blue-700">
                    {timeEvents.filter(e => e.type === 'lastAttempt' && e.jobStatus === 'Running').length}
                  </div>
                  <div className="text-sm text-blue-600 font-medium">Running Jobs</div>
                </div>
              </div>
            </CardContent>
          </Card>

          {/* Recent Events */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-lg">Recent Events</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="space-y-2 max-h-80 overflow-y-auto">
                {timeEvents.length > 0 ? (
                  timeEvents
                    .sort((a, b) => a.time.getTime() - b.time.getTime())
                    .map((event, index) => (
                      <div 
                        key={`${event.jobId}-${event.type}-${event.time.getTime()}`}
                        className={`flex items-center gap-2 p-2 rounded-lg transition-all duration-200 cursor-pointer ${
                          hoveredEvent?.jobId === event.jobId && hoveredEvent?.type === event.type
                            ? 'bg-primary/10 shadow-sm' 
                            : 'hover:bg-muted/50'
                        }`}
                        onMouseEnter={(e: React.MouseEvent) => {
                          setHoveredEvent(event);
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
                            onNavigateToJob(event.jobId);
                          }
                        }}
                      >
                        <div 
                          className="w-3 h-3 rounded-full flex-shrink-0 shadow-sm transition-all duration-200"
                          style={{ 
                            backgroundColor: getEventColor(event.type, event.jobStatus),
                            transform: hoveredEvent?.jobId === event.jobId && hoveredEvent?.type === event.type ? 'scale(1.3)' : 'scale(1)'
                          }}
                        ></div>
                        <div className="flex-1 min-w-0">
                          <div className="font-mono text-xs font-medium truncate">{shortenJobName(event.jobId)}</div>
                          <div className="text-xs text-muted-foreground">{getEventName(event.type)}</div>
                        </div>
                      </div>
                    ))
                ) : (
                  <div className="text-center text-muted-foreground py-6">
                    <Clock className="h-6 w-6 mx-auto mb-2 opacity-50" />
                    <div className="text-xs">No recent events</div>
                  </div>
                )}
              </div>
            </CardContent>
          </Card>
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

