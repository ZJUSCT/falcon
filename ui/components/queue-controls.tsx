'use client';

import { useState } from 'react';
import { apiClient } from '@/lib/api';
import { Pause, Play, Loader2 } from 'lucide-react';

interface QueueControlsProps {
  isPaused?: boolean;
  onSuccess?: () => void;
}

export function QueueControls({ isPaused = false, onSuccess }: QueueControlsProps) {
  const [isLoading, setIsLoading] = useState(false);
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null);

  const handlePause = async () => {
    if (isLoading) return;

    setIsLoading(true);
    setMessage(null);

    try {
      await apiClient.pauseQueue();
      setMessage({ type: 'success', text: 'Queue paused successfully' });
      
      // Clear success message after 3 seconds
      setTimeout(() => setMessage(null), 3000);
      
      // Call onSuccess callback to refresh data
      if (onSuccess) {
        setTimeout(onSuccess, 500);
      }
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Failed to pause queue';
      setMessage({ type: 'error', text: errorMessage });
      
      // Clear error message after 5 seconds
      setTimeout(() => setMessage(null), 5000);
    } finally {
      setIsLoading(false);
    }
  };

  const handleResume = async () => {
    if (isLoading) return;

    setIsLoading(true);
    setMessage(null);

    try {
      await apiClient.resumeQueue();
      setMessage({ type: 'success', text: 'Queue resumed successfully' });
      
      // Clear success message after 3 seconds
      setTimeout(() => setMessage(null), 3000);
      
      // Call onSuccess callback to refresh data
      if (onSuccess) {
        setTimeout(onSuccess, 500);
      }
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Failed to resume queue';
      setMessage({ type: 'error', text: errorMessage });
      
      // Clear error message after 5 seconds
      setTimeout(() => setMessage(null), 5000);
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <button
          onClick={handlePause}
          disabled={isPaused || isLoading}
          className={`
            inline-flex items-center gap-2 px-4 py-2 text-sm rounded-md font-medium transition-colors
            ${isPaused || isLoading
              ? 'bg-gray-300 text-gray-500 cursor-not-allowed'
              : 'bg-yellow-600 hover:bg-yellow-700 text-white'
            }
          `}
        >
          {isLoading ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <Pause className="h-4 w-4" />
          )}
          {isPaused ? 'Paused' : 'Pause Queue'}
        </button>

        <button
          onClick={handleResume}
          disabled={!isPaused || isLoading}
          className={`
            inline-flex items-center gap-2 px-4 py-2 text-sm rounded-md font-medium transition-colors
            ${!isPaused || isLoading
              ? 'bg-gray-300 text-gray-500 cursor-not-allowed'
              : 'bg-green-600 hover:bg-green-700 text-white'
            }
          `}
        >
          {isLoading ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <Play className="h-4 w-4" />
          )}
          Resume Queue
        </button>
      </div>

      {message && (
        <div className={`
          text-sm px-3 py-2 rounded border
          ${message.type === 'success' 
            ? 'text-green-700 bg-green-50 border-green-200' 
            : 'text-red-700 bg-red-50 border-red-200'
          }
        `}>
          {message.text}
        </div>
      )}
    </div>
  );
}
