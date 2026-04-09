import { Repo, Job, Action, QueueItem, QueueResponse, LogListResponse, Worker, ZFSWorkerReport, ZFSPoolInfo, ZFSDatasetInfo, ZFSSnapshotInfo } from '@/types';

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
    return this.setNextAttempt(repoId);
  }

  async setNextAttempt(repoId: string, time?: string): Promise<Job> {
    const params = new URLSearchParams({ repo_id: repoId });
    if (time) params.set('time', time);
    const response = await fetch(`${API_BASE}/jobs/set_next_attempt?${params}`, {
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

  async setMaxConcurrency(max: number): Promise<{ max_concurrency: number }> {
    const response = await fetch(`${API_BASE}/queue/set_max_concurrency?max=${max}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobToHead(repoId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean; max_concurrency: number }> {
    const response = await fetch(`${API_BASE}/queue/move_to_head?repo_id=${encodeURIComponent(repoId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobToTail(repoId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean; max_concurrency: number }> {
    const response = await fetch(`${API_BASE}/queue/move_to_tail?repo_id=${encodeURIComponent(repoId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobBefore(targetId: string, refId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean; max_concurrency: number }> {
    const response = await fetch(`${API_BASE}/queue/move_before?target_id=${encodeURIComponent(targetId)}&ref_id=${encodeURIComponent(refId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async moveJobAfter(targetId: string, refId: string): Promise<{ ok: boolean; queue: QueueItem[]; paused: boolean; max_concurrency: number }> {
    const response = await fetch(`${API_BASE}/queue/move_after?target_id=${encodeURIComponent(targetId)}&ref_id=${encodeURIComponent(refId)}`, {
      method: 'POST',
    });
    
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    
    return response.json();
  }

  async deleteJobFromQueue(repoId: string): Promise<{ removed: number; queue: QueueItem[]; paused: boolean; max_concurrency: number }> {
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

  async saveRepo(repo: Repo): Promise<Repo> {
    const response = await fetch(`${API_BASE}/repos`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(repo),
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    return response.json();
  }

  async deleteRepo(id: string): Promise<void> {
    const response = await fetch(`${API_BASE}/repos?id=${encodeURIComponent(id)}`, {
      method: 'DELETE',
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
  }

  async getWorkers(): Promise<Worker[]> {
    return this.fetch<Worker[]>('/workers');
  }

  async pauseJob(repoId: string, paused: boolean = true): Promise<Job> {
    const response = await fetch(`${API_BASE}/jobs/pause?repo_id=${encodeURIComponent(repoId)}&paused=${paused}`, {
      method: 'POST',
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
    return response.json();
  }

  async deleteJob(id: string): Promise<void> {
    const response = await fetch(`${API_BASE}/jobs/delete?id=${encodeURIComponent(id)}`, {
      method: 'POST',
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
  }

  async removeWorker(name: string): Promise<void> {
    const response = await fetch(`${API_BASE}/workers/remove?name=${encodeURIComponent(name)}`, {
      method: 'POST',
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
  }

  getLogStreamUrl(actionId: string, fileName: string, from: 'start' | 'end' = 'end'): string {
    return `${API_BASE}/logs/stream?action_id=${encodeURIComponent(actionId)}&file=${encodeURIComponent(fileName)}&from=${from}`;
  }

  // ZFS Management APIs
  async refreshZFS(): Promise<void> {
    const response = await fetch(`${API_BASE}/zfs/refresh`, { method: 'POST' });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || response.statusText);
    }
  }

  async getZFSReports(): Promise<ZFSWorkerReport[]> {
    return this.fetch<ZFSWorkerReport[]>('/zfs/report');
  }

  async getZFSReport(worker: string): Promise<ZFSWorkerReport> {
    return this.fetch<ZFSWorkerReport>(`/zfs/report?worker=${encodeURIComponent(worker)}`);
  }

  async getZFSPools(worker: string): Promise<ZFSPoolInfo[]> {
    return this.fetch<ZFSPoolInfo[]>(`/zfs/pools?worker=${encodeURIComponent(worker)}`);
  }

  async getZFSDatasets(worker: string): Promise<ZFSDatasetInfo[]> {
    return this.fetch<ZFSDatasetInfo[]>(`/zfs/datasets?worker=${encodeURIComponent(worker)}`);
  }

  async getZFSSnapshots(worker: string, dataset?: string): Promise<ZFSSnapshotInfo[]> {
    let url = `/zfs/snapshots?worker=${encodeURIComponent(worker)}`;
    if (dataset) url += `&dataset=${encodeURIComponent(dataset)}`;
    return this.fetch<ZFSSnapshotInfo[]>(url);
  }

  async createZFSSnapshot(worker: string, dataset: string, snapName: string, recursive: boolean = false): Promise<void> {
    const response = await fetch(`${API_BASE}/zfs/snapshots/create`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ worker, dataset, snap_name: snapName, recursive }),
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || response.statusText);
    }
  }

  async destroyZFSSnapshot(worker: string, snapshot: string): Promise<void> {
    const response = await fetch(`${API_BASE}/zfs/snapshots/destroy`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ worker, snapshot }),
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || response.statusText);
    }
  }

  async createZFSDataset(worker: string, name: string, properties?: Record<string, string>): Promise<void> {
    const response = await fetch(`${API_BASE}/zfs/datasets/create`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ worker, name, properties }),
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || response.statusText);
    }
  }

  async setZFSProperty(worker: string, dataset: string, property: string, value: string): Promise<void> {
    const response = await fetch(`${API_BASE}/zfs/datasets/set_property`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ worker, dataset, property, value }),
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || response.statusText);
    }
  }
}

export const apiClient = new ApiClient();
