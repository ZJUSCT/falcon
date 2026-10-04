import { Job, mirrorMode } from '@/types';

export function MirrorModeBadge({ job }: { job: Job }) {
  const mode = mirrorMode(job);
  if (mode === 'sync') return null;
  return (
    <span className="rounded bg-primary/10 px-1.5 py-0.5 text-[10px] font-mono text-primary">
      {mode === 'cache' ? 'Cache' : 'Proxy'}
    </span>
  );
}
