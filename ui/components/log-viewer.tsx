'use client';

import { useState, useEffect, useRef } from 'react';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { RelativeTime } from '@/components/relative-time';
import { apiClient } from '@/lib/api';
import { LogEntry, LogListResponse } from '@/types';
import { FileText, Folder, Download, ExternalLink, RefreshCw, ArrowDown } from 'lucide-react';
import { formatBytes } from '@/lib/utils';

interface LogViewerProps {
  actionId: string;
}

// Parse SSE (Server-Sent Events) message format
function parseSSEMessage(message: string): string | null {
  const lines = message.trim().split('\n');
  let data = '';
  let event = '';
  
  for (const line of lines) {
    if (line.startsWith('data: ')) {
      // Extract data content, handle multiple data lines
      const dataContent = line.substring(6); // Remove 'data: ' prefix
      data +=  dataContent+ '\n';
    } else if (line.startsWith('event: ')) {
      event = line.substring(7); // Remove 'event: ' prefix
    } else if (line.startsWith('id: ')) {
      // Ignore id for now
      continue;
    } else if (line.startsWith('retry: ')) {
      // Ignore retry for now
      continue;
    } else if (line.trim() === '') {
      // Empty line - end of message
      break;
    }
  }
  
  // Return the data content if available
  return data || null;
}

