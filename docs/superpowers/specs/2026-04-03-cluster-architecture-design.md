# MirrorGo Cluster Architecture Design

## Overview

Refactor MirrorGo from a single-node sync controller into a master-worker cluster architecture with label-based node affinity scheduling.

- **Master**: pure scheduler + Web UI + worker management. No Docker execution.
- **Worker**: registers to Master, executes sync containers via Docker, reports status, serves logs.

Single binary `mirrorgo` with subcommands `master` and `worker`.

## Project Structure

```
mirrorgo
├── main.go                  # Entry: parse subcommand master / worker
├── shared/
│   ├── types.go             # Repo, Job, Action, Volume, Worker shared types
│   └── protocol.go          # Master<->Worker request/response structs
├── master/
│   ├── master.go            # Master startup entry
│   ├── scheduler.go         # scheduleLoop + dispatchLoop (affinity-aware)
│   ├── worker_manager.go    # Worker registration, heartbeat, status management
│   ├── db.go                # SQLite persistence (Job/Action/Queue/Worker state)
│   ├── queue.go             # Dispatch queue
│   ├── web.go               # Web UI + API + log proxy
│   ├── mirrorz.go           # MirrorZ output
│   └── ws_hub.go            # WebSocket hub: receive Worker status pushes
├── worker/
│   ├── worker.go            # Worker startup: register, heartbeat, task execution loop
│   ├── docker.go            # Docker container management
│   ├── log.go               # Log directory management
│   ├── api.go               # Worker HTTP API: log query/stream + action status
│   └── ws_client.go         # WebSocket client: push Action status to Master
└── ui/                      # Frontend (unchanged)
```

## Startup Commands

```bash
mirrorgo master \
  --addr :8080 \
  --db state.db \
  --auth-token "shared-secret" \
  --configs ./Configs

mirrorgo worker \
  --name worker-1 \
  --master http://master:8080 \
  --auth-token "shared-secret" \
  --addr :9090 \
  --labels storage=ssd,zone=a \
  --basedir /home/zjusct/mirrorgo \
  --repodir /test1/mirrors/
```

## Authentication

Two API surface areas with different auth models:

- **Internal APIs** (`/api/internal/*`): Master-Worker communication. Pre-shared key (PSK) carried as `Authorization: Bearer <token>` header. WebSocket connections pass token as query parameter. Both Master and Worker validate the token on all internal endpoints.
- **Public APIs** (`/api/*`, `/mirrorz.json`, UI): No built-in authentication. Access control is handled externally via reverse proxy, firewall, or network policy (matching current single-node behavior).

## Communication Protocol

HTTP REST for regular interactions. WebSocket for real-time Action status push. Log streaming uses HTTP SSE (Server-Sent Events), same as the current implementation.

### Worker Registration (Worker -> Master, HTTP POST)

```
POST /api/internal/register
Authorization: Bearer <token>
```
```json
{
  "name": "worker-1",
  "labels": {"storage": "ssd", "zone": "a"},
  "addr": "http://10.0.0.2:9090"
}
```

Registration rules:
- Name already exists and Online: return 409 Conflict, reject. Worker must wait for old entry to go Offline.
- Name already exists and Offline: treat as re-registration, update Addr/Labels, transition Offline -> Online, trigger reconciliation.
- Name does not exist: create new record, set Online.

### Heartbeat (Worker -> Master, HTTP POST, every 10s)

```
POST /api/internal/heartbeat
Authorization: Bearer <token>
```
```json
{
  "name": "worker-1",
  "running_actions": ["action-id-1", "action-id-2"],
  "load": {"cpu": 0.3, "mem": 0.6}
}
```

Master updates Worker LastHeartbeat. If no heartbeat for 30s, mark Offline.

Master diffs `running_actions` against its own records for that Worker:
- Master has a Running/Reconciling action not in the heartbeat list: action completed but WebSocket message was lost. Master calls `GET worker:9090/api/internal/action_status?id=xxx` to fetch final status and updates accordingly.

### Task Dispatch (Master -> Worker, HTTP POST)

```
POST http://worker-addr:9090/api/internal/dispatch
Authorization: Bearer <token>
```
```json
{
  "action": {
    "id": "17119...",
    "job_id": "debian",
    "container_image": "debian-ftpsync:latest",
    "container_command": ["/bin/dash", "/scripts/ftpsync.sh", "sync:archive:debian"],
    "container_volumes": [...],
    "container_env": [...],
    "container_timeout": "12h"
  }
}
```

