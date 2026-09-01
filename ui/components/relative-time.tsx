'use client';

import { useEffect, useState } from 'react';
import { useCurrentTime, useRelativeTime } from '@/lib/hooks';
import { cn, formatRFC3339, formatLocalISO8601, formatCountdown } from '@/lib/utils';

interface RelativeTimeProps {
  date: string;
  className?: string;
  variant?: 'stacked' | 'inline' | 'compact' | 'absolute' | 'countdown';
}

export function RelativeTime({ date, className, variant = 'stacked' }: RelativeTimeProps) {
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);
  const relativeTime = useRelativeTime(date);
  const rfc3339Time = formatRFC3339(date);
  const currentTime = useCurrentTime();

  if (rfc3339Time === 'Never') {
    return <span className={cn('font-mono', className)}>Never</span>;
  }

  if (variant === 'absolute') {
    // Localized formatting differs between server and browser timezones;
    // defer the client-specific value until hydration has completed.
    return <span className={cn('font-mono whitespace-nowrap', className)} suppressHydrationWarning title={mounted ? new Date(date).toLocaleString() : undefined}>{mounted ? formatLocalISO8601(date) : '—'}</span>;
  }

  if (variant === 'countdown') {
    const target = new Date(date);
    const remaining = target.getTime() - currentTime.getTime();
    return <span className={cn('font-mono whitespace-nowrap', className)} title={target.toLocaleString()}>{remaining > 0 ? `in ${formatCountdown(remaining)}` : 'now'}</span>;
  }

  const title = new Date(date).toLocaleString();

  if (variant === 'compact') {
    return (
      <span className={cn('font-mono whitespace-nowrap', className)} title={title}>
        {relativeTime}
      </span>
    );
  }

  if (variant === 'inline') {
    return (
      <span className={cn('font-mono whitespace-nowrap', className)} title={title}>
        {rfc3339Time}
        <span className="mx-1 text-xs text-muted-foreground">•</span>
        <span className="text-xs text-muted-foreground">{relativeTime}</span>
      </span>
    );
  }

  return (
    <span className={cn('font-mono', className)} title={title}>
      {rfc3339Time}
      <br />
      <span className="text-xs text-muted-foreground">{relativeTime}</span>
    </span>
  );
}
