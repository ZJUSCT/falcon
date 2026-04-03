# MirrorGo

A cluster-aware sync controller for mirror sites.

## Architecture

MirrorGo uses a master-worker architecture. A single binary `mirrorgo` runs as either `master` or `worker` via subcommands.

- **Master**: scheduling, Web UI, worker management. No Docker dependency.
- **Worker**: registers to master, executes sync containers via Docker, reports status via WebSocket.

```
                  ┌──────────────┐
   UI / API ────► │    Master    │ ◄──── Configs/*.json
                  │  :8080       │
                  │  SQLite DB   │
                  └──┬───┬───┬──┘
           register  │   │   │  dispatch
          heartbeat  │   │   │  log proxy
          websocket  │   │   │
                  ┌──┴┐ ┌┴──┐┌┴──┐
                  │ W1│ │W2 ││W3 │  Workers
                  │:9090:9090:9090  (Docker)
                  └───┘ └───┘└───┘
```

## Quick Start

### Build

```bash
docker compose build
```

Or build the binary directly:

```bash
go build -o mirrorgo .
```

### Deploy (Docker Compose)

1. Create `.env` with a shared auth token:

```bash
echo "AUTH_TOKEN=$(openssl rand -hex 16)" > .env
```

2. Put repo configs in `Configs/` (JSON files, see `Configs/debian.json` for example).

3. Start:

```bash
docker compose up -d
```

This starts one master and one worker on the same host. The master Web UI is at `http://localhost:8080`.

### Multi-Host Deploy

On the master host, run only the master service. On each worker host, run a worker pointing to the master:

```bash
# Worker host
docker run -d \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/docker:/var/lib/docker \
  -v /data/mirrors:/data/mirrors \
  -v /data/mirrorgo/logs:/data/mirrorgo/logs \
  mirrorgo:latest \
  worker \
    --name worker-2 \
    --master http://master-host:8080 \
    --auth-token "$AUTH_TOKEN" \
    --addr :9090 \
    --labels storage=hdd,zone=b \
    --basedir /data/mirrorgo \
    --repodir /data/mirrors/
```

## CLI Reference

### Master

```
mirrorgo master [flags]

Flags:
  --addr       Listen address (default: :8080)
  --db         SQLite database path (default: state.db)
  --auth-token PSK token for worker auth (or AUTH_TOKEN env var)
  --configs    Config directory (default: Configs)
  --basedir    Base directory for mirrorgo.json/mirrorz.json output
```

### Worker

```
mirrorgo worker [flags]

Flags:
  --name       Worker name (unique, used as hostname in Docker Compose)
  --master     Master URL (e.g. http://master:8080)
  --auth-token PSK token (or AUTH_TOKEN env var)
  --addr       Listen address or reachable URL (default: :9090)
  --labels     Comma-separated key=value labels (e.g. storage=ssd,zone=a)
  --basedir    Base directory for logs (default: /home/zjusct/mirrorgo)
  --repodir    Mirror data directory (default: /test1/mirrors/)
  --dryrun     Simulate Docker execution (for testing)
```

## Node Affinity

Repos can be pinned to specific workers via `sync.node` or matched by labels via `sync.nodeSelector`:

```json
{
  "id": "debian",
  "sync": {
    "node": "worker-1",
    "nodeSelector": {"storage": "ssd"},
    ...
  }
}
```

- `node`: exact worker name, highest priority
- `nodeSelector`: all key-value pairs must match worker labels
- Neither set: schedules to any online worker

## API Endpoints

### Public (no auth)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/repos` | List repo configs |
| GET | `/api/jobs` | List all jobs |
| GET | `/api/actions` | List active actions |
| GET | `/api/actions/recent?limit=N` | Recent actions |
| GET | `/api/actions/by_repo?repo_id=X` | Actions for a repo |
| GET | `/api/queue` | Queue state |
| GET | `/api/workers` | List workers |
| GET | `/api/mirrors` | Mirror status |
| GET | `/mirrorz.json` | MirrorZ format |
| POST | `/api/jobs/next_attempt_now?repo_id=X` | Trigger immediate sync |
| POST | `/api/queue/pause` | Pause dispatch |
| POST | `/api/queue/continue` | Resume dispatch |
| POST | `/api/queue/set_max_concurrency?max=N` | Set parallelism |
| POST | `/api/queue/delete?repo_id=X` | Remove from queue |
| POST | `/api/workers/remove?name=X` | Remove offline worker |
| GET | `/api/logs/list?action_id=X` | List log files |
| GET | `/api/logs/raw?action_id=X&file=Y` | Download log |
| GET | `/api/logs/stream?action_id=X&file=Y` | Stream log (SSE) |

### Internal (PSK auth, `/api/internal/*`)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/internal/register` | Worker registration |
| POST | `/api/internal/heartbeat` | Worker heartbeat |
| GET | `/api/internal/ws` | WebSocket (status push) |
| POST | `/api/internal/dispatch` | Task dispatch (worker) |
| GET | `/api/internal/action_status` | Query action (worker) |

## Job State Machine

```
Waiting ──(time)──► Scheduled ──(dispatch)──► Running ──(done)──► Waiting
                                                                    ↑
Config deleted → Orphan                              NextAttemptAt = Now + Interval
```

## Worker State Machine

```
Unregistered ──(register)──► Online ◄──(re-register)── Offline
                               │                          ↑
                               └──(30s no heartbeat)──────┘
```

## Dryrun Mode

For testing without Docker:

```bash
mirrorgo worker --name test --master http://localhost:8080 --auth-token test --dryrun
```

Simulates container execution (5-15s random duration, always succeeds). Useful for validating scheduling, WebSocket communication, and API behavior.
