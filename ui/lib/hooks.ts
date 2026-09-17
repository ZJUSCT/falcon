import { useState, useEffect } from 'react';

import { apiClient } from '@/lib/api';
import { StorageResponse, UsageResponse } from '@/types';

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

/**
 * Per-node ZFS inventory (GET /api/storage), polled every 30 s like
 * useUsage — the same agent-side aggregation backs both endpoints, so it
 * is equally expensive. Same degradation contract: the endpoint 404s when
 * the usage feature is not deployed, and any other failure behaves like a
 * network error; both cases degrade silently to "no data" (console.warn
 * only): null is returned until the first success, and a failure after a
 * success keeps the last good snapshot.
 */
export function useStorage() {
  const [storage, setStorage] = useState<StorageResponse | null>(null);

  useEffect(() => {
    const fetchStorage = () => {
      apiClient
        .getStorage()
        .then(setStorage)
        .catch(err => console.warn('Background storage refresh failed (feature disabled or unreachable):', err));
    };

    fetchStorage();
    const interval = setInterval(fetchStorage, 30000);
    return () => clearInterval(interval);
  }, []);

  return storage;
}
