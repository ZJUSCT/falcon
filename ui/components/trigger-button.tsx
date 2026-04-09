'use client';

import { useState, useRef, useEffect } from 'react';
import { apiClient } from '@/lib/api';
import { Play, Loader2, X, Pause, CalendarClock } from 'lucide-react';

interface TriggerButtonProps {
  jobId: string;
  jobStatus: string;
  onSuccess?: () => void;
  size?: 'sm' | 'md';
  variant?: 'button' | 'icon';
}

export function TriggerButton({
  jobId,
  jobStatus,
  onSuccess,
  size = 'md',
  variant = 'button',
}: TriggerButtonProps) {
  const [loading, setLoading] = useState(false);
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null);
  const [showTimePicker, setShowTimePicker] = useState(false);
  const [timeInput, setTimeInput] = useState('');
  const popoverRef = useRef<HTMLDivElement>(null);

  const isWaiting = jobStatus === 'Waiting';
  const isScheduled = jobStatus === 'Scheduled';
  const isRunning = jobStatus === 'Running';
  const isPaused = jobStatus === 'Paused';
  const canPause = isWaiting || isScheduled || isRunning;
  const canInteract = isWaiting || isScheduled || isPaused;

  useEffect(() => {
    if (!showTimePicker) return;
    const handler = (e: MouseEvent) => {
      if (popoverRef.current && !popoverRef.current.contains(e.target as Node)) {
        setShowTimePicker(false);
      }
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [showTimePicker]);

  const flash = (type: 'success' | 'error', text: string) => {
    setMessage({ type, text });
    setTimeout(() => setMessage(null), 3000);
  };

  const doAction = async (action: () => Promise<void>) => {
    setLoading(true);
    setMessage(null);
    try {
      await action();
      if (onSuccess) setTimeout(onSuccess, 500);
    } catch (err) {
      flash('error', err instanceof Error ? err.message : 'Failed');
    } finally {
      setLoading(false);
    }
  };

  const handleTriggerNow = (e: React.MouseEvent) => {
    e.stopPropagation();
    doAction(async () => {
      await apiClient.triggerJobNow(jobId);
      flash('success', 'Triggered!');
    });
  };

  const handleCancelSchedule = (e: React.MouseEvent) => {
    e.stopPropagation();
    doAction(async () => {
      await apiClient.deleteJobFromQueue(jobId);
      flash('success', 'Schedule cancelled');
    });
  };

  const handlePause = (e: React.MouseEvent) => {
    e.stopPropagation();
    doAction(async () => {
      await apiClient.pauseJob(jobId, true);
      flash('success', 'Job paused');
    });
  };

  const handleUnpause = (e: React.MouseEvent) => {
    e.stopPropagation();
    doAction(async () => {
      await apiClient.pauseJob(jobId, false);
      flash('success', 'Job resumed');
    });
  };

  const handleSetTime = (e: React.FormEvent) => {
    e.preventDefault();
    if (!timeInput) return;
    const iso = new Date(timeInput).toISOString();
    setShowTimePicker(false);
    doAction(async () => {
      await apiClient.setNextAttempt(jobId, iso);
      flash('success', 'Next attempt updated');
    });
  };

  const openTimePicker = (e: React.MouseEvent) => {
    e.stopPropagation();
    const def = new Date(Date.now() + 3600_000);
    const pad = (n: number) => String(n).padStart(2, '0');
    setTimeInput(
      `${def.getFullYear()}-${pad(def.getMonth() + 1)}-${pad(def.getDate())}T${pad(def.getHours())}:${pad(def.getMinutes())}`
    );
    setShowTimePicker(true);
  };

  if (!canInteract && !canPause && variant === 'icon') return null;

  const iconSize = size === 'sm' ? 'h-3 w-3' : 'h-4 w-4';
  const btnBase = `inline-flex items-center justify-center rounded-full transition-colors font-medium ${
    size === 'sm' ? 'px-2 py-1 text-xs' : 'px-3 py-2 text-sm'
  }`;

  const messageEl = message && (
    <div
      className={`absolute top-full left-1/2 -translate-x-1/2 mt-2 px-2 py-1 text-xs rounded whitespace-nowrap z-10 ${
        message.type === 'success'
          ? 'bg-green-100 text-green-800 border border-green-200'
          : 'bg-red-100 text-red-800 border border-red-200'
      }`}
    >
      {message.text}
    </div>
  );

  const timePickerEl = showTimePicker && (
    <div
      ref={popoverRef}
      onClick={e => e.stopPropagation()}
      className="absolute top-full right-0 mt-2 p-3 bg-popover border border-border rounded-lg shadow-lg z-20 space-y-2"
    >
      <form onSubmit={handleSetTime} className="flex items-end gap-2">
        <div>
          <label className="text-[11px] text-muted-foreground uppercase tracking-wide">
            Next sync at
          </label>
          <input
            type="datetime-local"
            value={timeInput}
            onChange={e => setTimeInput(e.target.value)}
            className="block mt-1 px-2 py-1 text-xs rounded border border-border bg-background text-foreground focus:outline-none focus:ring-2 focus:ring-primary/60"
          />
        </div>
        <button
          type="submit"
          className="px-3 py-1 text-xs rounded bg-primary text-primary-foreground hover:bg-primary/90 transition-colors"
        >
          Set
        </button>
      </form>
    </div>
  );

  // ----- icon variant (used in mirrors table) -----
  if (variant === 'icon') {
    return (
      <div className="relative flex items-center gap-1">
        {isWaiting && (
          <button onClick={handleTriggerNow} disabled={loading}
            className={`${btnBase} bg-green-600 hover:bg-green-700 text-white`} title="Trigger now">
            {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <Play className={iconSize} />}
          </button>
        )}
        {isScheduled && (
          <button onClick={handleCancelSchedule} disabled={loading}
            className={`${btnBase} bg-red-600 hover:bg-red-700 text-white`} title="Cancel schedule">
            {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <X className={iconSize} />}
          </button>
        )}
        {isPaused && (
          <button onClick={handleUnpause} disabled={loading}
            className={`${btnBase} bg-green-600 hover:bg-green-700 text-white`} title="Resume job">
            {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <Play className={iconSize} />}
          </button>
        )}
        {canPause && (
          <button onClick={handlePause} disabled={loading}
            className={`${btnBase} bg-orange-500 hover:bg-orange-600 text-white`} title="Pause job">
            <Pause className={iconSize} />
          </button>
        )}
        {canInteract && (
          <button onClick={openTimePicker} disabled={loading}
            className={`${btnBase} bg-muted hover:bg-muted/80 text-foreground`} title="Set next sync time">
            <CalendarClock className={iconSize} />
          </button>
        )}
        {messageEl}
        {timePickerEl}
      </div>
    );
  }

  // ----- button variant (used in mirror detail) -----
  return (
    <div className="relative flex items-center gap-2">
      {isWaiting && (
        <button onClick={handleTriggerNow} disabled={loading}
          className={`${btnBase} bg-green-600 hover:bg-green-700 text-white gap-2`}>
          {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <Play className={iconSize} />}
          Trigger Now
        </button>
      )}
      {isScheduled && (
        <button onClick={handleCancelSchedule} disabled={loading}
          className={`${btnBase} bg-red-600 hover:bg-red-700 text-white gap-2`}>
          {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <X className={iconSize} />}
          Cancel Schedule
        </button>
      )}
      {isPaused && (
        <button onClick={handleUnpause} disabled={loading}
          className={`${btnBase} bg-green-600 hover:bg-green-700 text-white gap-2`}>
          {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <Play className={iconSize} />}
          Resume
        </button>
      )}
      {canPause && (
        <button onClick={handlePause} disabled={loading}
          className={`${btnBase} bg-orange-500 hover:bg-orange-600 text-white gap-2`}>
          {loading ? <Loader2 className={`${iconSize} animate-spin`} /> : <Pause className={iconSize} />}
          Pause
        </button>
      )}
      {canInteract && (
        <button onClick={openTimePicker} disabled={loading}
          className={`${btnBase} bg-muted hover:bg-muted/80 text-foreground gap-2`}>
          <CalendarClock className={iconSize} />
          Set Time
        </button>
      )}
      {messageEl}
      {timePickerEl}
    </div>
  );
}
