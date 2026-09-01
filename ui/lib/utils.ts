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

export function formatRelativeTime(dateString: string): string {
  if (!dateString || dateString === '0001-01-01T00:00:00Z') {
    return 'Never';
  }

  const date = new Date(dateString);
  if (isNaN(date.getTime()) || date.getTime() <= 0) {
    return 'Never';
  }

  const now = new Date();
  const diffMs = now.getTime() - date.getTime();

  // Handle future dates (e.g. next_attempt_at).
  if (diffMs < 0) {
    const futureDiffMs = Math.abs(diffMs);
    const parts = formatDuration(futureDiffMs);
    return `in ${parts}`;
  }

  if (diffMs < 1000) return 'Just now';

  const parts = formatDuration(diffMs);
  return `${parts} ago`;
}

export function formatDuration(ms: number): string {
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

  if (parts.length === 0) {
    return 'just now';
  }

  if (parts.length === 1) {
    return parts[0];
  } else if (parts.length === 2) {
    return parts.join(' and ');
  } else {
    const lastPart = parts.pop();
    return parts.join(', ') + ' and ' + lastPart;
  }
}

export function formatRFC3339(dateString: string): string {
  if (!dateString || dateString === '0001-01-01T00:00:00Z') {
    return 'Never';
  }

  const date = new Date(dateString);
  if (isNaN(date.getTime()) || date.getTime() <= 0) {
    return 'Never';
  }

  // Get local timezone offset in minutes
  const timezoneOffset = date.getTimezoneOffset();
  const offsetHours = Math.floor(Math.abs(timezoneOffset) / 60);
  const offsetMinutes = Math.abs(timezoneOffset) % 60;
  const offsetSign = timezoneOffset <= 0 ? '+' : '-';
  const timezoneString = `${offsetSign}${offsetHours.toString().padStart(2, '0')}:${offsetMinutes.toString().padStart(2, '0')}`;

  // Format date and time in local timezone
  const year = date.getFullYear();
  const month = (date.getMonth() + 1).toString().padStart(2, '0');
  const day = date.getDate().toString().padStart(2, '0');
  const hours = date.getHours().toString().padStart(2, '0');
  const minutes = date.getMinutes().toString().padStart(2, '0');
  const seconds = date.getSeconds().toString().padStart(2, '0');

  return `${year}-${month}-${day} ${hours}:${minutes}:${seconds}${timezoneString}`;
}

// Local ISO-8601-like timestamp for compact tables. The browser's timezone
// is used and the numeric offset keeps the value unambiguous (for example,
// 09-01T13:42:00+08:00 in Asia/Shanghai).
export function formatLocalISO8601(dateString: string): string {
  if (!dateString || dateString === '0001-01-01T00:00:00Z') return 'Never';
  const date = new Date(dateString);
  if (Number.isNaN(date.getTime()) || date.getTime() <= 0) return 'Never';
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  const hours = String(date.getHours()).padStart(2, '0');
  const minutes = String(date.getMinutes()).padStart(2, '0');
  const seconds = String(date.getSeconds()).padStart(2, '0');
  const offsetMinutes = -date.getTimezoneOffset();
  const sign = offsetMinutes >= 0 ? '+' : '-';
  const absolute = Math.abs(offsetMinutes);
  const offsetHours = String(Math.floor(absolute / 60)).padStart(2, '0');
  const offsetRemainder = String(absolute % 60).padStart(2, '0');
  return `${month}-${day}T${hours}:${minutes}:${seconds}${sign}${offsetHours}:${offsetRemainder}`;
}

export function formatCountdown(ms: number): string {
  const totalSeconds = Math.max(0, Math.floor(ms / 1000));
  const hours = String(Math.floor(totalSeconds / 3600)).padStart(2, '0');
  const minutes = String(Math.floor((totalSeconds % 3600) / 60)).padStart(2, '0');
  const seconds = String(totalSeconds % 60).padStart(2, '0');
  return `${hours}:${minutes}:${seconds}`;
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
    case 'Publishing':
      return 'bg-blue-600 border-blue-200 hover:bg-blue-700';
    case 'Initializing':
      return 'bg-yellow-600 border-yellow-200 hover:bg-yellow-700';
    case 'Degraded':
      return 'text-red-600 bg-red-50 border-red-200 hover:bg-red-100';
    case 'Pending':
      return 'text-gray-600 bg-gray-100 border-gray-200 hover:bg-gray-200';
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
