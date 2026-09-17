import { type ClassValue, clsx } from 'clsx';

export function cn(...inputs: ClassValue[]) {
  return clsx(inputs);
}

// Human-readable byte sizes (Mirrors "Size" column, detail "Storage Usage"
// card). 1024-based with units B/KB/MB/GB/TB/PB. Rules:
//   - null/undefined/NaN/infinite/negative input → null (callers render `—`).
//   - Under 1 KB the value is an exact count: integer bytes, no decimals
//     (e.g. "512 B"). 1024 B therefore renders as "1 KB".
//   - Otherwise the largest fitting unit is picked; the value gets one
//     decimal, dropped when the rounded value is a whole number or >= 100
//     (e.g. "1.5 KB", "128 GB", "999.9 KB" — never "128.0 GB").
export function formatBytes(bytes: number | null | undefined): string | null {
  if (bytes === null || bytes === undefined) return null;
  if (!Number.isFinite(bytes) || bytes < 0) return null;

  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }

  if (unit === 0) return `${bytes} B`;

  const rounded = Math.round(value * 10) / 10;
  const text = Number.isInteger(rounded) || rounded >= 100 ? rounded.toFixed(0) : rounded.toFixed(1);
  return `${text} ${units[unit]}`;
}

// Signed relative duration in the shape of .NET TimeSpan invariant format
// "c", prefixed with the direction: "+" for the future, "-" for the past.
// The days segment is omitted when zero, so five minutes ahead is
// "+00:05:00" and two days, three hours ago is "-2.03:00:00". Zero renders
// as "+00:00:00". The displayed text everywhere; hover titles (rendered by
// RelativeTime) carry the absolute local time for debugging.
export function formatSignedDuration(ms: number): string {
  const sign = ms < 0 ? '-' : '+';
  const totalSeconds = Math.floor(Math.abs(ms) / 1000);
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const hhmmss = [hours, minutes, seconds].map(n => String(n).padStart(2, '0')).join(':');
  return days > 0 ? `${sign}${days}.${hhmmss}` : `${sign}${hhmmss}`;
}

export function getStatusColor(status: string): string {
  switch (status) {
    // Legacy sync vocabulary (Mirror jobs).
    case 'Running':
      return 'bg-blue-600 border-blue-200 hover:bg-blue-700';
    case 'Succeeded':
      return 'bg-green-600 border-green-200 hover:bg-green-700';
    case 'Failed':
      return 'text-red-600 bg-red-50 border-red-200 hover:bg-red-100';
    case 'Waiting':
      return 'text-yellow-600 bg-yellow-50 border-yellow-200 hover:bg-yellow-100';
    case 'Scheduled':
      return 'bg-purple-600 border-purple-200 hover:bg-purple-700';
    case 'Paused':
      return 'text-orange-600 bg-orange-50 border-orange-200 hover:bg-orange-100';
    case 'Orphan':
      return 'text-gray-600 bg-gray-400 border-gray-200 hover:bg-gray-600';
    // Raw CR phases (ProxyMirror rows, detail views).
    case 'Ready':
      return 'bg-green-600 border-green-200 hover:bg-green-700';
    case 'Syncing':
    case 'Snapshotting':
    case 'Progressing':
    case 'Cancelling':
    case 'Publishing':
      return 'bg-blue-600 border-blue-200 hover:bg-blue-700';
    case 'Retrying':
    case 'Pending':
    case 'Initializing':
      return 'bg-yellow-600 border-yellow-200 hover:bg-yellow-700';
    case 'Degraded':
      return 'text-red-600 bg-red-50 border-red-200 hover:bg-red-100';
    case 'U':
      return 'bg-green-600 border-green-200 hover:bg-green-700';
    case 'S':
      return 'bg-blue-600 border-blue-200 hover:bg-blue-700';
    case 'D':
      return 'text-red-600 bg-red-50 border-red-200 hover:bg-red-100';
    case 'P':
      return 'text-orange-600 bg-orange-50 border-orange-200 hover:bg-orange-100';
    default:
      return 'text-gray-600 bg-gray-400 border-gray-200 hover:bg-gray-600';
  }
}
