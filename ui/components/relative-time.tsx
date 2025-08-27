'use client';

import { useRelativeTime } from '@/lib/hooks';
import { formatRFC3339 } from '@/lib/utils';

interface RelativeTimeProps {
  date: string;
  className?: string;
}

export function RelativeTime({ date, className }: RelativeTimeProps) {
  const relativeTime = useRelativeTime(date);
  const rfc3339Time = formatRFC3339(date);
  
  if (rfc3339Time === 'Never') {
    return (
      <span className={`font-mono ${className || ''}`}>
        Never
      </span>
    );
  }
  
  return (
    <span className={`font-mono ${className || ''}`} title={new Date(date).toLocaleString()}>
      {rfc3339Time} <br/> <span className="text-s text-muted-foreground">{relativeTime}</span>
    </span>
  );
}
