import type { MirrorCondition } from '@/types';
import { StatusBadge } from '@/components/status-badge';

export function ConditionBadges({ conditions }: { conditions: MirrorCondition[] }) {
  const active = (conditions ?? []).filter(condition => condition.status === 'True');
  if (active.length === 0) return <span className="text-muted-foreground">—</span>;
  return <div className="flex flex-wrap justify-center gap-1">{active.map(condition => (
    <span key={condition.type} title={`${condition.reason}: ${condition.message}`}>
      <StatusBadge status={condition.type} />
    </span>
  ))}</div>;
}
