'use client';

import { useState, useEffect, useCallback } from 'react';
import { apiClient } from '@/lib/api';
import { Repo, Volume } from '@/types';

function emptyRepo(): Repo {
  return {
    id: '',
    info: { name: {}, description: {}, type: '', upstream: '', url: '' },
    sync: {
      jobName: '',
      interval: { type: 'free', value: '' },
      timeout: '',
      image: '',
      volumes: [],
      command: [],
      environments: [],
    },
  };
}

interface RepoFormProps {
  repo: Repo;
  isNew: boolean;
  onSave: (repo: Repo) => Promise<void>;
  onCancel: () => void;
  saving: boolean;
}

function RepoForm({ repo, isNew, onSave, onCancel, saving }: RepoFormProps) {
  const [form, setForm] = useState<Repo>(JSON.parse(JSON.stringify(repo)));
  const [error, setError] = useState<string | null>(null);

  const updateInfo = (key: string, value: string) => {
    setForm(f => ({ ...f, info: { ...f.info, [key]: value } }));
  };

  const updateInfoI18n = (field: 'name' | 'description', lang: string, value: string) => {
    setForm(f => ({
      ...f,
      info: { ...f.info, [field]: { ...f.info[field], [lang]: value } },
    }));
  };

  const updateSync = (key: string, value: any) => {
    setForm(f => ({ ...f, sync: { ...f.sync, [key]: value } }));
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      await onSave(form);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save');
    }
  };

  const addVolume = () => {
    updateSync('volumes', [...(form.sync.volumes || []), { src: '', dst: '' }]);
  };

  const removeVolume = (idx: number) => {
    updateSync('volumes', (form.sync.volumes || []).filter((_: Volume, i: number) => i !== idx));
  };

  const updateVolume = (idx: number, field: 'src' | 'dst', value: string) => {
    const vols = [...(form.sync.volumes || [])];
    vols[idx] = { ...vols[idx], [field]: value };
    updateSync('volumes', vols);
  };

  const addEnv = () => {
    updateSync('environments', [...(form.sync.environments || []), '']);
  };

  const removeEnv = (idx: number) => {
    updateSync('environments', (form.sync.environments || []).filter((_: string, i: number) => i !== idx));
  };

  const updateEnv = (idx: number, value: string) => {
    const envs = [...(form.sync.environments || [])];
    envs[idx] = value;
    updateSync('environments', envs);
  };

  const [nodeSelectorText, setNodeSelectorText] = useState(
    form.sync.nodeSelector
      ? Object.entries(form.sync.nodeSelector).map(([k, v]) => `${k}=${v}`).join('\n')
      : ''
  );

  const commitNodeSelector = (value: string) => {
    if (value.trim() === '') {
      updateSync('nodeSelector', undefined);
      return;
    }
    const pairs: Record<string, string> = {};
    value.split('\n').forEach(line => {
      const eqIdx = line.indexOf('=');
      if (eqIdx > 0) {
        pairs[line.slice(0, eqIdx).trim()] = line.slice(eqIdx + 1).trim();
      }
    });
    updateSync('nodeSelector', Object.keys(pairs).length > 0 ? pairs : undefined);
  };

  const inputClass = "w-full bg-muted/40 border border-border rounded px-2 py-1.5 text-sm font-mono focus:outline-none focus:ring-1 focus:ring-ring";
  const labelClass = "text-muted-foreground text-xs uppercase tracking-wide mb-1 block";

  return (
    <form onSubmit={handleSubmit} className="rounded-lg border border-border bg-card p-4 space-y-4">
      <h3 className="font-bold text-base">{isNew ? 'New Repository' : `Edit: ${repo.id}`}</h3>

      {error && <div className="text-destructive text-sm bg-destructive/10 rounded p-2">{error}</div>}

      {/* ID */}
      <div>
        <label className={labelClass}>Repository ID *</label>
        <input
          className={inputClass}
          value={form.id}
          onChange={e => setForm(f => ({ ...f, id: e.target.value }))}
          disabled={!isNew}
          required
          placeholder="e.g. ubuntu"
        />
      </div>

      {/* Info section */}
      <fieldset className="space-y-3 border border-border/50 rounded p-3">
        <legend className="text-xs font-bold uppercase tracking-wide px-1">Info</legend>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <div>
            <label className={labelClass}>Name (en)</label>
            <input className={inputClass} value={form.info.name?.en || ''} onChange={e => updateInfoI18n('name', 'en', e.target.value)} />
          </div>
          <div>
            <label className={labelClass}>Name (zh)</label>
            <input className={inputClass} value={form.info.name?.zh || ''} onChange={e => updateInfoI18n('name', 'zh', e.target.value)} />
          </div>
          <div>
            <label className={labelClass}>Description (en)</label>
            <input className={inputClass} value={form.info.description?.en || ''} onChange={e => updateInfoI18n('description', 'en', e.target.value)} />
          </div>
          <div>
            <label className={labelClass}>Description (zh)</label>
            <input className={inputClass} value={form.info.description?.zh || ''} onChange={e => updateInfoI18n('description', 'zh', e.target.value)} />
          </div>
          <div>
            <label className={labelClass}>Type</label>
            <input className={inputClass} value={form.info.type} onChange={e => updateInfo('type', e.target.value)} placeholder="e.g. sync" />
          </div>
          <div>
            <label className={labelClass}>Upstream</label>
            <input className={inputClass} value={form.info.upstream} onChange={e => updateInfo('upstream', e.target.value)} />
          </div>
          <div className="sm:col-span-2">
            <label className={labelClass}>URL</label>
            <input className={inputClass} value={form.info.url} onChange={e => updateInfo('url', e.target.value)} />
          </div>
        </div>
      </fieldset>

      {/* Sync section */}
      <fieldset className="space-y-3 border border-border/50 rounded p-3">
        <legend className="text-xs font-bold uppercase tracking-wide px-1">Sync</legend>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <div className="sm:col-span-2">
            <label className={labelClass}>Image</label>
            <input className={inputClass} value={form.sync.image} onChange={e => updateSync('image', e.target.value)} placeholder="e.g. docker.io/library/ubuntu:latest" />
          </div>
          <div className="sm:col-span-2">
            <label className={labelClass}>Command (one arg per line)</label>
            <textarea
              className={inputClass + " min-h-[60px]"}
              value={(form.sync.command || []).join('\n')}
              onChange={e => updateSync('command', e.target.value.split('\n'))}
              rows={3}
            />
          </div>
          <div>
            <label className={labelClass}>Interval Type</label>
            <input className={inputClass} value={form.sync.interval.type} onChange={e => updateSync('interval', { ...form.sync.interval, type: e.target.value })} placeholder="free" />
          </div>
          <div>
            <label className={labelClass}>Interval Value</label>
            <input className={inputClass} value={form.sync.interval.value} onChange={e => updateSync('interval', { ...form.sync.interval, value: e.target.value })} placeholder="e.g. 6h" />
          </div>
          <div>
            <label className={labelClass}>Timeout</label>
            <input className={inputClass} value={form.sync.timeout} onChange={e => updateSync('timeout', e.target.value)} placeholder="e.g. 24h" />
          </div>
          <div>
            <label className={labelClass}>Node (optional)</label>
            <input className={inputClass} value={form.sync.node || ''} onChange={e => updateSync('node', e.target.value || undefined)} />
          </div>
          <div className="sm:col-span-2">
            <label className={labelClass}>Node Selector (key=value, one per line)</label>
            <textarea
              className={inputClass + " min-h-[40px]"}
              value={nodeSelectorText}
              onChange={e => setNodeSelectorText(e.target.value)}
              onBlur={e => commitNodeSelector(e.target.value)}
              rows={2}
              placeholder="e.g. kubernetes.io/hostname=node1"
            />
          </div>
        </div>

        {/* Volumes */}
        <div>
          <div className="flex items-center justify-between mb-1">
            <label className={labelClass + " mb-0"}>Volumes</label>
            <button type="button" onClick={addVolume} className="text-xs text-primary hover:underline">+ Add</button>
          </div>
          {(form.sync.volumes || []).map((vol: Volume, i: number) => (
            <div key={i} className="flex gap-2 items-center mb-1">
              <input className={inputClass} value={vol.src} onChange={e => updateVolume(i, 'src', e.target.value)} placeholder="src" />
              <span className="text-muted-foreground text-xs">-&gt;</span>
              <input className={inputClass} value={vol.dst} onChange={e => updateVolume(i, 'dst', e.target.value)} placeholder="dst" />
              <button type="button" onClick={() => removeVolume(i)} className="text-destructive text-xs hover:underline shrink-0">Remove</button>
            </div>
          ))}
        </div>

        {/* Environments */}
        <div>
          <div className="flex items-center justify-between mb-1">
            <label className={labelClass + " mb-0"}>Environments</label>
            <button type="button" onClick={addEnv} className="text-xs text-primary hover:underline">+ Add</button>
          </div>
          {(form.sync.environments || []).map((env: string, i: number) => (
            <div key={i} className="flex gap-2 items-center mb-1">
              <input className={inputClass} value={env} onChange={e => updateEnv(i, e.target.value)} placeholder="KEY=VALUE" />
              <button type="button" onClick={() => removeEnv(i)} className="text-destructive text-xs hover:underline shrink-0">Remove</button>
            </div>
          ))}
        </div>
      </fieldset>

      <div className="flex gap-2 pt-2">
        <button
          type="submit"
          disabled={saving}
          className="px-4 py-1.5 bg-primary text-primary-foreground rounded text-sm font-medium hover:bg-primary/90 disabled:opacity-50"
        >
          {saving ? 'Saving...' : 'Save'}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="px-4 py-1.5 bg-muted text-muted-foreground rounded text-sm font-medium hover:bg-muted/80"
        >
          Cancel
        </button>
      </div>
    </form>
  );
}

