import { useState, useEffect } from 'react';

/**
 * Custom hook that provides the current time and updates it every second
 * This is useful for real-time relative time displays
 */
export function useCurrentTime() {
  const [currentTime, setCurrentTime] = useState(new Date());

  useEffect(() => {
    const interval = setInterval(() => {
      setCurrentTime(new Date());
    }, 1000); // Update every second

    return () => clearInterval(interval);
  }, []);

  return currentTime;
}

/**
 * Custom hook that provides real-time relative time formatting
 * Updates every second to show live timing
 */
export function useRelativeTime(dateString: string) {
  const currentTime = useCurrentTime();
  
  if (!dateString || dateString === '0001-01-01T00:00:00Z') {
    return 'Never';
  }
  
  const date = new Date(dateString);
  const diffMs = currentTime.getTime() - date.getTime();
  
  // Handle future dates
  if (diffMs < 0) {
    const futureDiffMs = Math.abs(diffMs);
    const parts = formatDuration(futureDiffMs);
    return `in ${parts}`;
  }
  
  if (diffMs < 1000) return 'Just now';
  
  const parts = formatDuration(diffMs);
  return `${parts} ago`;
}

function formatDuration(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);
  const days = Math.floor(hours / 24);
  
  const remainingHours = hours % 24;
  const remainingMinutes = minutes % 60;
  const remainingSeconds = seconds % 60;
  
  const parts: string[] = [];
  
  if (days > 0) {
    parts.push(`${days}d`);
  }
  
  if (remainingHours > 0) {
    parts.push(`${remainingHours}h`);
  }
  
  if (remainingMinutes > 0) {
    parts.push(`${remainingMinutes}min`);
  }
  
  // Only show seconds if no days are shown (to avoid too much detail for long periods)
  if (days === 0 && remainingSeconds > 0) {
    parts.push(`${remainingSeconds}s`);
  }
  
  // If no parts, it means less than 1 second
  if (parts.length === 0) {
    return 'just now';
  }
  
  // Join with commas and "and" for the last item
  if (parts.length === 1) {
    return parts[0];
  } else {
    return parts.join(' ');
  }
}