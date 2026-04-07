export interface I18NString {
  [key: string]: string;
}

export interface Info {
  name: I18NString;
  description: I18NString;
  type: string; // sync, cached, etc.
  upstream: string;
  url: string;
}

export interface Volume {
  src: string;
  dst: string;
}

export interface IntervalConfig {
  type: string;
  value: string;
}

export interface SyncConfig {
  jobName: string;
  interval: IntervalConfig;
  timeout: string;
  image: string;
  volumes: Volume[];
  command: string[];
  environments: string[];
  node?: string;
  nodeSelector?: Record<string, string>;
}

export interface Repo {
  id: string;
  info: Info;
  sync: SyncConfig;
}

export interface Job {
  id: string;
  status: 'Waiting' | 'Scheduled' | 'Running' | 'Orphan';
  updated_at: string;
  last_success_at: string;
  last_failure_at: string;
  last_attempt_at: string;
  next_attempt_at: string;
  last_action_status: 'Running' | 'Succeeded' | 'Failed' | '';
  actions: string[];
}

export interface Action {
  id: string;
  updated_at: string;
  job_id: string;
  status: 'Running' | 'Succeeded' | 'Failed' | 'Reconciling';
  message: string;
  worker_name?: string;
  container_id: string;
  container_name: string;
  container_image: string;
  container_status: string;
  container_exit_code: number;
  container_exit_signal: number;
  container_exit_reason: string;
  container_volumes?: Volume[];
  container_env?: string[];
  container_command?: string[];
  container_timeout?: string;
  created_at?: string;
  started_at?: string;
  finished_at?: string;
}

export interface Worker {
  name: string;
  addr: string;
  labels: Record<string, string> | null;
  vars: Record<string, string> | null;
  status: 'Online' | 'Offline';
  last_heartbeat: string;
  running_actions: string[] | null;
  registered_at: string;
}

export type QueueItem = string;

export interface QueueResponse {
  paused: boolean;
  max_concurrency: number;
  queue: QueueItem[];
}

export interface LogEntry {
  name: string;
  is_dir: boolean;
  size: number;
  mod_time: string;
}

export interface LogListResponse {
  action_id: string;
  entries: LogEntry[];
}
