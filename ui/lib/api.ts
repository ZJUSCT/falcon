// Read-only API client for the Kubernetes controller backend.
//
// Same-origin by construction: the go-staging gateway (HTTPRoute) sends
// `Exact /api/jobs` and `PathPrefix /api/repos/` to the controller service,
// and everything else (`PathPrefix /`) to this static UI. The UI container's
// nginx therefore serves files only — there is no proxying anywhere, and
// fetch('/api/...') just works. All old mutation endpoints (queue/worker/
// action/job management) are gone and are not modeled.

import { Job, UsageResponse } from '@/types';

const API_BASE = '/api';

class ApiClient {
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