export function LogViewer({ actionId }: LogViewerProps) {
  const [logList, setLogList] = useState<LogListResponse | null>(null);
  const [selectedFile, setSelectedFile] = useState<string | null>(null);
  const [logContent, setLogContent] = useState<string>('');
  const [loading, setLoading] = useState(true);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [autoScroll, setAutoScroll] = useState(true); // Track auto-scroll state

  const autoScrollRef = useRef(autoScroll);
  useEffect(() => {
    autoScrollRef.current = autoScroll;
  }, [autoScroll]); 
  
  const logContentRef = useRef<HTMLDivElement>(null);
  const abortControllerRef = useRef<AbortController | null>(null);

  // Check if user is at the bottom of the scroll area
  const isAtBottom = () => {
    if (!logContentRef.current) return false;
    const { scrollTop, scrollHeight, clientHeight } = logContentRef.current;
    // Consider "at bottom" if within 30px of the bottom
    return scrollHeight - scrollTop - clientHeight < 30;
  };

  // Handle scroll events to update auto-scroll state
  const handleScroll = () => {
    const atBottom = isAtBottom();
    if (atBottom !== autoScroll) {
      console.debug(`User scroll detected: atBottom=${atBottom}, setting autoScroll=${atBottom}`);
      setAutoScroll(atBottom);
    }
  };

  // Force scroll to bottom (used by manual scroll button)
  const forceScrollToBottom = () => {
    const el = logContentRef.current;
    if (!el) return;
    el.scrollTo({
      top: el.scrollHeight, // 浏览器会自动 clamp 到最大有效值
      behavior: 'auto',     // 'instant' -> 'auto'（标准值）
    });
  };

  // Manually scroll to bottom and enable auto-scroll
  const scrollToBottom = () => {
    forceScrollToBottom();
    setAutoScroll(true);
  };

  // Fetch log file list
  const fetchLogList = async () => {
    try {
      const data = await apiClient.getLogList(actionId);
      setLogList(data);
      
      // Auto-select first non-directory file
      const firstFile = data.entries.find(entry => !entry.is_dir);
      if (firstFile && !selectedFile) {
        setSelectedFile(firstFile.name);
      }
      
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch log files');
    } finally {
      setLoading(false);
    }
  };

  // Stream log content for selected file
  const streamLogContent = async (fileName: string) => {
    if (!fileName) return;

    // Check if there's already a stream for this file
    if (abortControllerRef.current) {
      console.debug('Aborting previous stream for file switch');
      abortControllerRef.current.abort();
      abortControllerRef.current = null;
    }

    setStreaming(true);
    setError(null);

    try {
      const controller = new AbortController();
      abortControllerRef.current = controller;

      const streamUrl = apiClient.getLogStreamUrl(actionId, fileName, 'end');
      console.debug(`Starting stream for file: ${fileName}`, { streamUrl });
      
      const response = await fetch(streamUrl, {
        signal: controller.signal,
      });

      if (!response.ok) {
        throw new Error(`Failed to stream logs: ${response.statusText}`);
      }

      const reader = response.body?.getReader();
      if (!reader) {
        throw new Error('Failed to create stream reader');
      }

      const decoder = new TextDecoder();
      let buffer = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          console.debug(`Stream ended for file: ${fileName}`);
          break;
        }

        buffer += decoder.decode(value, { stream: true });
        
        // Parse SSE format - split by double newlines to get complete messages
        const messages = buffer.split('\n\n');
        
        // Keep the last incomplete message in buffer
        buffer = messages.pop() || '';
        
        // Process complete SSE messages
        for (const message of messages) {
          if (message.trim()) {
            const logData = parseSSEMessage(message);
            if (logData) {
              // Decide stickiness before appending new content
              const shouldStickToBottom = isAtBottom();

              // Update content
              setLogContent(prevContent => {
                const newContent = prevContent + logData;
                // Keep only last 10000 lines to prevent memory issues
                const lines = newContent.split('\n');
                if (lines.length > 10000) {
                  return lines.slice(-10000).join('\n');
                }
                return newContent;
              });

              // Scroll if user was at bottom OR auto-scroll mode is enabled
              if (shouldStickToBottom || autoScrollRef.current) {
                // 等待下一帧，确保内容已经渲染到 DOM 再滚动
                requestAnimationFrame(() => {
                  forceScrollToBottom();
                });
              }
            } else {
              // Debug: log unparseable SSE messages
              console.debug('Could not parse SSE message:', message);
            }
          }
        }
      }
    } catch (err) {
      if (err instanceof Error) {
        if (err.name === 'AbortError') {
          console.debug(`Stream aborted for file: ${fileName}`);
        } else {
          console.error(`Stream error for file: ${fileName}`, err);
          setError(err.message);
        }
      }
    } finally {
      console.debug(`Stream cleanup for file: ${fileName}`);
      setStreaming(false);
    }
  };

  // Handle file selection
  const handleFileSelect = (fileName: string) => {
    console.debug(`Switching to file: ${fileName}, previous file: ${selectedFile}`);
    
    // Abort current stream before switching
    if (abortControllerRef.current) {
      console.debug('Aborting current stream for file switch');
      abortControllerRef.current.abort();
      abortControllerRef.current = null;
    }
    
    setSelectedFile(fileName);
    setAutoScroll(true); // Reset auto-scroll when selecting a new file
    setLogContent(''); // Clear previous content
    setStreaming(false);
  };

  // Generate raw URL for download
  const getRawUrl = (fileName: string) => {
    return apiClient.getLogRawUrl(actionId, fileName);
  };

  // Refresh log list
  const refreshLogList = () => {
    fetchLogList();
  };

  useEffect(() => {
    fetchLogList();
    
    // Cleanup on unmount
    return () => {
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }
    };
  }, [actionId]);

  useEffect(() => {
    if (selectedFile) {
      // Small delay to ensure state has settled after file selection
      const timeoutId = setTimeout(() => {
        streamLogContent(selectedFile);
      }, 100);
      
      return () => {
        clearTimeout(timeoutId);
      };
    }
  }, [selectedFile, actionId]);

  // Auto-scroll to bottom when first content loads for a new file
  useEffect(() => {
    if (logContent && selectedFile && autoScroll) {
      requestAnimationFrame(() => {
        forceScrollToBottom();
      });
    }
  }, [selectedFile, Boolean(logContent), autoScroll]);
  // Remove the conflicting useEffect that was forcing scroll on every content update

  if (loading) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Log Files</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex items-center justify-center h-32">
            <div className="text-muted-foreground">Loading log files...</div>
          </div>
        </CardContent>
      </Card>
    );
  }

  if (error && !logList) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Log Files</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex items-center justify-center h-32">
            <div className="text-destructive">Error: {error}</div>
          </div>
        </CardContent>
      </Card>
    );
  }

  const logFiles = logList?.entries.filter(entry => !entry.is_dir) || [];
  const logDirs = logList?.entries.filter(entry => entry.is_dir) || [];

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <div>
            <CardTitle className="flex items-center gap-2">
              <FileText className="h-5 w-5" />
              Log Files
            </CardTitle>
            <CardDescription>
              Action logs
            </CardDescription>
          </div>
          <button
            onClick={refreshLogList}
            className="flex items-center gap-2 px-3 py-1 text-sm rounded-md border hover:bg-muted transition-colors"
          >
            <RefreshCw className="h-4 w-4" />
            Refresh
          </button>
        </div>
      </CardHeader>
      <CardContent>
        <div className="grid grid-cols-1 lg:grid-cols-5 gap-4 h-[650px]">
          {/* Left Column - File List */}
          <div className="lg:col-span-1 space-y-2 lg:max-w-xs">
            <div className="text-xs font-medium text-muted-foreground">
              Files ({logFiles.length})
            </div>
            
            {logFiles.length === 0 && logDirs.length === 0 ? (
              <div className="text-center text-muted-foreground py-8">
                No log files found
              </div>
            ) : (
                              <div className="space-y-1 max-h-[600px] overflow-y-auto">
                {/* Directories */}
                {logDirs.map((entry) => (
                  <div
                    key={entry.name}
                    className="flex items-center gap-1 p-1 rounded-md bg-muted/50 text-xs"
                  >
                    <Folder className="h-3 w-3 text-blue-600" />
                    <span className="font-mono truncate text-xs">{entry.name}</span>
                  </div>
                ))}
                
                {/* Files */}
                {logFiles.map((entry) => (
                  <div
                    key={entry.name}
                    className={`
                      flex items-center justify-between p-1 rounded-md border cursor-pointer transition-colors
                      ${selectedFile === entry.name 
                        ? 'bg-primary/10 border-primary text-primary' 
                        : 'hover:bg-muted/50 border-border'
                      }
                    `}
                    onClick={() => handleFileSelect(entry.name)}
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      <FileText className="h-3 w-3 flex-shrink-0" />
                      <span className="font-mono text-xs truncate">{entry.name}</span>
                    </div>
                    <div className="flex items-center gap-1 flex-shrink-0">
                      <span className="text-xs text-muted-foreground">
                        {formatBytes(entry.size)}
                      </span>
                      <a
                        href={getRawUrl(entry.name)}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="p-1 hover:bg-background rounded"
                        onClick={(e) => e.stopPropagation()}
                        title="Download raw file"
                      >
                        <Download className="h-3 w-3" />
                      </a>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* Right Column - Log Content */}
          <div className="lg:col-span-4 space-y-4">
            <div className="flex items-center justify-between">
              <div className="text-sm font-medium text-muted-foreground">
                {selectedFile ? `Content: ${selectedFile}` : 'Select a file to view'}
              </div>
              {selectedFile && (
                <div className="flex items-center gap-2">
                  {streaming && (
                    <div className="flex items-center gap-1 text-xs text-blue-600">
                      <div className="w-2 h-2 bg-blue-600 rounded-full animate-pulse"></div>
                      SSE Streaming
                    </div>
                  )}
                  {streaming && (
                    <div className={`flex items-center gap-1 text-xs ${autoScroll ? 'text-green-600' : 'text-yellow-600'}`}>
                      <div className={`w-2 h-2 rounded-full ${autoScroll ? 'bg-green-600' : 'bg-yellow-600'}`}></div>
                      {autoScroll ? 'Auto-scroll ON' : 'Auto-scroll OFF'}
                    </div>
                  )}
                  {streaming && !autoScroll && (
                    <button
                      onClick={scrollToBottom}
                      className="flex items-center gap-1 px-2 py-1 text-xs rounded border hover:bg-muted transition-colors text-blue-600 hover:text-blue-700"
                      title="Scroll to bottom and enable auto-scroll"
                    >
                      <ArrowDown className="h-3 w-3" />
                      To Bottom
                    </button>
                  )}
                  <a
                    href={getRawUrl(selectedFile)}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="flex items-center gap-1 px-2 py-1 text-xs rounded border hover:bg-muted transition-colors"
                  >
                    <ExternalLink className="h-3 w-3" />
                    Raw
                  </a>
                </div>
              )}
            </div>
            
            <div
              ref={logContentRef}
              className="h-[620px] p-3 bg-black text-green-400 font-mono text-xs overflow-auto rounded-md border"
              onScroll={handleScroll}
            >
              {selectedFile ? (
                logContent ? (
                  <pre className="whitespace-pre-wrap break-words">{logContent}</pre>
                ) : streaming ? (
                  <div className="text-muted-foreground">Connecting to SSE log stream...</div>
                ) : (
                  <div className="text-muted-foreground">No content available</div>
                )
              ) : (
                <div className="flex items-center justify-center h-full text-muted-foreground">
                  Select a log file to view its content
                </div>
              )}
            </div>
            
            {error && (
              <div className="text-destructive text-sm">
                Error: {error}
              </div>
            )}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}
