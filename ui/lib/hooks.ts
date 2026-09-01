import { useState, useEffect } from 'react';

import { formatDuration } from '@/lib/utils';
import { apiClient } from '@/lib/api';
import { UsageResponse } from '@/types';

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

/**
 * Cluster storage usage (GET /api/usage), polled every 30 s — much slower
 * than the jobs poll (5 s) because the aggregation is expensive on the
 * backend. Same mount-fetch + setInterval + cleanup pattern as the views'
 * jobs polling. The endpoint 404s when the usage feature is not deployed,
 * and it can fail like any network call; both cases degrade silently to
 * "no data" (console.warn only): null is returned until the first success,
 * and a failure after a success keeps the last good snapshot.
 */
export function useUsage() {
  const [usage, setUsage] = useState<UsageResponse | null>(null);

  useEffect(() => {
    const fetchUsage = () => {
      apiClient
        .getUsage()
        .then(setUsage)
        .catch(err => console.warn('Background usage refresh failed (feature disabled or unreachable):', err));
    };

    fetchUsage();
    const interval = setInterval(fetchUsage, 30000);
    return () => clearInterval(interval);
  }, []);

  return usage;
}