### Status Push (Worker -> Master, WebSocket)

```
ws://master:8080/api/internal/ws?name=worker-1&token=<token>
```

Messages:
```json
{
  "type": "action_status",
  "action_id": "17119...",
  "status": "Succeeded",
  "container_status": "Exited",
  "exit_code": 0,
  "exit_reason": "",
  "updated_at": "2026-04-03T12:00:00Z"
}
```

### Action Status Query (Master -> Worker, HTTP GET)

Used when heartbeat diff detects a missing action (WebSocket completion message lost).

```
GET http://worker-addr:9090/api/internal/action_status?id=xxx
Authorization: Bearer <token>
```

Worker returns final status from memory cache. Worker keeps the last 1000 completed action results in an LRU cache (action ID -> final status, exit code, exit reason, timestamps). This cache is lost on Worker restart, which is acceptable because Master-side reconciliation handles that case separately.

### Log Proxy (Master -> Worker, HTTP proxy)

User requests Master `/api/logs/*`, Master looks up which Worker the action ran on, proxies to that Worker's API:

- `GET /api/logs/list?action_id=xxx` -> proxy to Worker
- `GET /api/logs/raw?action_id=xxx&file=name` -> proxy to Worker
- `GET /api/logs/stream?action_id=xxx&file=name` -> proxy to Worker

Worker exposes the same log endpoints as the current single-node implementation.

## Repo Configuration: Node Affinity

```json
{
  "id": "debian",
  "sync": {
    "node": "worker-1",
    "nodeSelector": {"storage": "ssd"},
    "interval": {"type": "free", "value": "1h"},
    "timeout": "12h",
    "image": "debian-ftpsync:latest",
    "volumes": [...],
    "command": [...],
    "environments": [...]
  }
}
```

- `node`: exact Worker name. Highest priority. Only schedules to that Worker.
- `nodeSelector`: label matching. Worker labels must contain ALL specified key-value pairs.
- Both set: `node` takes priority.
- Neither set: schedules to any online Worker.

## Worker State Machine

### States

- **Unregistered**: no record in Master DB
- **Online**: registered, heartbeat normal
- **Offline**: heartbeat timeout (30s)

### Transitions

```
Unregistered -> Online
  Trigger: Worker POST /register (name not in DB)
  Action: Create Worker record with Addr/Labels, set LastHeartbeat

Online -> Online (self-loop)
  Trigger: Heartbeat received / WebSocket message received
  Action: Update LastHeartbeat, RunningActions, diff check for completed actions

Online -> Offline
  Trigger: >30s since last heartbeat
  Action: Mark Offline. All Running Actions on this Worker -> Reconciling.
          No new tasks dispatched to this Worker.
          Corresponding Jobs stay Running (suspended, waiting).

Offline -> Online
  Trigger: Worker re-registers (POST /register, name exists and Offline)
  Action: Update Addr/Labels, mark Online, update LastHeartbeat.
          Reconciliation begins on the FIRST heartbeat after re-registration
          (heartbeat contains running_actions):
            - Reported action -> restore to Running
            - Unreported Reconciling action -> mark Failed, finishJob

Offline -> Unregistered
  Trigger: Admin POST /api/workers/remove
  Action: Delete Worker record. Residual Reconciling actions -> mark Failed, finishJob.

Online -> Unregistered
  Trigger: Admin POST /api/workers/remove
  Action: Reject with error. Worker must go Offline first.
```

```
                    register
  Unregistered ──────────────> Online
       ^                        | ^ heartbeat (self-loop)
       |                        v |
       | admin remove         Offline
       |                        |
       +<----- admin remove ----+
       |                        |
       |         re-register    |
       |         Online <-------+
```

## Action State Machine

### States

- **Running**: container executing on a Worker
- **Reconciling**: Master uncertain of actual status, waiting for Worker confirmation
- **Succeeded**: container exited with code 0 (terminal)
- **Failed**: container exited with code != 0, or abnormal (terminal)

### Transitions

