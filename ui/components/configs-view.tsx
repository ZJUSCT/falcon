'use client';

import { useState, useEffect, useCallback, useRef } from 'react';
import { apiClient } from '@/lib/api';
import { Repo, Volume, Worker } from '@/types';
import { Search, X, Plus, Trash2, Eye, Copy, Code, FileText } from 'lucide-react';

/* ------------------------------------------------------------------ */
/*  Helpers                                                            */
/* ------------------------------------------------------------------ */

function emptyRepo(): Repo {
  return {
    id: '',
    info: { name: {}, description: {}, type: 'sync', upstream: '', url: '' },
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

function expandVars(s: string, vars: Record<string, string>): string {
  for (const [k, v] of Object.entries(vars)) {
    s = s.split('$' + k).join(v);
  }
  return s;
}

/** Check if a string still contains unresolved $VAR references */
function hasUnresolved(s: string): boolean {
  return /\$[A-Z_][A-Z0-9_]*/i.test(s);
}

/** Mirror backend MatchWorker logic: check node name + nodeSelector labels */
function matchWorker(worker: Worker, repo: Repo): boolean {
  if (repo.sync.node && worker.name !== repo.sync.node) return false;
  const sel = repo.sync.nodeSelector;
  if (sel) {
    for (const [k, v] of Object.entries(sel)) {
      if (!worker.labels || worker.labels[k] !== v) return false;
    }
  }
  return true;
}

/* ------------------------------------------------------------------ */
/*  Drawer                                                             */
/* ------------------------------------------------------------------ */

interface DrawerProps {
  open: boolean;
  onClose: () => void;
  children: React.ReactNode;
}

function Drawer({ open, onClose, children }: DrawerProps) {
  useEffect(() => {
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    if (open) document.addEventListener('keydown', handleKey);
    return () => document.removeEventListener('keydown', handleKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <div className="absolute inset-0 bg-black/40 transition-opacity" onClick={onClose} />
      <div className="relative w-full max-w-xl bg-background border-l border-border shadow-2xl flex flex-col animate-in slide-in-from-right duration-200">
        {children}
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  RepoForm (inside drawer)                                           */
/* ------------------------------------------------------------------ */

interface VolumeConflict {
  src: string;
  otherRepoId: string;
}

/** Find $REPODIR volume src paths in this repo that collide with other repos (excluding self).
 *  Only checks volumes containing $REPODIR — shared scripts ($BASEDIR) are expected to overlap. */
function findVolumeConflicts(form: Repo, allRepos: Repo[]): VolumeConflict[] {
  const mySrcs = (form.sync.volumes || []).map(v => v.src).filter(s => s.includes('$REPODIR'));
  if (mySrcs.length === 0) return [];

  const conflicts: VolumeConflict[] = [];
  for (const other of allRepos) {
    if (other.id === form.id) continue;
    const otherSrcs = new Set((other.sync.volumes || []).filter(v => v.src.includes('$REPODIR')).map(v => v.src));
    for (const src of mySrcs) {
      if (otherSrcs.has(src)) {
        conflicts.push({ src, otherRepoId: other.id });
      }
    }
  }
  return conflicts;
}

interface RepoFormProps {
  repo: Repo;
  isNew: boolean;
  allRepos: Repo[];
  onSave: (repo: Repo) => Promise<void>;
  onDelete?: () => void;
  onDuplicate?: () => void;
  onClose: () => void;
  saving: boolean;
  deleteConfirm: boolean;
  onDeleteConfirmChange: (v: boolean) => void;
  workers: Worker[];
}

function RepoForm({ repo, isNew, allRepos, onSave, onDelete, onDuplicate, onClose, saving, deleteConfirm, onDeleteConfirmChange, workers }: RepoFormProps) {
  const [form, setForm] = useState<Repo>(JSON.parse(JSON.stringify(repo)));
  const [error, setError] = useState<string | null>(null);
  const [previewWorker, setPreviewWorker] = useState<string>('');
  const [jsonMode, setJsonMode] = useState(false);
  const [jsonText, setJsonText] = useState('');
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [conflictConfirmed, setConflictConfirmed] = useState(false);

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

  const conflicts = findVolumeConflicts(form, allRepos);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    if (jsonMode) {
      if (!applyJson()) return;
    }
    // If conflicts exist and not yet confirmed, block save
    if (conflicts.length > 0 && !conflictConfirmed) {
      setConflictConfirmed(true); // show warning, require second click
      return;
    }
    try {
      await onSave(form);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save');
    }
  };

  // Reset conflict confirmation when volumes change
  const prevVolumesRef = useRef(JSON.stringify(form.sync.volumes));
  useEffect(() => {
    const cur = JSON.stringify(form.sync.volumes);
    if (cur !== prevVolumesRef.current) {
      prevVolumesRef.current = cur;
      setConflictConfirmed(false);
    }
  }, [form.sync.volumes]);

  /* JSON mode helpers */
  const enterJsonMode = () => {
    setJsonText(JSON.stringify(form, null, 2));
    setJsonError(null);
    setJsonMode(true);
  };

  const applyJson = (): boolean => {
    try {
      const parsed = JSON.parse(jsonText) as Repo;
      if (!parsed.id && !isNew) parsed.id = repo.id;
      setForm(parsed);
      setJsonError(null);
      return true;
    } catch (err) {
      setJsonError(err instanceof Error ? err.message : 'Invalid JSON');
      return false;
    }
  };

  const exitJsonMode = () => {
    if (applyJson()) {
      setJsonMode(false);
    }
  };

  /* volumes helpers */
  const addVolume = () => updateSync('volumes', [...(form.sync.volumes || []), { src: '', dst: '' }]);
  const removeVolume = (idx: number) => updateSync('volumes', (form.sync.volumes || []).filter((_: Volume, i: number) => i !== idx));
  const updateVolume = (idx: number, field: 'src' | 'dst', value: string) => {
    const vols = [...(form.sync.volumes || [])];
    vols[idx] = { ...vols[idx], [field]: value };
    updateSync('volumes', vols);
  };

  /* environment helpers */
  const addEnv = () => updateSync('environments', [...(form.sync.environments || []), '']);
  const removeEnv = (idx: number) => updateSync('environments', (form.sync.environments || []).filter((_: string, i: number) => i !== idx));
  const updateEnv = (idx: number, value: string) => {
    const envs = [...(form.sync.environments || [])];
    envs[idx] = value;
    updateSync('environments', envs);
  };

  /* node selector */
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

  /* node affinity: which workers match */
  const matchedWorkers = workers.filter(w => matchWorker(w, form));
  const hasAffinity = !!form.sync.node || (form.sync.nodeSelector && Object.keys(form.sync.nodeSelector).length > 0);

  const inputClass = "w-full bg-muted/40 border border-border rounded px-2.5 py-1.5 text-sm font-mono focus:outline-none focus:ring-1 focus:ring-ring";
  const labelClass = "text-muted-foreground text-xs uppercase tracking-wide mb-1 block";

  return (
    <form onSubmit={handleSubmit} className="flex flex-col h-full">
      {/* header */}
      <div className="flex items-center justify-between px-5 py-4 border-b border-border shrink-0">
        <h3 className="font-bold text-base truncate">
          {isNew ? 'New Repository' : repo.id}
        </h3>
        <div className="flex items-center gap-1">
          {/* JSON toggle */}
          <button
            type="button"
            onClick={jsonMode ? exitJsonMode : enterJsonMode}
            className={`p-1.5 rounded text-xs flex items-center gap-1 ${jsonMode ? 'bg-primary/20 text-primary' : 'hover:bg-muted text-muted-foreground'}`}
            title={jsonMode ? 'Switch to form' : 'Edit as JSON'}
          >
            {jsonMode ? <FileText className="h-3.5 w-3.5" /> : <Code className="h-3.5 w-3.5" />}
            <span className="hidden sm:inline">{jsonMode ? 'Form' : 'JSON'}</span>
          </button>
          {/* Duplicate */}
          {!isNew && onDuplicate && (
            <button
              type="button"
              onClick={onDuplicate}
              className="p-1.5 rounded hover:bg-muted text-muted-foreground flex items-center gap-1 text-xs"
              title="Duplicate"
            >
              <Copy className="h-3.5 w-3.5" />
              <span className="hidden sm:inline">Duplicate</span>
            </button>
          )}
          <button type="button" onClick={onClose} className="p-1 rounded hover:bg-muted text-muted-foreground ml-1">
            <X className="h-4 w-4" />
          </button>
        </div>
      </div>

      {/* scrollable body */}
      <div className="flex-1 overflow-y-auto px-5 py-4 space-y-5">
        {error && <div className="text-destructive text-sm bg-destructive/10 rounded p-2">{error}</div>}

        {jsonMode ? (
          /* ---- JSON editor ---- */
          <div className="space-y-2">
            {jsonError && <div className="text-destructive text-sm bg-destructive/10 rounded p-2">{jsonError}</div>}
            <textarea
              className="w-full bg-muted/40 border border-border rounded px-3 py-2 text-xs font-mono focus:outline-none focus:ring-1 focus:ring-ring resize-y"
              value={jsonText}
              onChange={e => { setJsonText(e.target.value); setJsonError(null); }}
              rows={30}
              spellCheck={false}
            />
          </div>
        ) : (
          /* ---- Form editor ---- */
          <>
            {/* ID */}
            {isNew && (
              <div>
                <label className={labelClass}>Repository ID *</label>
                <input
                  className={inputClass}
                  value={form.id}
                  onChange={e => setForm(f => ({ ...f, id: e.target.value }))}
                  required
                  placeholder="e.g. ubuntu"
                  autoFocus
                />
              </div>
            )}

            {/* Info section */}
            <fieldset className="space-y-3 border border-border/50 rounded-lg p-3">
              <legend className="text-xs font-bold uppercase tracking-wide px-1.5">Info</legend>
              <div className="grid grid-cols-2 gap-3">
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
                  <input className={inputClass} value={form.info.type} onChange={e => updateInfo('type', e.target.value)} placeholder="sync" />
                </div>
                <div>
                  <label className={labelClass}>Upstream</label>
                  <input className={inputClass} value={form.info.upstream} onChange={e => updateInfo('upstream', e.target.value)} />
                </div>
                <div className="col-span-2">
                  <label className={labelClass}>URL</label>
                  <input className={inputClass} value={form.info.url} onChange={e => updateInfo('url', e.target.value)} placeholder="/ubuntu" />
                </div>
              </div>
            </fieldset>

            {/* Sync section */}
            <fieldset className="space-y-3 border border-border/50 rounded-lg p-3">
              <legend className="text-xs font-bold uppercase tracking-wide px-1.5">Sync</legend>
              <div className="grid grid-cols-2 gap-3">
                <div className="col-span-2">
                  <label className={labelClass}>Image</label>
                  <input className={inputClass} value={form.sync.image} onChange={e => updateSync('image', e.target.value)} placeholder="debian-ftpsync:latest" />
                </div>
                <div className="col-span-2">
                  <label className={labelClass}>Command (one arg per line)</label>
                  <textarea
                    className={inputClass + " min-h-[60px] resize-y"}
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
                  <input className={inputClass} value={form.sync.interval.value} onChange={e => updateSync('interval', { ...form.sync.interval, value: e.target.value })} placeholder="6h" />
                </div>
                <div>
                  <label className={labelClass}>Timeout</label>
                  <input className={inputClass} value={form.sync.timeout} onChange={e => updateSync('timeout', e.target.value)} placeholder="24h" />
                </div>
                <div>
                  <label className={labelClass}>Node (optional)</label>
                  <input className={inputClass} value={form.sync.node || ''} onChange={e => updateSync('node', e.target.value || undefined)} />
                </div>
                <div className="col-span-2">
                  <label className={labelClass}>Node Selector (key=value, one per line)</label>
                  <textarea
                    className={inputClass + " min-h-[40px] resize-y"}
                    value={nodeSelectorText}
                    onChange={e => setNodeSelectorText(e.target.value)}
                    onBlur={e => commitNodeSelector(e.target.value)}
                    rows={2}
                    placeholder="zone=a"
                  />
                </div>
              </div>

              {/* Node affinity match preview */}
              {hasAffinity && (
                <div className="rounded bg-muted/40 px-2.5 py-2 text-xs">
                  <span className="text-muted-foreground">Matches: </span>
                  {matchedWorkers.length === 0 ? (
                    <span className="text-destructive font-medium">no workers</span>
                  ) : (
                    matchedWorkers.map((w, i) => (
                      <span key={w.name}>
                        {i > 0 && <span className="text-muted-foreground">, </span>}
                        <span className={`font-mono ${w.status === 'Online' ? 'text-green-400' : 'text-muted-foreground'}`}>
                          {w.name}
                        </span>
                      </span>
                    ))
                  )}
                </div>
              )}

              {/* Volumes */}
              <div>
                <div className="flex items-center justify-between mb-1.5">
                  <label className={labelClass + " mb-0"}>Volumes</label>
                  <button type="button" onClick={addVolume} className="text-xs text-primary hover:underline flex items-center gap-0.5">
                    <Plus className="h-3 w-3" /> Add
                  </button>
                </div>
                <div className="space-y-1.5">
                  {(form.sync.volumes || []).map((vol: Volume, i: number) => (
                    <div key={i} className="flex gap-1.5 items-center">
                      <input className={inputClass + " flex-1"} value={vol.src} onChange={e => updateVolume(i, 'src', e.target.value)} placeholder="src" />
                      <span className="text-muted-foreground text-xs shrink-0">&rarr;</span>
                      <input className={inputClass + " flex-1"} value={vol.dst} onChange={e => updateVolume(i, 'dst', e.target.value)} placeholder="dst" />
                      <button type="button" onClick={() => removeVolume(i)} className="text-destructive/60 hover:text-destructive p-1 shrink-0">
                        <X className="h-3.5 w-3.5" />
                      </button>
                    </div>
                  ))}
                </div>
              </div>

              {/* Environments */}
              <div>
                <div className="flex items-center justify-between mb-1.5">
                  <label className={labelClass + " mb-0"}>Environments</label>
                  <button type="button" onClick={addEnv} className="text-xs text-primary hover:underline flex items-center gap-0.5">
                    <Plus className="h-3 w-3" /> Add
                  </button>
                </div>
                <div className="space-y-1.5">
                  {(form.sync.environments || []).map((env: string, i: number) => (
                    <div key={i} className="flex gap-1.5 items-center">
                      <input className={inputClass + " flex-1"} value={env} onChange={e => updateEnv(i, e.target.value)} placeholder="KEY=VALUE" />
                      <button type="button" onClick={() => removeEnv(i)} className="text-destructive/60 hover:text-destructive p-1 shrink-0">
                        <X className="h-3.5 w-3.5" />
                      </button>
                    </div>
                  ))}
                </div>
              </div>
            </fieldset>

            {/* Runtime Preview */}
            {workers.length > 0 && (
              <fieldset className="space-y-3 border border-border/50 rounded-lg p-3">
                <legend className="text-xs font-bold uppercase tracking-wide px-1.5 flex items-center gap-1">
                  <Eye className="h-3 w-3" /> Runtime Preview
                </legend>
                <div>
                  <label className={labelClass}>Worker</label>
                  <select
                    className="w-full bg-muted/40 border border-border rounded px-2.5 py-1.5 text-sm font-mono focus:outline-none focus:ring-1 focus:ring-ring"
                    value={previewWorker}
                    onChange={e => setPreviewWorker(e.target.value)}
                  >
                    <option value="">Select a worker...</option>
                    {workers.map(w => (
                      <option key={w.name} value={w.name}>
                        {w.name}{w.status === 'Offline' ? ' (offline)' : ''}{!matchWorker(w, form) ? ' (no match)' : ''}
                      </option>
                    ))}
                  </select>
                </div>
                {(() => {
                  const w = workers.find(w => w.name === previewWorker);
                  if (!w || !w.vars) return null;
                  const vars = w.vars;
                  const hasVars = Object.keys(vars).length > 0;
                  if (!hasVars) return (
                    <div className="text-xs text-muted-foreground">This worker has no variables configured.</div>
                  );

                  const volumes = form.sync.volumes || [];
                  const envs = form.sync.environments || [];

                  return (
                    <div className="space-y-3">
                      <div>
                        <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Worker Variables</div>
                        <div className="space-y-0.5">
                          {Object.entries(vars).map(([k, v]) => (
                            <div key={k} className="font-mono text-xs">
                              <span className="text-blue-400">${k}</span>
                              <span className="text-muted-foreground"> = </span>
                              <span>{v}</span>
                            </div>
                          ))}
                        </div>
                      </div>

                      {volumes.length > 0 && (
                        <div>
                          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Resolved Volumes</div>
                          <div className="space-y-1">
                            {volumes.map((vol, i) => {
                              const resolvedSrc = expandVars(vol.src, vars);
                              const changed = resolvedSrc !== vol.src;
                              const unresolved = hasUnresolved(resolvedSrc);
                              return (
                                <div key={i} className="font-mono text-xs bg-muted/40 rounded px-2 py-1">
                                  <span className={unresolved ? 'text-destructive' : changed ? 'text-green-400' : ''}>
                                    {resolvedSrc}
                                  </span>
                                  {unresolved && <span className="text-destructive ml-1 text-[10px]">(unresolved)</span>}
                                  <span className="text-muted-foreground"> &rarr; </span>
                                  <span>{vol.dst}</span>
                                </div>
                              );
                            })}
                          </div>
                        </div>
                      )}

                      {envs.length > 0 && envs.some(e => e.includes('$')) && (
                        <div>
                          <div className="text-[10px] uppercase tracking-widest text-muted-foreground mb-1">Resolved Environments</div>
                          <div className="space-y-1">
                            {envs.filter(e => e.includes('$')).map((env, i) => {
                              const resolved = expandVars(env, vars);
                              const unresolved = hasUnresolved(resolved);
                              return (
                                <div key={i} className="font-mono text-xs bg-muted/40 rounded px-2 py-1">
                                  <span className={unresolved ? 'text-destructive' : 'text-green-400'}>{resolved}</span>
                                  {unresolved && <span className="text-destructive ml-1 text-[10px]">(unresolved)</span>}
                                </div>
                              );
                            })}
                          </div>
                        </div>
                      )}
                    </div>
                  );
                })()}
              </fieldset>
            )}
          </>
        )}
      </div>

      {/* conflict warning */}
      {conflicts.length > 0 && conflictConfirmed && (
        <div className="px-5 py-2 border-t border-destructive/30 bg-destructive/10 shrink-0">
          <div className="text-destructive text-xs font-medium mb-1">Volume path conflicts detected:</div>
          <div className="space-y-0.5">
            {conflicts.map((c, i) => (
              <div key={i} className="text-xs font-mono">
                <span className="text-destructive">{c.src}</span>
                <span className="text-muted-foreground"> also used by </span>
                <span className="text-destructive font-semibold">{c.otherRepoId}</span>
              </div>
            ))}
          </div>
          <div className="text-destructive text-xs mt-1">Click Save again to confirm.</div>
        </div>
      )}

      {/* footer */}
      <div className="flex items-center justify-between px-5 py-3 border-t border-border shrink-0 bg-background">
        <div>
          {!isNew && onDelete && (
            deleteConfirm ? (
              <span className="flex items-center gap-2 text-xs">
                <span className="text-destructive">Delete this repo?</span>
                <button type="button" onClick={onDelete} className="text-destructive font-bold hover:underline">Yes</button>
                <button type="button" onClick={() => onDeleteConfirmChange(false)} className="text-muted-foreground hover:underline">No</button>
              </span>
            ) : (
              <button
                type="button"
                onClick={() => onDeleteConfirmChange(true)}
                className="flex items-center gap-1 text-xs text-muted-foreground hover:text-destructive transition-colors"
              >
                <Trash2 className="h-3.5 w-3.5" /> Delete
              </button>
            )
          )}
        </div>
        <div className="flex gap-2">
          <button
            type="button"
            onClick={onClose}
            className="px-4 py-1.5 bg-muted text-muted-foreground rounded text-sm font-medium hover:bg-muted/80"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={saving}
            className={`px-4 py-1.5 rounded text-sm font-medium disabled:opacity-50 ${
              conflicts.length > 0 && conflictConfirmed
                ? 'bg-destructive text-destructive-foreground hover:bg-destructive/90'
                : 'bg-primary text-primary-foreground hover:bg-primary/90'
            }`}
          >
            {saving ? 'Saving...' : conflicts.length > 0 && conflictConfirmed ? 'Save Anyway' : conflicts.length > 0 ? 'Save' : 'Save'}
          </button>
        </div>
      </div>
    </form>
  );
}

/* ------------------------------------------------------------------ */
/*  ConfigsView (main)                                                 */
/* ------------------------------------------------------------------ */

export function ConfigsView() {
  const [repos, setRepos] = useState<Repo[]>([]);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  /* drawer state */
  const [drawerRepo, setDrawerRepo] = useState<Repo | null>(null);
  const [isCreating, setIsCreating] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deleteConfirm, setDeleteConfirm] = useState(false);

  const drawerOpen = drawerRepo !== null || isCreating;

  const fetchRepos = useCallback(async () => {
    try {
      const [repoData, workerData] = await Promise.all([
        apiClient.getRepos(),
        apiClient.getWorkers(),
      ]);
      setRepos(repoData);
      setWorkers(workerData);
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

  const closeDrawer = () => {
    setDrawerRepo(null);
    setIsCreating(false);
    setDeleteConfirm(false);
  };

  const handleSave = async (repo: Repo) => {
    setSaving(true);
    try {
      await apiClient.saveRepo(repo);
      closeDrawer();
      await fetchRepos();
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!drawerRepo) return;
    try {
      await apiClient.deleteRepo(drawerRepo.id);
      closeDrawer();
      await fetchRepos();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete');
    }
  };

  const openEdit = (repo: Repo) => {
    setIsCreating(false);
    setDeleteConfirm(false);
    setDrawerRepo(repo);
  };

  const openCreate = (from?: Repo) => {
    setDrawerRepo(null);
    setDeleteConfirm(false);
    if (from) {
      // duplicate: deep copy, clear id
      const dup = JSON.parse(JSON.stringify(from)) as Repo;
      dup.id = '';
      setDrawerRepo(dup);
      setIsCreating(true);
    } else {
      setIsCreating(true);
    }
  };

  const handleDuplicate = () => {
    if (!drawerRepo) return;
    const dup = JSON.parse(JSON.stringify(drawerRepo)) as Repo;
    dup.id = '';
    setDrawerRepo(dup);
    setIsCreating(true);
    setDeleteConfirm(false);
  };

  /* filter */
  const filtered = repos.filter(r =>
    !search || r.id.toLowerCase().includes(search.toLowerCase())
      || (r.sync.image || '').toLowerCase().includes(search.toLowerCase())
      || (r.info.upstream || '').toLowerCase().includes(search.toLowerCase())
  );

  if (loading && repos.length === 0) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-muted-foreground">Loading configs...</div>
      </div>
    );
  }

  return (
    <div className="p-6 space-y-4">
      {/* header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-bold">Configs</h2>
          <p className="text-xs text-muted-foreground">Repository configuration management</p>
        </div>
        <span className="text-xs text-muted-foreground font-mono">{repos.length} total</span>
      </div>

      {error && (
        <div className="text-destructive text-sm bg-destructive/10 rounded p-2">{error}</div>
      )}

      {/* toolbar */}
      <div className="flex flex-col sm:flex-row gap-3">
        <div className="relative flex-1">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <input
            type="text"
            placeholder="Filter by name, image, upstream..."
            value={search}
            onChange={e => setSearch(e.target.value)}
            className="w-full pl-9 pr-3 py-1.5 text-xs rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/60"
          />
        </div>
        <button
          onClick={() => openCreate()}
          className="flex items-center gap-1.5 px-4 py-1.5 bg-primary text-primary-foreground rounded-md text-sm font-medium hover:bg-primary/90 shrink-0"
        >
          <Plus className="h-3.5 w-3.5" /> Add New
        </button>
      </div>

      {/* table */}
      {filtered.length === 0 ? (
        <div className="rounded-lg border border-border bg-card p-6 text-sm text-muted-foreground">
          {repos.length === 0 ? 'No repository configs found.' : 'No configs match the current filter.'}
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead className="bg-muted/40 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                <tr>
                  <th className="px-3 py-2 text-left">ID</th>
                  <th className="px-3 py-2 text-left">Image</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Interval</th>
                  <th className="hidden md:table-cell px-3 py-2 text-left">Timeout</th>
                  <th className="hidden lg:table-cell px-3 py-2 text-left">Node / Workers</th>
                  <th className="hidden lg:table-cell px-3 py-2 text-left">Upstream</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {filtered.map(repo => {
                  const matched = workers.filter(w => matchWorker(w, repo));
                  const hasAff = !!repo.sync.node || (repo.sync.nodeSelector && Object.keys(repo.sync.nodeSelector).length > 0);

                  return (
                    <tr
                      key={repo.id}
                      onClick={() => openEdit(repo)}
                      className="group cursor-pointer bg-background transition-colors hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60"
                      tabIndex={0}
                      onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openEdit(repo); } }}
                    >
                      <td className="px-3 py-2 align-top">
                        <div className="font-mono font-semibold text-sm">{repo.id}</div>
                        <div className="text-[11px] text-muted-foreground mt-0.5 md:hidden">
                          {repo.sync.interval.value} / {repo.sync.timeout}
                        </div>
                      </td>
                      <td className="px-3 py-2 align-top">
                        <div className="font-mono text-xs text-muted-foreground max-w-[200px] truncate" title={repo.sync.image}>
                          {repo.sync.image || '--'}
                        </div>
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        <span className="font-mono">{repo.sync.interval.value || '--'}</span>
                      </td>
                      <td className="hidden md:table-cell px-3 py-2 align-top">
                        <span className="font-mono">{repo.sync.timeout || '--'}</span>
                      </td>
                      <td className="hidden lg:table-cell px-3 py-2 align-top">
                        {hasAff ? (
                          <div className="flex flex-wrap gap-1">
                            {matched.length === 0 ? (
                              <span className="text-destructive text-[11px]">no match</span>
                            ) : (
                              matched.map(w => (
                                <span
                                  key={w.name}
                                  className={`inline-block font-mono text-[11px] px-1.5 py-0.5 rounded ${
                                    w.status === 'Online'
                                      ? 'bg-green-500/15 text-green-400'
                                      : 'bg-muted text-muted-foreground'
                                  }`}
                                >
                                  {w.name}
                                </span>
                              ))
                            )}
                          </div>
                        ) : (
                          <span className="font-mono text-muted-foreground text-[11px]">any</span>
                        )}
                      </td>
                      <td className="hidden lg:table-cell px-3 py-2 align-top">
                        <div className="font-mono text-muted-foreground max-w-[220px] truncate" title={repo.info.upstream}>
                          {repo.info.upstream || '--'}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* drawer */}
      <Drawer open={drawerOpen} onClose={closeDrawer}>
        {isCreating && (
          <RepoForm
            repo={drawerRepo || emptyRepo()}
            isNew={true}
            allRepos={repos}
            onSave={handleSave}
            onClose={closeDrawer}
            saving={saving}
            deleteConfirm={false}
            onDeleteConfirmChange={() => {}}
            workers={workers}
          />
        )}
        {drawerRepo && !isCreating && (
          <RepoForm
            repo={drawerRepo}
            isNew={false}
            allRepos={repos}
            onSave={handleSave}
            onDelete={handleDelete}
            onDuplicate={handleDuplicate}
            onClose={closeDrawer}
            saving={saving}
            deleteConfirm={deleteConfirm}
            onDeleteConfirmChange={setDeleteConfirm}
            workers={workers}
          />
        )}
      </Drawer>
    </div>
  );
}
