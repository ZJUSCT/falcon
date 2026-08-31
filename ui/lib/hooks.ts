import { useState, useEffect } from 'react';

import { formatDuration } from '@/lib/utils';

/**
 * Provides the current time, updated every second. Powers the clock wheel
 * hand and live relative-time displays.
 */
export function useCurrentTime() {
  const [currentTime, setCurrentTime] = useState(new Date());

  useEffect(() => {
    const interval = setInterval(() => {
      setCurrentTime(new Date());
    }, 1000);

    return () => clearInterval(interval);
  }, []);

  return currentTime;
}

/**
 * Live relative time for an RFC3339 timestamp; re-renders every second.
 * Zero-value timestamps ("0001-01-01T00:00:00Z", the API's "never") render
 * as `Never`.
 */
export function useRelativeTime(dateString: string) {
  const currentTime = useCurrentTime();

  if (!dateString || dateString === '0001-01-01T00:00:00Z') {
    return 'Never';
  }

  const date = new Date(dateString);
  if (isNaN(date.getTime()) || date.getTime() <= 0) {
    return 'Never';
  }

  const diffMs = currentTime.getTime() - date.getTime();

  if (diffMs < 0) {
    return `in ${formatDuration(Math.abs(diffMs))}`;
  }

  if (diffMs < 1000) return 'Just now';

  return `${formatDuration(diffMs)} ago`;
}