```
Running -> Succeeded
  Trigger: Worker reports exit_code == 0 via WebSocket
  Action: Update Action, finishJob(succeeded=true)

Running -> Failed
  Trigger: Worker reports exit_code != 0 via WebSocket
  Action: Update Action, finishJob(succeeded=false)

Note: dispatch POST failure does NOT create an Action at all (see Scheduler section).
Only Actions that exist can transition.

Running -> Reconciling
  Trigger: Master restarts (all Running actions become Reconciling)
           OR Worker transitions Online -> Offline
  Action: Job stays Running, no new Action scheduled for that Job

Reconciling -> Running
  Trigger: Worker comes back online, heartbeat reports this action in running_actions
  Action: Restore normal monitoring

Reconciling -> Succeeded
  Trigger: Worker comes back, reports action completed with exit_code == 0
  Action: Update Action, finishJob(succeeded=true)

Reconciling -> Failed
  Trigger: Worker comes back and reports exit_code != 0
           OR Worker comes back but doesn't report this action (container lost)
           OR Admin removes Worker (residual actions all Failed)
  Action: Update Action, finishJob(succeeded=false)
```

```
                    exit_code==0
  Running ─────────────────────────> Succeeded
    |           exit_code!=0
    | ─────────────────────────────> Failed
    |
    |  master restart / worker offline
    v
  Reconciling
    |  worker reports running    -> Running
    |  worker reports succeeded  -> Succeeded
    |  worker reports failed     -> Failed
    |  worker reports nothing    -> Failed
    |  admin removes worker      -> Failed
```

## Job State Machine

Unchanged from current design:

- **Waiting**: waiting for NextAttemptAt
- **Scheduled**: NextAttemptAt passed, in dispatch queue
- **Running**: Action executing on a Worker
- **Orphan**: repo config deleted

Transitions:
- Waiting -> Scheduled: NextAttemptAt passed, enqueue
- Scheduled -> Running: dispatched to Worker, Action created
- Running -> Waiting: Action finished, NextAttemptAt = Now + Interval
- Any -> Orphan: repo config deleted

## Scheduler (dispatchLoop) Changes

Current: dequeue from head, start local Docker container.

New:

```
1. Dequeue job from queue head
2. Read repo affinity config (node / nodeSelector)
3. Filter matching online Workers
4. No available Worker:
     Put job back to queue tail, continue to next
     If consecutive re-queues == queue length: stop this tick, wait for next
5. Multiple Workers available:
     Pick the one with fewest running actions (simple load balancing)
6. POST /dispatch to selected Worker
7. Worker returns success:
     Create Action record (Running, with worker_name)
     Update Job to Running
     Re-check if Worker is still Online:
       If Offline: immediately move Action to Reconciling
8. Worker returns failure:
     Put job back to queue tail, do not create Action
```

### Action Data Model Change

```go
type Action struct {
    // ...existing fields
    WorkerName string `json:"worker_name"` // which Worker executed this action
}
```

## Restart Recovery

### Master Restart

1. Load Job/Action/Queue/Worker records from SQLite
2. All `status==Running` Actions -> mark `Reconciling`
3. All `status==Scheduled` Jobs -> revert to `Waiting` with `NextAttemptAt = Now` (prevents loss of jobs dequeued but not yet dispatched)
4. All Workers -> mark `Offline` (until they re-register)
5. **Immediately** resume scheduleLoop and dispatchLoop (non-blocking). Dispatch will naturally skip jobs whose Workers are Offline.
6. Workers reconnect at their own pace, re-register, and send first heartbeat
7. On first heartbeat from a reconnected Worker: reconcile actions based on `running_actions`
   - Reported as running -> restore Action to Running
   - Reported as completed -> update Action to Succeeded/Failed, finishJob
   - Not reported -> mark Failed, finishJob

### Worker Restart

1. Worker is stateless (no persistent DB). However, Worker maintains an in-memory LRU cache of completed action results (see Action Status Query).
2. On startup: POST /register to Master, establish WebSocket
3. Scan local Docker containers with `syncing-` name prefix:
   - Still running -> resume monitoring, report via WebSocket
   - Already exited -> collect exit code from Docker inspect (container is NOT deleted until status is confirmed reported to Master), populate completed action cache, report Succeeded/Failed via WebSocket
   - Not found -> no action (Master-side reconciliation handles it)
4. Normal heartbeat resumes, Master completes reconciliation

