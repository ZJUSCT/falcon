import { Repo, Job, Action, QueueItem, QueueResponse, LogListResponse } from '@/types';

const API_BASE = '/api';

class ApiClient {
  private async fetch<T>(endpoint: string): Promise<T> {
    const response = await fetch(`${API_BASE}${endpoint}`);
    if (!response.ok) {
      throw new Error(`API request failed: ${response.statusText}`);
    }
    return response.json();
  }

  async getRepos(): Promise<Repo[]> {
    return this.fetch<Repo[]>('/repos');
  }

  async getJobs(): Promise<Job[]> {
    return this.fetch<Job[]>('/jobs');
  }

  async getActions(): Promise<Action[]> {
    return this.fetch<Action[]>('/actions');
  }

  async getRecentActions(limit: number = 20): Promise<Action[]> {
    return this.fetch<Action[]>(`/actions/recent?limit=${limit}`);
  }

  async getQueue(): Promise<QueueResponse> {
    return this.fetch<QueueResponse>('/queue');
  }

  async getActionsByRepo(repoId: string, limit: number = 10): Promise<Action[]> {
    return this.fetch<Action[]>(`/actions/by_repo?repo_id=${encodeURIComponent(repoId)}&limit=${limit}`);
  }

  async getActionsByIds(ids: string[]): Promise<Action[]> {
    if (ids.length === 0) return [];
    const idsParam = ids.join(',');
    return this.fetch<Action[]>(`/actions/lookup?ids=${encodeURIComponent(idsParam)}`);
  }

  async triggerJobNow(repoId: string): Promise<Job> {
    const response = await fetch(`${API_BASE}/jobs/next_attempt_now?repo_id=${encodeURIComponent(repoId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  // Queue Management APIs
  async pauseQueue(): Promise<{ paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/pause`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async resumeQueue(): Promise<{ paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/continue`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobToHead(repoId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/move_to_head?repo_id=${encodeURIComponent(repoId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobToTail(repoId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/move_to_tail?repo_id=${encodeURIComponent(repoId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobBefore(targetId: string, refId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/move_before?target_id=${encodeURIComponent(targetId)}&ref_id=${encodeURIComponent(refId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobAfter(targetId: string, refId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/move_after?target_id=${encodeURIComponent(targetId)}&ref_id=${encodeURIComponent(refId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async deleteJobFromQueue(repoId: string): Promise<{ removed: number; queue: QueueItem[]; paused: boolean }> {
    const response = await fetch(`${API_BASE}/queue/delete?repo_id=${encodeURIComponent(repoId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  // Log Management APIs
  async getLogList(actionId: string): Promise<LogListResponse> {
    return this.fetch<LogListResponse>(`/logs/list?action_id=${encodeURIComponent(actionId)}`);
  }

  getLogRawUrl(actionId: string, fileName: string): string {
    return `${API_BASE}/logs/raw?action_id=${encodeURIComponent(actionId)}&file=${encodeURIComponent(fileName)}`;
  }

  getLogStreamUrl(actionId: string, fileName: string, from: 'start' | 'end' = 'end'): string {
    return `${API_BASE}/logs/stream?action_id=${encodeURIComponent(actionId)}&file=${encodeURIComponent(fileName)}&from=${from}`;
  }
}

export const apiClient = new ApiClient();
