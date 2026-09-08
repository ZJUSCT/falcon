// Same-origin client for public reads and authenticated mirror controls.

import { Job, StorageResponse, UsageResponse } from '@/types';

const API_BASE = '/api';

class ApiClient {
  async canAdminister(): Promise<boolean> {
    const response = await fetch("/oauth/session");
    return response.ok;
  }

  async mirrorAction(name: string, action: "pause" | "resume" | "sync" | "abort"): Promise<Job> {
    const response = await fetch(`${API_BASE}/mirrors/${encodeURIComponent(name)}/actions`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action }),
    });
    if (!response.ok) throw new Error(await errorMessage(response));
    return response.json();
  }

  private async fetchJson<T>(endpoint: string): Promise<T> {
    const response = await fetch(`${API_BASE}${endpoint}`);
    if (!response.ok) {
      throw new Error(await errorMessage(response));
    }
    return response.json();
  }

  // GET /api/jobs — legacy-compatible list of Mirror/ProxyMirror jobs.
  async getJobs(): Promise<Job[]> {
    return this.fetchJson<Job[]>('/jobs');
  }

  // GET /api/usage — cluster-wide storage usage aggregation. Replies 404
  // when the usage feature is disabled; callers treat any failure as "no
  // usage data" and degrade silently (column `—` / card hint).
  async getUsage(): Promise<UsageResponse> {
    return this.fetchJson<UsageResponse>('/usage');
  }

  // GET /api/storage — per-node ZFS inventory (pools/datasets/snapshots).
  // Available only when the usage aggregation is enabled; replies 404
  // otherwise, and callers treat any failure as "no storage data" and
  // degrade silently (hint card on the Storage page).
  async getStorage(): Promise<StorageResponse> {
    return this.fetchJson<StorageResponse>('/storage');
  }

  // GET /api/repos/<name> — spec-only view of one Mirror/ProxyMirror.
  // Default serialization is YAML; pass ext: 'json' for JSON.
  async getRepoSpec(name: string, ext: '' | 'json' = ''): Promise<string> {
    const suffix = ext ? `.${ext}` : '';
    const response = await fetch(`${API_BASE}/repos/${encodeURIComponent(name)}${suffix}`);
    if (!response.ok) {
      throw new Error(await errorMessage(response));
    }
    return response.text();
  }
}

async function errorMessage(response: Response): Promise<string> {
  try {
    const body = await response.json();
    if (body && typeof body.error === 'string') {
      return body.error;
    }
  } catch {
    // not a JSON error body
  }
  return `API request failed: ${response.statusText}`;
}

export const apiClient = new ApiClient();
