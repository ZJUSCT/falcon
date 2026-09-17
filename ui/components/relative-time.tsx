'use client';

import { useEffect, useState } from 'react';
import { useCurrentTime } from '@/lib/hooks';
import { cn, formatSignedDuration } from '@/lib/utils';
import { isZeroTime } from '@/types';

interface RelativeTimeProps {
  date: string;
  className?: string;
}

// One unified relative-time rendering: the text is the signed distance to
// now in .NET TimeSpan "c" shape ("+00:05:00", "-2.03:24:15"), ticking every
// second; the hover title carries the absolute local time for debugging.
// Zero-value timestamps (the API's "never") render as "Never".
export function RelativeTime({ date, className }: RelativeTimeProps) {
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);
  const currentTime = useCurrentTime();

  if (isZeroTime(date)) {
    return <span className={cn('font-mono', className)}>Never</span>;
  }

  const target = new Date(date);
  // Localized formatting differs between server and browser timezones;
  // defer the client-specific value until hydration has completed.
  if (!mounted) {
    return <span className={cn('font-mono whitespace-nowrap', className)}>—</span>;
  }

  return (
    <span
      className={cn('font-mono whitespace-nowrap', className)}
      suppressHydrationWarning
      title={target.toLocaleString()}
    >
      {formatSignedDuration(target.getTime() - currentTime.getTime())}
    </span>
  );
}
