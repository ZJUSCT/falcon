'use client';

import { useState } from 'react';
import { apiClient } from '@/lib/api';
import { Play, Loader2 } from 'lucide-react';

interface TriggerButtonProps {
  jobId: string;
  jobStatus: string;
  onSuccess?: () => void;
  size?: 'sm' | 'md';
  variant?: 'button' | 'icon';
}

export function TriggerButton({ 
  jobId, 
  jobStatus, 
  onSuccess, 
  size = 'md',
  variant = 'button'
}: TriggerButtonProps) {
  const [isTriggering, setIsTriggering] = useState(false);
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null);

  const canTrigger = jobStatus === 'Waiting';

  const handleTrigger = async (e: React.MouseEvent) => {
    e.stopPropagation(); // Prevent card click in jobs view
    
    if (!canTrigger || isTriggering) return;

    setIsTriggering(true);
    setMessage(null);

    try {
      await apiClient.triggerJobNow(jobId);
      setMessage({ type: 'success', text: 'Job scheduled to run now!' });
      
      // Clear success message after 3 seconds
      setTimeout(() => setMessage(null), 3000);
      
      // Call onSuccess callback to refresh data
      if (onSuccess) {
        setTimeout(onSuccess, 500); // Small delay to let the backend update
      }
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Failed to trigger job';
      setMessage({ type: 'error', text: errorMessage });
      
      // Clear error message after 5 seconds
      setTimeout(() => setMessage(null), 5000);
    } finally {
      setIsTriggering(false);
    }
  };

  if (!canTrigger && variant === 'icon') {
    return null; // Don't show icon button if can't trigger
  }

  const buttonSizeClasses = size === 'sm' 
    ? 'px-2 py-1 text-xs' 
    : 'px-3 py-2 text-sm';

  const iconSize = size === 'sm' ? 'h-3 w-3' : 'h-4 w-4';

  if (variant === 'icon') {
    return (
      <div className="relative">
        <button
          onClick={handleTrigger}
          disabled={!canTrigger || isTriggering}
          className={`
            inline-flex items-center justify-center rounded-full 
            ${buttonSizeClasses}
            ${canTrigger && !isTriggering
              ? 'bg-green-600 hover:bg-green-700 text-white'
              : 'bg-gray-300 text-gray-500 cursor-not-allowed'
            }
            transition-colors font-medium
          `}
          title={canTrigger ? 'Trigger job now' : `Cannot trigger (status: ${jobStatus})`}
        >
          {isTriggering ? (
            <Loader2 className={`${iconSize} animate-spin`} />
          ) : (
            <Play className={iconSize} />
          )}
        </button>
        
        {message && (
          <div className={`
            absolute top-full left-1/2 transform -translate-x-1/2 mt-2 
            px-2 py-1 text-xs rounded whitespace-nowrap z-10
            ${message.type === 'success' 
              ? 'bg-green-100 text-green-800 border border-green-200' 
              : 'bg-red-100 text-red-800 border border-red-200'
            }
          `}>
            {message.text}
          </div>
        )}
      </div>
    );
  }

  return (
    <div className="space-y-2">
      <button
        onClick={handleTrigger}
        disabled={!canTrigger || isTriggering}
        className={`
          inline-flex items-center gap-2 rounded-md 
          ${buttonSizeClasses}
          ${canTrigger && !isTriggering
            ? 'bg-green-600 hover:bg-green-700 text-white'
            : 'bg-gray-300 text-gray-500 cursor-not-allowed'
          }
          transition-colors font-medium
        `}
      >
        {isTriggering ? (
          <Loader2 className={`${iconSize} animate-spin`} />
        ) : (
          <Play className={iconSize} />
        )}
        {isTriggering ? 'Triggering...' : 'Trigger Now'}
      </button>
      
      {message && (
        <div className={`
          text-xs px-2 py-1 rounded
          ${message.type === 'success' 
            ? 'text-green-700 bg-green-50 border border-green-200' 
            : 'text-red-700 bg-red-50 border border-red-200'
          }
        `}>
          {message.text}
        </div>
      )}
      
      {/* {!canTrigger && (
        <div className="text-xs text-muted-foreground">
          Only waiting jobs can be triggered
        </div>
      )} */}
    </div>
  );
}
