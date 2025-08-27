'use client';

import { useState } from 'react';
import { apiClient } from '@/lib/api';
import { 
  ChevronUp, 
  ChevronDown, 
  ChevronsUp, 
  ChevronsDown, 
  X, 
  Loader2,
  MoreHorizontal 
} from 'lucide-react';

interface QueueJobControlsProps {
  jobId: string;
  index: number;
  totalJobs: number;
  queue: string[];
  onSuccess?: () => void;
}

export function QueueJobControls({ 
  jobId, 
  index, 
  totalJobs, 
  queue, 
  onSuccess 
}: QueueJobControlsProps) {
  const [isLoading, setIsLoading] = useState(false);
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null);
  const [showMore, setShowMore] = useState(false);

  const handleOperation = async (operation: () => Promise<any>, successMessage: string) => {
    if (isLoading) return;

    setIsLoading(true);
    setMessage(null);

    try {
      await operation();
      setMessage({ type: 'success', text: successMessage });
      
      // Clear success message after 3 seconds
      setTimeout(() => setMessage(null), 3000);
      
      // Call onSuccess callback to refresh data
      if (onSuccess) {
        setTimeout(onSuccess, 500);
      }
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Operation failed';
      setMessage({ type: 'error', text: errorMessage });
      
      // Clear error message after 5 seconds
      setTimeout(() => setMessage(null), 5000);
    } finally {
      setIsLoading(false);
    }
  };

  const moveToHead = () => handleOperation(
    () => apiClient.moveJobToHead(jobId),
    'Moved to front of queue'
  );

  const moveToTail = () => handleOperation(
    () => apiClient.moveJobToTail(jobId),
    'Moved to end of queue'
  );

  const moveUp = () => {
    if (index === 0) return;
    const refId = queue[index - 1];
    handleOperation(
      () => apiClient.moveJobBefore(jobId, refId),
      'Moved up in queue'
    );
  };

  const moveDown = () => {
    if (index === totalJobs - 1) return;
    const refId = queue[index + 1];
    handleOperation(
      () => apiClient.moveJobAfter(jobId, refId),
      'Moved down in queue'
    );
  };

  const deleteFromQueue = () => handleOperation(
    () => apiClient.deleteJobFromQueue(jobId),
    'Removed from queue'
  );

  const isFirst = index === 0;
  const isLast = index === totalJobs - 1;

  return (
    <div className="relative">
      <div className="flex items-center gap-1">
        {/* Quick controls - always visible */}
        <button
          onClick={moveUp}
          disabled={isFirst || isLoading}
          className={`
            p-1 rounded transition-colors
            ${isFirst || isLoading
              ? 'text-gray-300 cursor-not-allowed'
              : 'text-gray-600 hover:text-blue-600 hover:bg-blue-50'
            }
          `}
          title="Move up one position"
        >
          <ChevronUp className="h-4 w-4" />
        </button>

        <button
          onClick={moveDown}
          disabled={isLast || isLoading}
          className={`
            p-1 rounded transition-colors
            ${isLast || isLoading
              ? 'text-gray-300 cursor-not-allowed'
              : 'text-gray-600 hover:text-blue-600 hover:bg-blue-50'
            }
          `}
          title="Move down one position"
        >
          <ChevronDown className="h-4 w-4" />
        </button>

        {/* More options toggle */}
        <button
          onClick={() => setShowMore(!showMore)}
          className="p-1 rounded transition-colors text-gray-600 hover:text-gray-800 hover:bg-gray-50"
          title="More options"
        >
          <MoreHorizontal className="h-4 w-4" />
        </button>

        {/* Loading indicator */}
        {isLoading && (
          <Loader2 className="h-4 w-4 animate-spin text-blue-600" />
        )}
      </div>

      {/* Expanded controls */}
      {showMore && (
        <div className="absolute top-full right-0 mt-1 bg-white border rounded-md shadow-lg z-10 p-2 space-y-1 min-w-[140px]">
          <button
            onClick={moveToHead}
            disabled={isFirst || isLoading}
            className={`
              w-full flex items-center gap-2 px-2 py-1 text-xs rounded transition-colors
              ${isFirst || isLoading
                ? 'text-gray-400 cursor-not-allowed'
                : 'text-gray-700 hover:bg-green-50 hover:text-green-700'
              }
            `}
          >
            <ChevronsUp className="h-3 w-3" />
            Move to front
          </button>

          <button
            onClick={moveToTail}
            disabled={isLast || isLoading}
            className={`
              w-full flex items-center gap-2 px-2 py-1 text-xs rounded transition-colors
              ${isLast || isLoading
                ? 'text-gray-400 cursor-not-allowed'
                : 'text-gray-700 hover:bg-yellow-50 hover:text-yellow-700'
              }
            `}
          >
            <ChevronsDown className="h-3 w-3" />
            Move to end
          </button>

          <div className="border-t my-1"></div>

          <button
            onClick={deleteFromQueue}
            disabled={isLoading}
            className={`
              w-full flex items-center gap-2 px-2 py-1 text-xs rounded transition-colors
              ${isLoading
                ? 'text-gray-400 cursor-not-allowed'
                : 'text-red-600 hover:bg-red-50'
              }
            `}
          >
            <X className="h-3 w-3" />
            Remove from queue
          </button>
        </div>
      )}

      {/* Feedback message */}
      {message && (
        <div className={`
          absolute top-full right-0 mt-1 px-2 py-1 text-xs rounded whitespace-nowrap z-20
          ${message.type === 'success' 
            ? 'bg-green-100 text-green-800 border border-green-200' 
            : 'bg-red-100 text-red-800 border border-red-200'
          }
        `}>
          {message.text}
        </div>
      )}

      {/* Click outside to close more options */}
      {showMore && (
        <div 
          className="fixed inset-0 z-5"
          onClick={() => setShowMore(false)}
        />
      )}
    </div>
  );
}