**Container deletion policy change**: In the current single-node code, `CheckContainer` immediately deletes exited containers. In cluster mode, Worker defers container deletion until Master has acknowledged the final status (via WebSocket ack or heartbeat diff confirmation). This ensures exited containers can be re-inspected after Worker restart. Worker periodically cleans up acknowledged containers older than 1 hour as a safety net.

### WebSocket Reconnection

```
Worker side:
  Connection lost -> wait 1s -> reconnect -> re-send registration
  Consecutive failures -> exponential backoff (1s, 2s, 4s, ... max 30s)
  Reconnect success -> re-report all running action current status
```

Worker containers continue running during disconnection. Status changes cached in memory, batch-pushed on reconnect.

### Dispatch Atomicity

Dispatch ordering:
1. Master generates action ID
2. POST /dispatch to Worker (includes the action ID)
3. Worker returns success -> Master creates Action record in DB
4. Worker returns failure (HTTP error / timeout) -> no Action created, job back to queue tail

The `/dispatch` endpoint is **idempotent by action ID**: if the Worker receives a duplicate action ID it already knows about, it returns success without creating a second container. This handles the case where Master's POST succeeded but the response was lost (timeout), so Master retries safely.

**Dispatch timeout**: Master uses a 30s HTTP timeout for `/dispatch`. On timeout, Master does NOT create an Action record. The next heartbeat from the Worker will reveal whether the container actually started:
- If the action ID appears in `running_actions` -> Master creates the Action record retroactively as Running
- If it does not appear -> the dispatch was lost, no action needed

**Master crash between steps 2 and 3**: Worker runs a container that Master has no record of. On Worker reconnection, heartbeat reports the unknown action ID. Master queries `GET /api/internal/action_status?id=xxx` on the Worker to get full details, then creates the Action record with the correct status.

## Log Management

- Logs stored on Worker local disk at `<logdir>/<action_id>/`
- Sync scripts write logs to `/mirrorlogs/` (bind-mounted to host)
- Docker stdout/stderr copied to `container.log` after exit (existing logic)
- Master proxies log requests to the appropriate Worker based on `action.worker_name`
- No log upload to Master; logs remain on Worker

## Volume Path Resolution

`$BASEDIR` and `$REPODIR` substitution moves to Worker side. Each Worker defines its own paths via startup parameters:

```bash
mirrorgo worker ... --basedir /home/zjusct/mirrorgo --repodir /test1/mirrors/
```

Different Workers can have different paths.

## Worker Data Model (Master DB)

```go
type WorkerModel struct {
    Name           string    `gorm:"primaryKey;column:name"`
    Addr           string    `gorm:"column:addr"`
    Labels         string    `gorm:"column:labels;type:TEXT"`   // JSON serialized map[string]string
    Status         string    `gorm:"column:status"`             // Online / Offline
    LastHeartbeat  time.Time `gorm:"column:last_heartbeat"`
    RunningActions string    `gorm:"column:running_actions;type:TEXT"` // JSON serialized []string
    RegisteredAt   time.Time `gorm:"column:registered_at"`
}
```

Table name: `workers`. Persisted in Master's SQLite so Worker list survives Master restart.

## Worker Management API

```
GET  /api/workers              — list all Workers and their status
POST /api/workers/remove?name= — remove an Offline Worker record
```

## Deployment

### Minimum Configuration

One Master + one Worker.

### Docker Compose Example

```yaml
services:
  master:
    image: mirrorgo:latest
    command: >
      master
      --addr :8080
      --db /data/state.db
      --auth-token "${AUTH_TOKEN}"
      --configs /Configs
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./Configs:/Configs:ro
      - ./data:/data
    restart: unless-stopped

  worker-1:
    image: mirrorgo:latest
    command: >
      worker
      --name worker-1
      --master http://master:8080
      --auth-token "${AUTH_TOKEN}"
      --addr :9090
      --labels storage=ssd
      --basedir /home/zjusct/mirrorgo
      --repodir /test1/mirrors/
    ports:
      - "9090:9090"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /var/lib/docker:/var/lib/docker
      - /test1/mirrors:/test1/mirrors
      - /home/zjusct/mirrorgo/logs:/home/zjusct/mirrorgo/logs
    restart: unless-stopped
```

### Backward Compatibility

None. This is an architectural rewrite. The single-node mode is removed. Minimum deployment is Master + 1 Worker.
