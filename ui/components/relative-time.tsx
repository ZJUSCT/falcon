'use client';

import { useRelativeTime } from '@/lib/hooks';
import { cn, formatRFC3339 } from '@/lib/utils';

interface RelativeTimeProps {
  date: string;
  className?: string;
  variant?: 'stacked' | 'inline' | 'compact';
}

export function RelativeTime({ date, className, variant = 'stacked' }: RelativeTimeProps) {
  const relativeTime = useRelativeTime(date);
  const rfc3339Time = formatRFC3339(date);
  
  if (rfc3339Time === 'Never') {
    return <span className={cn('font-mono', className)}>Never</span>;
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