export function ConfigsView() {
  const [repos, setRepos] = useState<Repo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [editingRepo, setEditingRepo] = useState<Repo | null>(null);
  const [isCreating, setIsCreating] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);

  const fetchRepos = useCallback(async () => {
    try {
      setLoading(true);
      const data = await apiClient.getRepos();
      setRepos(data);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch configs');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchRepos();
  }, [fetchRepos]);

  const handleSave = async (repo: Repo) => {
    setSaving(true);
    try {
      await apiClient.saveRepo(repo);
      setEditingRepo(null);
      setIsCreating(false);
      await fetchRepos();
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (id: string) => {
    try {
      await apiClient.deleteRepo(id);
      setDeleteConfirm(null);
      await fetchRepos();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete');
    }
  };

  const handleEdit = (repo: Repo) => {
    setIsCreating(false);
    setEditingRepo(repo);
  };

  const handleCreate = () => {
    setEditingRepo(null);
    setIsCreating(true);
  };

  const handleCancel = () => {
    setEditingRepo(null);
    setIsCreating(false);
  };

  if (loading && repos.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading configs...</div>
      </div>
    );
  }

  if (error && repos.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-destructive">Error: {error}</div>
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-bold">Configs</h2>
          <p className="text-xs text-muted-foreground">Repository configuration management</p>
        </div>
        <button
          onClick={handleCreate}
          disabled={isCreating}
          className="px-4 py-1.5 bg-primary text-primary-foreground rounded text-sm font-medium hover:bg-primary/90 disabled:opacity-50"
        >
          Add New
        </button>
      </div>

      {error && (
        <div className="text-destructive text-sm bg-destructive/10 rounded p-2">{error}</div>
      )}

      {isCreating && (
        <RepoForm
          repo={emptyRepo()}
          isNew={true}
          onSave={handleSave}
          onCancel={handleCancel}
          saving={saving}
        />
      )}

      {editingRepo && !isCreating && (
        <RepoForm
          repo={editingRepo}
          isNew={false}
          onSave={handleSave}
          onCancel={handleCancel}
          saving={saving}
        />
      )}

      {repos.length === 0 ? (
        <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
          No repository configs found.
        </div>
      ) : (
        <div className="space-y-4">
          {repos.map(repo => {
            const nodeSelectorEntries = repo.sync.nodeSelector ? Object.entries(repo.sync.nodeSelector) : [];

            return (
              <div
                key={repo.id}
                className="rounded-lg border border-border bg-card p-4 space-y-3"
              >
                <div className="flex items-center justify-between">
                  <h3 className="font-mono font-bold text-base">{repo.id}</h3>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground font-mono">{repo.info.type}</span>
                    <button
                      onClick={() => handleEdit(repo)}
                      className="text-xs text-primary hover:underline"
                    >
                      Edit
                    </button>
                    {deleteConfirm === repo.id ? (
                      <span className="flex items-center gap-1">
                        <button
                          onClick={() => handleDelete(repo.id)}
                          className="text-xs text-destructive font-bold hover:underline"
                        >
                          Confirm
                        </button>
                        <button
                          onClick={() => setDeleteConfirm(null)}
                          className="text-xs text-muted-foreground hover:underline"
                        >
                          Cancel
                        </button>
                      </span>
                    ) : (
                      <button
                        onClick={() => setDeleteConfirm(repo.id)}
                        className="text-xs text-destructive hover:underline"
                      >
                        Delete
                      </button>
                    )}
                  </div>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 text-sm">
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Image</div>
                    <div className="font-mono text-xs break-all">{repo.sync.image}</div>
                  </div>
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Interval</div>
                    <div className="font-mono text-xs">{repo.sync.interval.value}</div>
                  </div>
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Timeout</div>
                    <div className="font-mono text-xs">{repo.sync.timeout}</div>
                  </div>
                  {repo.sync.node && (
                    <div>
                      <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Node</div>
                      <div className="font-mono text-xs">{repo.sync.node}</div>
                    </div>
                  )}
                  {nodeSelectorEntries.length > 0 && (
                    <div>
                      <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Node Selector</div>
                      <div className="space-y-0.5">
                        {nodeSelectorEntries.map(([k, v]) => (
                          <div key={k} className="font-mono text-xs">
                            <span className="text-muted-foreground">{k}:</span> {v}
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </div>

                {repo.sync.command && repo.sync.command.length > 0 && (
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Command</div>
                    <div className="font-mono text-xs bg-muted/40 rounded p-2 break-all">
                      {repo.sync.command.join(' ')}
                    </div>
                  </div>
                )}

                {repo.sync.volumes && repo.sync.volumes.length > 0 && (
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Volumes</div>
                    <div className="space-y-1">
                      {repo.sync.volumes.map((vol, i) => (
                        <div key={i} className="font-mono text-xs bg-muted/40 rounded px-2 py-1">
                          {vol.src} &rarr; {vol.dst}
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                {repo.sync.environments && repo.sync.environments.length > 0 && (
                  <div>
                    <div className="text-muted-foreground text-xs uppercase tracking-wide mb-1">Environment</div>
                    <div className="space-y-1 max-h-32 overflow-y-auto">
                      {repo.sync.environments.map((env, i) => (
                        <div key={i} className="font-mono text-xs bg-muted/40 rounded px-2 py-1 break-all">
                          {env}
                        </div>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
