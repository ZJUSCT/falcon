import { type ClassValue, clsx } from 'clsx';

export function cn(...inputs: ClassValue[]) {
  return clsx(inputs);
}

export function formatDate(dateString: string): string {
  if (!dateString || dateString === '0001-01-01T00:00:00Z') {
    return 'Never';
  }
  const date = new Date(dateString);
  return date.toLocaleString();
}

export function formatRelativeTime(dateString: string): string {
  if (!dateString || dateString === '0001-01-01T00:00:00Z') {
    return 'Never';
  }
  
  const date = new Date(dateString);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  
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

export function getStatusColor(status: string): string {
  switch (status) {
    case 'Running':
      return 'bg-blue-600 border-blue-200';
    case 'Succeeded':
      return 'bg-green-600 border-green-200';
    case 'Failed':
      return 'text-red-600 bg-red-50 border-red-200';
    case 'Waiting':
      return 'text-yellow-600 bg-yellow-50 border-yellow-200';
    case 'Scheduled':
      return 'bg-purple-600 border-purple-200';
    case 'Orphan':
      return 'bg-gray-600 border-gray-200';
    default:
      return 'text-gray-600 bg-gray-50 border-gray-200';
  }
}

export function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB', 'ZB', 'YB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}