'use client';

import { useState, useEffect } from 'react';
import { apiClient } from '@/lib/api';
import { Pause, Play, Loader2, Settings } from 'lucide-react';

interface QueueControlsProps {
  isPaused?: boolean;
  maxConcurrency?: number;
  onSuccess?: () => void;
}

export function QueueControls({ isPaused = false, maxConcurrency = 1, onSuccess }: QueueControlsProps) {
  const [isLoading, setIsLoading] = useState(false);
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null);
  const [showConcurrencyModal, setShowConcurrencyModal] = useState(false);
  const [concurrencyValue, setConcurrencyValue] = useState(maxConcurrency.toString());

  // Update concurrencyValue when maxConcurrency prop changes
  useEffect(() => {
    setConcurrencyValue(maxConcurrency.toString());
  }, [maxConcurrency]);

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

  const handleSetConcurrency = async () => {
    if (isLoading) return;

    const value = parseInt(concurrencyValue);
    if (isNaN(value) || value < 1) {
      setMessage({ type: 'error', text: 'Please enter a valid number (minimum 1)' });
      setTimeout(() => setMessage(null), 5000);
      return;
    }

    setIsLoading(true);
    setMessage(null);

    try {
      await apiClient.setMaxConcurrency(value);
      setMessage({ type: 'success', text: `Max concurrency set to ${value}` });
      setShowConcurrencyModal(false);
      
      setTimeout(() => setMessage(null), 3000);
      
      if (onSuccess) {
        setTimeout(onSuccess, 500);
      }
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Failed to set max concurrency';
      setMessage({ type: 'error', text: errorMessage });
      
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

        <button
          onClick={() => setShowConcurrencyModal(true)}
          disabled={isLoading}
          className={`
            inline-flex items-center gap-2 px-4 py-2 text-sm rounded-md font-medium transition-colors
            ${isLoading
              ? 'bg-gray-300 text-gray-500 cursor-not-allowed'
              : 'bg-blue-600 hover:bg-blue-700 text-white'
            }
          `}
        >
          <Settings className="h-4 w-4" />
          Set Concurrency ({maxConcurrency})
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

      {/* Concurrency Setting Modal */}
      {showConcurrencyModal && (
        <div className="fixed inset-0 bg-black bg-opacity-50 flex items-center justify-center z-50">
          <div className="bg-white rounded-lg p-6 w-96">
            <h3 className="text-lg font-semibold mb-4">Set Max Concurrency</h3>
            <p className="text-sm text-gray-600 mb-4">
              Set the maximum number of jobs that can run simultaneously.
            </p>
            <div className="space-y-4">
              <div>
                <label htmlFor="concurrency" className="block text-sm font-medium text-gray-700 mb-1">
                  Max Concurrency
                </label>
                <input
                  type="number"
                  id="concurrency"
                  min="1"
                  value={concurrencyValue}
                  onChange={(e) => setConcurrencyValue(e.target.value)}
                  className="w-full px-3 py-2 border border-gray-300 rounded-md focus:outline-none focus:ring-2 focus:ring-blue-500"
                  placeholder="Enter a number (minimum 1)"
                />
              </div>
              <div className="flex gap-3">
                <button
                  onClick={handleSetConcurrency}
                  disabled={isLoading}
                  className={`
                    flex-1 px-4 py-2 text-sm font-medium rounded-md transition-colors
                    ${isLoading
                      ? 'bg-gray-300 text-gray-500 cursor-not-allowed'
                      : 'bg-blue-600 hover:bg-blue-700 text-white'
                    }
                  `}
                >
                  {isLoading ? (
                    <Loader2 className="h-4 w-4 animate-spin mx-auto" />
                  ) : (
                    'Save'
                  )}
                </button>
                <button
                  onClick={() => setShowConcurrencyModal(false)}
                  disabled={isLoading}
                  className="flex-1 px-4 py-2 text-sm font-medium text-gray-700 bg-gray-100 hover:bg-gray-200 rounded-md transition-colors"
                >
                  Cancel
                </button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
