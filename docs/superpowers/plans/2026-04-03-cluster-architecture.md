# Cluster Architecture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor MirrorGo from single-node to master-worker cluster with label-based node affinity scheduling.

**Architecture:** Single binary with `master`/`worker` subcommands. Master handles scheduling, Web UI, and worker management via SQLite. Workers register to Master, execute Docker containers, and push status via WebSocket. Communication uses HTTP REST + WebSocket (PSK auth on internal endpoints).

**Tech Stack:** Go 1.24, gorilla/websocket, GORM/SQLite, Docker API (moby/moby/client), zerolog

**Spec:** `docs/superpowers/specs/2026-04-03-cluster-architecture-design.md`

---

## File Map

### New files to create

| File | Responsibility |
|---|---|
| `shared/types.go` | Repo, Job, Action, Volume, Worker, queue/action status constants — extracted from `job.go` |
| `shared/protocol.go` | Request/response structs for all Master↔Worker HTTP and WebSocket messages |
| `shared/auth.go` | PSK auth middleware and helper for HTTP + WebSocket |
| `master/master.go` | Master startup: init DB, load configs, load state, run recovery, start loops, start web server |
| `master/scheduler.go` | scheduleLoop + dispatchLoop with affinity matching, Worker selection, remote dispatch |
| `master/worker_manager.go` | Worker registration, heartbeat processing, offline detection, reconciliation, heartbeat diff |
| `master/db.go` | GORM models (JobModel, ActionModel, QueueItemModel, QueueStateModel, WorkerModel), init, CRUD |
| `master/queue.go` | Queue struct — moved from `queue.go`, unchanged |
| `master/web.go` | Public API handlers + log proxy to Workers — adapted from `web.go` |
| `master/mirrorz.go` | MirrorZ generation — moved from `mirrorz.go`, minimal changes |
| `master/ws_hub.go` | WebSocket hub: accept Worker connections, route action_status messages, dispatch ack |
| `worker/worker.go` | Worker startup: register, heartbeat loop, container recovery scan, WebSocket connect |
| `worker/docker.go` | StartContainer, CheckContainer, DeleteContainer — adapted from `docker.go`, deferred deletion |
| `worker/log.go` | Log dir management — moved from `log.go` |
| `worker/api.go` | Worker HTTP server: `/api/internal/dispatch`, `/api/internal/action_status`, log endpoints |
| `worker/ws_client.go` | WebSocket client: connect to Master, push action status, reconnect with backoff |
| `worker/action_cache.go` | LRU cache for last 1000 completed action results |

### Files to delete after migration

| File | Replaced by |
|---|---|
| `job.go` | `shared/types.go` |
| `queue.go` | `master/queue.go` |
| `db.go` | `master/db.go` |
| `docker.go` | `worker/docker.go` |
| `log.go` | `worker/log.go` |
| `web.go` | `master/web.go` + `worker/api.go` |
| `mirrorz.go` | `master/mirrorz.go` |
| `zfs.go` | deleted (empty file) |

### Files to modify

| File | Changes |
|---|---|
| `main.go` | Rewrite: subcommand parser (`master`/`worker`), flag parsing, delegate to `master.Run()` or `worker.Run()` |
| `go.mod` | Add `github.com/gorilla/websocket`, remove nothing |
| `Dockerfile` | Update entrypoint, expose both 8080 and 9090 |
| `compose.yml` | Rewrite: separate master and worker services |

---

## Task Breakdown

### Task 1: Extract shared types

Move type definitions from `job.go` to `shared/types.go` so both master and worker packages can import them.

**Files:**
- Create: `shared/types.go`
- Delete after: `job.go` (but keep until all references updated in later tasks)

- [ ] **Step 1: Create `shared/` directory and `shared/types.go`**

```go
// shared/types.go
package shared

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

type I18NString map[string]string

type Info struct {
	Name        I18NString `json:"name"`
	Description I18NString `json:"description"`
	Type        string     `json:"type"`
	Upstream    string     `json:"upstream"`
	Url         string     `json:"url"`
}

type Volume struct {
	Source      string `json:"src"`
	Destination string `json:"dst"`
}

func (v Volume) Value() (driver.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (v *Volume) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, v)
	case string:
		return json.Unmarshal([]byte(data), v)
	default:
		return fmt.Errorf("unsupported Scan type for Volume: %T", src)
	}
}

type VolumeList []Volume

func (vl VolumeList) Value() (driver.Value, error) {
	b, err := json.Marshal(vl)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (vl *VolumeList) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, vl)
	case string:
		return json.Unmarshal([]byte(data), vl)
	default:
		return fmt.Errorf("unsupported Scan type for VolumeList: %T", src)
	}
}

type StringList []string

func (sl StringList) Value() (driver.Value, error) {
	b, err := json.Marshal(sl)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (sl *StringList) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, sl)
	case string:
		return json.Unmarshal([]byte(data), sl)
	default:
		return fmt.Errorf("unsupported Scan type for StringList: %T", src)
	}
}

type IntervalConfig struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type SyncConfig struct {
	JobName      string            `json:"jobName"`
	Interval     IntervalConfig    `json:"interval"`
	Timeout      string            `json:"timeout"`
	Image        string            `json:"image"`
	Volumes      []Volume          `json:"volumes"`
	Command      []string          `json:"command"`
	Environments []string          `json:"environments"`
	Node         string            `json:"node,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type Repo struct {
	RepoID     string     `json:"id"`
	Info       Info       `json:"info"`
	SyncParams SyncConfig `json:"sync"`
}

type Job struct {
	RepoID    string    `json:"id"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`

	LastSuccessAt time.Time `json:"last_success_at"`
	LastFailureAt time.Time `json:"last_failure_at"`
	LastAttemptAt time.Time `json:"last_attempt_at"`

	NextAttemptAt    time.Time `json:"next_attempt_at"`
	LastActionStatus string    `json:"last_action_status"`

	Actions []string `json:"actions"`
}

type Action struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updated_at"`

	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	WorkerName string `json:"worker_name"`

	ContainerID         string     `json:"container_id"`
	ContainerName       string     `json:"container_name"`
	ContainerImage      string     `json:"container_image"`
	ContainerStatus     string     `json:"container_status"`
	ContainerExitCode   int        `json:"container_exit_code"`
	ContainerExitSignal int        `json:"container_exit_signal"`
	ContainerExitReason string     `json:"container_exit_reason"`
	ContainerVolumes    VolumeList `json:"container_volumes" gorm:"type:TEXT"`
	ContainerEnv        StringList `json:"container_env" gorm:"type:TEXT"`
	ContainerCommand    StringList `json:"container_command" gorm:"type:TEXT"`
	ContainerTimeout    string     `json:"container_timeout"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type Worker struct {
	Name           string            `json:"name"`
	Addr           string            `json:"addr"`
	Labels         map[string]string `json:"labels"`
	Status         string            `json:"status"`
	LastHeartbeat  time.Time         `json:"last_heartbeat"`
	RunningActions []string          `json:"running_actions"`
	RegisteredAt   time.Time         `json:"registered_at"`
}

// Status constants
const (
	JobStatusWaiting   = "Waiting"
	JobStatusScheduled = "Scheduled"
	JobStatusRunning   = "Running"
	JobStatusOrphan    = "Orphan"

	ActionStatusRunning      = "Running"
	ActionStatusReconciling  = "Reconciling"
	ActionStatusSucceeded    = "Succeeded"
	ActionStatusFailed       = "Failed"

	ContainerStatusStarting   = "Starting"
	ContainerStatusNotCreated = "NotCreated"
	ContainerStatusOrphan     = "Orphan"
	ContainerStatusRunning    = "Running"
	ContainerStatusExited     = "Exited"

	WorkerStatusOnline  = "Online"
	WorkerStatusOffline = "Offline"
)
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./shared/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add shared/types.go
git -c commit.gpgsign=false commit -m "refactor: extract shared types to shared/types.go"
```

---

### Task 2: Create shared protocol definitions

Define all Master↔Worker HTTP and WebSocket message types.

**Files:**
- Create: `shared/protocol.go`

- [ ] **Step 1: Create `shared/protocol.go`**

```go
// shared/protocol.go
package shared

import "time"

// --- Worker -> Master: Registration ---

type RegisterRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Addr   string            `json:"addr"`
}

type RegisterResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// --- Worker -> Master: Heartbeat ---

type HeartbeatRequest struct {
	Name           string   `json:"name"`
	RunningActions []string `json:"running_actions"`
}

type HeartbeatResponse struct {
	OK bool `json:"ok"`
}

// --- Master -> Worker: Dispatch ---

type DispatchRequest struct {
	Action DispatchAction `json:"action"`
}

type DispatchAction struct {
	ID               string     `json:"id"`
	JobID            string     `json:"job_id"`
	ContainerImage   string     `json:"container_image"`
	ContainerCommand []string   `json:"container_command"`
	ContainerVolumes []Volume   `json:"container_volumes"`
	ContainerEnv     []string   `json:"container_env"`
	ContainerTimeout string     `json:"container_timeout"`
}

type DispatchResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// --- WebSocket messages (Worker -> Master) ---

type WSMessage struct {
	Type            string    `json:"type"`
	ActionID        string    `json:"action_id"`
	Status          string    `json:"status"`
	ContainerStatus string    `json:"container_status"`
	ExitCode        int       `json:"exit_code"`
	ExitReason      string    `json:"exit_reason"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// --- WebSocket messages (Master -> Worker) ---

type WSAck struct {
	Type     string `json:"type"`     // "ack"
	ActionID string `json:"action_id"`
}

// --- Master -> Worker: Action Status Query ---

type ActionStatusResponse struct {
	Found     bool      `json:"found"`
	ActionID  string    `json:"action_id"`
	Status    string    `json:"status"`
	ExitCode  int       `json:"exit_code"`
	ExitReason string   `json:"exit_reason"`
	StartedAt time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./shared/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add shared/protocol.go
git -c commit.gpgsign=false commit -m "feat: add shared protocol definitions for master-worker communication"
```

---

### Task 3: Create shared auth middleware

PSK validation for internal endpoints.

**Files:**
- Create: `shared/auth.go`

- [ ] **Step 1: Create `shared/auth.go`**

```go
// shared/auth.go
package shared

import (
	"net/http"
	"strings"
)

// InternalAuthMiddleware returns middleware that validates Bearer token on /api/internal/* routes.
// Other routes pass through without auth.
func InternalAuthMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		// Allow WebSocket upgrade with token in query param
		if r.URL.Query().Get("token") == token {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" || auth != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValidateToken checks a Bearer token from an HTTP request.
func ValidateToken(r *http.Request, token string) bool {
	// Check query param first (for WebSocket)
	if r.URL.Query().Get("token") == token {
		return true
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+token
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./shared/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add shared/auth.go
git -c commit.gpgsign=false commit -m "feat: add PSK auth middleware for internal API endpoints"
```

---

### Task 4: Create master/queue.go

Move Queue struct from root `queue.go` to `master/` package. No logic changes.

**Files:**
- Create: `master/queue.go`

- [ ] **Step 1: Create `master/queue.go`**

Copy the entire content of `queue.go` but change `package main` to `package master`. No other changes.

```go
// master/queue.go
package master

import "sync"

// (rest of queue.go content unchanged, just package name changed)
```

Copy all functions: `NewQueue`, `Enqueue`, `Dequeue`, `Remove`, `Len`, `Snapshot`, `ReplaceAll`, `SetPaused`, `IsPaused`, `SetMaxConcurrency`, `GetMaxConcurrency`, `MoveToHead`, `MoveToTail`, `MoveBefore`, `MoveAfter`.

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors (master package has no other dependencies yet)

- [ ] **Step 3: Commit**

```bash
git add master/queue.go
git -c commit.gpgsign=false commit -m "refactor: move Queue to master/ package"
```

---

### Task 5: Create master/db.go

Migrate DB layer from root `db.go` to `master/` package. Add WorkerModel. Use shared types.

**Files:**
- Create: `master/db.go`

- [ ] **Step 1: Create `master/db.go`**

```go
// master/db.go
package master

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var gormDB *gorm.DB

type JobModel struct {
	RepoID           string            `gorm:"primaryKey;column:repo_id"`
	Status           string            `gorm:"column:status"`
	UpdatedAt        time.Time         `gorm:"column:updated_at"`
	LastSuccessAt    time.Time         `gorm:"column:last_success_at"`
	LastFailureAt    time.Time         `gorm:"column:last_failure_at"`
	LastAttemptAt    time.Time         `gorm:"column:last_attempt_at"`
	NextAttemptAt    time.Time         `gorm:"column:next_attempt_at"`
	Actions          shared.StringList `gorm:"column:actions;type:TEXT"`
	LastActionStatus string            `gorm:"column:last_action_status"`
}

func (JobModel) TableName() string { return "jobs" }

type ActionModel struct {
	ID                  string            `gorm:"primaryKey;column:id"`
	JobID               string            `gorm:"column:job_id;index"`
	Status              string            `gorm:"column:status"`
	Message             string            `gorm:"column:message"`
	WorkerName          string            `gorm:"column:worker_name"`
	UpdatedAt           time.Time         `gorm:"column:updated_at"`
	ContainerID         string            `gorm:"column:container_id"`
	ContainerName       string            `gorm:"column:container_name"`
	ContainerImage      string            `gorm:"column:container_image"`
	ContainerStatus     string            `gorm:"column:container_status"`
	ContainerExitCode   int               `gorm:"column:container_exit_code"`
	ContainerExitSignal int               `gorm:"column:container_exit_signal"`
	ContainerExitReason string            `gorm:"column:container_exit_reason"`
	ContainerVolumes    shared.VolumeList `gorm:"column:container_volumes;type:TEXT"`
	ContainerEnv        shared.StringList `gorm:"column:container_env;type:TEXT"`
	ContainerCommand    shared.StringList `gorm:"column:container_command;type:TEXT"`
	ContainerTimeout    string            `gorm:"column:container_timeout"`
	CreatedAt           time.Time         `gorm:"column:created_at"`
	StartedAt           time.Time         `gorm:"column:started_at"`
	FinishedAt          time.Time         `gorm:"column:finished_at"`
}

func (ActionModel) TableName() string { return "actions" }

type QueueItemModel struct {
	ID         uint      `gorm:"primaryKey;column:id"`
	JobID      string    `gorm:"column:job_id;index"`
	EnqueuedAt time.Time `gorm:"column:enqueued_at"`
}

func (QueueItemModel) TableName() string { return "queue" }

type QueueStateModel struct {
	ID             uint `gorm:"primaryKey;column:id"`
	Paused         bool `gorm:"column:paused"`
	MaxConcurrency int  `gorm:"column:max_concurrency;default:1"`
}

func (QueueStateModel) TableName() string { return "queue_state" }

// JSONMap is a helper for storing map[string]string as JSON in SQLite
type JSONMap map[string]string

func (m JSONMap) Value() (interface{}, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (m *JSONMap) Scan(src interface{}) error {
	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, m)
	case string:
		return json.Unmarshal([]byte(v), m)
	default:
		*m = nil
		return nil
	}
}

type WorkerModel struct {
	Name           string    `gorm:"primaryKey;column:name"`
	Addr           string    `gorm:"column:addr"`
	Labels         JSONMap   `gorm:"column:labels;type:TEXT"`
	Status         string    `gorm:"column:status"`
	LastHeartbeat  time.Time `gorm:"column:last_heartbeat"`
	RunningActions shared.StringList `gorm:"column:running_actions;type:TEXT"`
	RegisteredAt   time.Time `gorm:"column:registered_at"`
}

func (WorkerModel) TableName() string { return "workers" }

func InitDB(path string) error {
	var err error
	gormDB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return err
	}
	if raw, err := gormDB.DB(); err == nil {
		_, _ = raw.Exec("PRAGMA journal_mode=WAL;")
		_, _ = raw.Exec("PRAGMA synchronous=NORMAL;")
	}
	if err := gormDB.AutoMigrate(&JobModel{}, &ActionModel{}, &QueueItemModel{}, &QueueStateModel{}, &WorkerModel{}); err != nil {
		return err
	}
	return nil
}

// --- Job CRUD ---

func LoadJobsFromDB() (map[string]*shared.Job, error) {
	var rows []JobModel
	if err := gormDB.Find(&rows).Error; err != nil {
		return nil, err
	}
	jobs := make(map[string]*shared.Job, len(rows))
	for _, m := range rows {
		jobs[m.RepoID] = &shared.Job{
			RepoID:           m.RepoID,
			Status:           m.Status,
			UpdatedAt:        m.UpdatedAt,
			LastSuccessAt:    m.LastSuccessAt,
			LastFailureAt:    m.LastFailureAt,
			LastAttemptAt:    m.LastAttemptAt,
			NextAttemptAt:    m.NextAttemptAt,
			Actions:          m.Actions,
			LastActionStatus: m.LastActionStatus,
		}
	}
	log.Info().Int("jobs", len(jobs)).Msg("Loaded jobs from DB")
	return jobs, nil
}

func UpsertJob(j *shared.Job) error {
	m := JobModel{
		RepoID:           j.RepoID,
		Status:           j.Status,
		UpdatedAt:        j.UpdatedAt,
		LastSuccessAt:    j.LastSuccessAt,
		LastFailureAt:    j.LastFailureAt,
		LastAttemptAt:    j.LastAttemptAt,
		NextAttemptAt:    j.NextAttemptAt,
		Actions:          j.Actions,
		LastActionStatus: j.LastActionStatus,
	}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

// --- Action CRUD ---

func UpsertAction(a *shared.Action) error {
	m := ActionModel{
		ID:                  a.ID,
		JobID:               a.JobID,
		Status:              a.Status,
		Message:             a.Message,
		WorkerName:          a.WorkerName,
		UpdatedAt:           a.UpdatedAt,
		ContainerID:         a.ContainerID,
		ContainerName:       a.ContainerName,
		ContainerImage:      a.ContainerImage,
		ContainerStatus:     a.ContainerStatus,
		ContainerExitCode:   a.ContainerExitCode,
		ContainerExitSignal: a.ContainerExitSignal,
		ContainerExitReason: a.ContainerExitReason,
		ContainerVolumes:    a.ContainerVolumes,
		ContainerEnv:        a.ContainerEnv,
		ContainerCommand:    a.ContainerCommand,
		ContainerTimeout:    a.ContainerTimeout,
		CreatedAt:           a.CreatedAt,
		StartedAt:           a.StartedAt,
		FinishedAt:          a.FinishedAt,
	}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

func LoadActiveActionsFromDB() (map[string]*shared.Action, error) {
	var rows []ActionModel
	if err := gormDB.Where("status IN ?", []string{shared.ActionStatusRunning, shared.ActionStatusReconciling}).Find(&rows).Error; err != nil {
		return nil, err
	}
	actions := make(map[string]*shared.Action, len(rows))
	for _, m := range rows {
		actions[m.ID] = actionModelToAction(&m)
	}
	log.Info().Int("actions", len(actions)).Msg("Loaded active actions from DB")
	return actions, nil
}

func GetActionByID(id string) (*shared.Action, error) {
	var m ActionModel
	if err := gormDB.First(&m, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return actionModelToAction(&m), nil
}

func GetActionsByRepo(repoID string, limit int) ([]shared.Action, error) {
	var rows []ActionModel
	if err := gormDB.Where("job_id = ?", repoID).Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]shared.Action, 0, len(rows))
	for _, m := range rows {
		out = append(out, *actionModelToAction(&m))
	}
	return out, nil
}

func GetActionsRecent(limit int) ([]shared.Action, error) {
	var rows []ActionModel
	if err := gormDB.Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]shared.Action, 0, len(rows))
	for _, m := range rows {
		out = append(out, *actionModelToAction(&m))
	}
	return out, nil
}

func actionModelToAction(m *ActionModel) *shared.Action {
	return &shared.Action{
		ID:                  m.ID,
		UpdatedAt:           m.UpdatedAt,
		JobID:               m.JobID,
		Status:              m.Status,
		Message:             m.Message,
		WorkerName:          m.WorkerName,
		ContainerID:         m.ContainerID,
		ContainerName:       m.ContainerName,
		ContainerImage:      m.ContainerImage,
		ContainerStatus:     m.ContainerStatus,
		ContainerExitCode:   m.ContainerExitCode,
		ContainerExitSignal: m.ContainerExitSignal,
		ContainerExitReason: m.ContainerExitReason,
		ContainerVolumes:    m.ContainerVolumes,
		ContainerEnv:        m.ContainerEnv,
		ContainerCommand:    m.ContainerCommand,
		ContainerTimeout:    m.ContainerTimeout,
		CreatedAt:           m.CreatedAt,
		StartedAt:           m.StartedAt,
		FinishedAt:          m.FinishedAt,
	}
}

// --- Queue persistence ---

func DBEnqueue(jobID string) error {
	return gormDB.Create(&QueueItemModel{JobID: jobID, EnqueuedAt: time.Now()}).Error
}

func DBDequeueOne(jobID string) error {
	var qi QueueItemModel
	tx := gormDB.Where("job_id = ?", jobID).Order("id asc").First(&qi)
	if tx.Error != nil {
		return tx.Error
	}
	return gormDB.Delete(&qi).Error
}

func DBDeleteAllQueueByJob(jobID string) error {
	return gormDB.Where("job_id = ?", jobID).Delete(&QueueItemModel{}).Error
}

func LoadQueueItemsFromDB() ([]string, error) {
	var items []QueueItemModel
	if err := gormDB.Order("id asc").Find(&items).Error; err != nil {
		return nil, err
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.JobID
	}
	return out, nil
}

func DBFlushQueue(items []string) error {
	if err := gormDB.Exec("DELETE FROM queue").Error; err != nil {
		return err
	}
	for _, id := range items {
		if err := DBEnqueue(id); err != nil {
			return err
		}
	}
	return nil
}

// --- Queue state ---

func DBGetQueueState() (paused bool, maxConcurrency int, err error) {
	var row QueueStateModel
	if err := gormDB.First(&row, 1).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, 1, nil
		}
		return false, 1, err
	}
	return row.Paused, row.MaxConcurrency, nil
}

func DBSetQueueState(paused bool, maxConcurrency int) error {
	row := QueueStateModel{ID: 1, Paused: paused, MaxConcurrency: maxConcurrency}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// --- Worker CRUD ---

func UpsertWorker(w *shared.Worker) error {
	labelsJSON, _ := json.Marshal(w.Labels)
	actionsJSON, _ := json.Marshal(w.RunningActions)
	m := WorkerModel{
		Name:           w.Name,
		Addr:           w.Addr,
		Labels:         JSONMap(w.Labels),
		Status:         w.Status,
		LastHeartbeat:  w.LastHeartbeat,
		RunningActions: shared.StringList(w.RunningActions),
		RegisteredAt:   w.RegisteredAt,
	}
	_ = labelsJSON
	_ = actionsJSON
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

func LoadWorkersFromDB() (map[string]*shared.Worker, error) {
	var rows []WorkerModel
	if err := gormDB.Find(&rows).Error; err != nil {
		return nil, err
	}
	workers := make(map[string]*shared.Worker, len(rows))
	for _, m := range rows {
		workers[m.Name] = &shared.Worker{
			Name:           m.Name,
			Addr:           m.Addr,
			Labels:         map[string]string(m.Labels),
			Status:         m.Status,
			LastHeartbeat:  m.LastHeartbeat,
			RunningActions: []string(m.RunningActions),
			RegisteredAt:   m.RegisteredAt,
		}
	}
	return workers, nil
}

func DeleteWorker(name string) error {
	return gormDB.Delete(&WorkerModel{}, "name = ?", name).Error
}

// --- Bulk status updates for restart recovery ---

func MarkAllRunningActionsReconciling() error {
	return gormDB.Model(&ActionModel{}).
		Where("status = ?", shared.ActionStatusRunning).
		Update("status", shared.ActionStatusReconciling).Error
}

func RevertScheduledJobsToWaiting() error {
	return gormDB.Model(&JobModel{}).
		Where("status = ?", shared.JobStatusScheduled).
		Updates(map[string]interface{}{
			"status":          shared.JobStatusWaiting,
			"next_attempt_at": time.Now(),
			"updated_at":      time.Now(),
		}).Error
}

func MarkAllWorkersOffline() error {
	return gormDB.Model(&WorkerModel{}).
		Where("status = ?", shared.WorkerStatusOnline).
		Update("status", shared.WorkerStatusOffline).Error
}

func RawDB() (*sql.DB, error) { return gormDB.DB() }
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add master/db.go
git -c commit.gpgsign=false commit -m "feat: add master DB layer with Worker model and recovery helpers"
```

---

### Task 6: Create master/worker_manager.go

Worker registration, heartbeat processing, offline detection, reconciliation.

**Files:**
- Create: `master/worker_manager.go`

- [ ] **Step 1: Create `master/worker_manager.go`**

```go
// master/worker_manager.go
package master

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

type WorkerManager struct {
	mu      sync.RWMutex
	workers map[string]*shared.Worker
	token   string

	// Callbacks
	onWorkerOnline  func(name string)
	onWorkerOffline func(name string)
}

func NewWorkerManager(token string) *WorkerManager {
	return &WorkerManager{
		workers: make(map[string]*shared.Worker),
		token:   token,
	}
}

func (wm *WorkerManager) SetCallbacks(onOnline, onOffline func(string)) {
	wm.onWorkerOnline = onOnline
	wm.onWorkerOffline = onOffline
}

func (wm *WorkerManager) LoadFromDB() error {
	workers, err := LoadWorkersFromDB()
	if err != nil {
		return err
	}
	wm.mu.Lock()
	wm.workers = workers
	wm.mu.Unlock()
	return nil
}

// Register handles POST /api/internal/register
func (wm *WorkerManager) Register(w http.ResponseWriter, r *http.Request) {
	var req shared.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, shared.RegisterResponse{OK: false, Message: "invalid request"})
		return
	}

	wm.mu.Lock()
	existing, exists := wm.workers[req.Name]

	if exists && existing.Status == shared.WorkerStatusOnline {
		wm.mu.Unlock()
		writeJSON(w, http.StatusConflict, shared.RegisterResponse{OK: false, Message: "worker already online"})
		return
	}

	worker := &shared.Worker{
		Name:           req.Name,
		Addr:           req.Addr,
		Labels:         req.Labels,
		Status:         shared.WorkerStatusOnline,
		LastHeartbeat:  time.Now(),
		RunningActions: nil,
		RegisteredAt:   time.Now(),
	}
	if exists {
		worker.RegisteredAt = existing.RegisteredAt
	}
	wm.workers[req.Name] = worker
	wm.mu.Unlock()

	if err := UpsertWorker(worker); err != nil {
		log.Error().Err(err).Str("worker", req.Name).Msg("Failed to persist worker")
	}

	log.Info().Str("worker", req.Name).Str("addr", req.Addr).Msg("Worker registered")

	if wm.onWorkerOnline != nil {
		wm.onWorkerOnline(req.Name)
	}

	writeJSON(w, http.StatusOK, shared.RegisterResponse{OK: true})
}

// Heartbeat handles POST /api/internal/heartbeat
func (wm *WorkerManager) Heartbeat(w http.ResponseWriter, r *http.Request) {
	var req shared.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, shared.HeartbeatResponse{OK: false})
		return
	}

	wm.mu.Lock()
	worker, exists := wm.workers[req.Name]
	if !exists {
		wm.mu.Unlock()
		writeJSON(w, http.StatusNotFound, shared.HeartbeatResponse{OK: false})
		return
	}

	prevActions := worker.RunningActions
	worker.LastHeartbeat = time.Now()
	worker.RunningActions = req.RunningActions
	worker.Status = shared.WorkerStatusOnline
	wm.mu.Unlock()

	if err := UpsertWorker(worker); err != nil {
		log.Error().Err(err).Str("worker", req.Name).Msg("Failed to persist heartbeat")
	}

	// Diff running actions to detect lost WebSocket completion messages
	go wm.diffActions(worker, prevActions, req.RunningActions)

	writeJSON(w, http.StatusOK, shared.HeartbeatResponse{OK: true})
}

// diffActions detects actions that Master thinks are Running/Reconciling on this Worker
// but are not in the heartbeat's running_actions list.
// This is called by the master's state manager, not directly by WorkerManager.
// The actual diff logic will be wired in master.go where we have access to ActiveActions.
func (wm *WorkerManager) diffActions(worker *shared.Worker, prev, current []string) {
	// This is a hook point. The actual implementation will be in master.go
	// where we have access to the ActiveActions map and can query the worker.
}

// CheckOffline scans workers and marks any with stale heartbeats as Offline.
// Should be called periodically (every 5s).
func (wm *WorkerManager) CheckOffline(threshold time.Duration) []string {
	now := time.Now()
	var offlined []string

	wm.mu.Lock()
	for name, worker := range wm.workers {
		if worker.Status == shared.WorkerStatusOnline && now.Sub(worker.LastHeartbeat) > threshold {
			worker.Status = shared.WorkerStatusOffline
			offlined = append(offlined, name)
			if err := UpsertWorker(worker); err != nil {
				log.Error().Err(err).Str("worker", name).Msg("Failed to persist offline status")
			}
			log.Warn().Str("worker", name).Dur("since_heartbeat", now.Sub(worker.LastHeartbeat)).Msg("Worker marked offline")
		}
	}
	wm.mu.Unlock()

	for _, name := range offlined {
		if wm.onWorkerOffline != nil {
			wm.onWorkerOffline(name)
		}
	}

	return offlined
}

// OfflineCheckLoop runs CheckOffline every 5 seconds.
func (wm *WorkerManager) OfflineCheckLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wm.CheckOffline(30 * time.Second)
		}
	}
}

// GetOnlineWorkers returns a snapshot of all online workers.
func (wm *WorkerManager) GetOnlineWorkers() []*shared.Worker {
	wm.mu.RLock()
	defer wm.mu.RUnlock()
	var out []*shared.Worker
	for _, w := range wm.workers {
		if w.Status == shared.WorkerStatusOnline {
			cp := *w
			out = append(out, &cp)
		}
	}
	return out
}

// GetWorker returns a copy of a worker by name.
func (wm *WorkerManager) GetWorker(name string) (*shared.Worker, bool) {
	wm.mu.RLock()
	defer wm.mu.RUnlock()
	w, ok := wm.workers[name]
	if !ok {
		return nil, false
	}
	cp := *w
	return &cp, true
}

// GetAllWorkers returns a copy of all workers.
func (wm *WorkerManager) GetAllWorkers() []*shared.Worker {
	wm.mu.RLock()
	defer wm.mu.RUnlock()
	out := make([]*shared.Worker, 0, len(wm.workers))
	for _, w := range wm.workers {
		cp := *w
		out = append(out, &cp)
	}
	return out
}

// RemoveWorker removes an offline worker. Returns error if worker is online.
func (wm *WorkerManager) RemoveWorker(name string) error {
	wm.mu.Lock()
	w, ok := wm.workers[name]
	if !ok {
		wm.mu.Unlock()
		return fmt.Errorf("worker %q not found", name)
	}
	if w.Status == shared.WorkerStatusOnline {
		wm.mu.Unlock()
		return fmt.Errorf("worker %q is online, cannot remove", name)
	}
	delete(wm.workers, name)
	wm.mu.Unlock()
	return DeleteWorker(name)
}

// MatchWorker checks if a worker matches the repo's affinity requirements.
func MatchWorker(worker *shared.Worker, repo *shared.Repo) bool {
	sync := repo.SyncParams
	// Exact node match takes priority
	if sync.Node != "" {
		return worker.Name == sync.Node
	}
	// Label matching: all selectors must match
	if len(sync.NodeSelector) > 0 {
		for k, v := range sync.NodeSelector {
			if worker.Labels[k] != v {
				return false
			}
		}
	}
	return true
}

// DispatchToWorker sends a dispatch request to a worker.
func DispatchToWorker(worker *shared.Worker, req *shared.DispatchRequest, token string) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	url := worker.Addr + "/api/internal/dispatch"
	httpReq, err := http.NewRequest("POST", url, io.NopCloser(
		// Use bytes.NewReader instead
		nil,
	))
	if err != nil {
		return err
	}
	// Fix: use bytes
	httpReq.Body = io.NopCloser(bytesReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("dispatch request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dispatch failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var dresp shared.DispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&dresp); err != nil {
		return fmt.Errorf("failed to decode dispatch response: %w", err)
	}
	if !dresp.OK {
		return fmt.Errorf("worker rejected dispatch: %s", dresp.Message)
	}
	return nil
}

// QueryActionStatus queries a worker for the final status of an action.
func QueryActionStatus(worker *shared.Worker, actionID string, token string) (*shared.ActionStatusResponse, error) {
	url := fmt.Sprintf("%s/api/internal/action_status?id=%s", worker.Addr, actionID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result shared.ActionStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// helper
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type bytesReaderWrapper struct {
	data []byte
	pos  int
}

func bytesReader(b []byte) *bytesReaderWrapper {
	return &bytesReaderWrapper{data: b}
}

func (r *bytesReaderWrapper) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add master/worker_manager.go
git -c commit.gpgsign=false commit -m "feat: add WorkerManager with registration, heartbeat, offline detection, affinity matching"
```

---

### Task 7: Create master/ws_hub.go

WebSocket hub to accept Worker connections and route action status messages.

**Files:**
- Create: `master/ws_hub.go`

- [ ] **Step 1: Add gorilla/websocket dependency**

Run: `cd /Users/star/mirrorgo && go get github.com/gorilla/websocket`

- [ ] **Step 2: Create `master/ws_hub.go`**

```go
// master/ws_hub.go
package master

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WSHub manages WebSocket connections from workers.
type WSHub struct {
	mu    sync.RWMutex
	conns map[string]*websocket.Conn // worker name -> conn
	token string

	// Called when an action status message is received
	OnActionStatus func(workerName string, msg *shared.WSMessage)
}

func NewWSHub(token string) *WSHub {
	return &WSHub{
		conns: make(map[string]*websocket.Conn),
		token: token,
	}
}

// HandleWS handles GET /api/internal/ws?name=xxx&token=xxx
func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Str("worker", name).Msg("WebSocket upgrade failed")
		return
	}

	h.mu.Lock()
	old, exists := h.conns[name]
	if exists && old != nil {
		old.Close()
	}
	h.conns[name] = conn
	h.mu.Unlock()

	log.Info().Str("worker", name).Msg("WebSocket connected")

	defer func() {
		h.mu.Lock()
		if h.conns[name] == conn {
			delete(h.conns, name)
		}
		h.mu.Unlock()
		conn.Close()
		log.Info().Str("worker", name).Msg("WebSocket disconnected")
	}()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Error().Err(err).Str("worker", name).Msg("WebSocket read error")
			}
			return
		}

		var msg shared.WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Error().Err(err).Str("worker", name).Msg("Invalid WebSocket message")
			continue
		}

		switch msg.Type {
		case "action_status":
			if h.OnActionStatus != nil {
				h.OnActionStatus(name, &msg)
			}
		default:
			log.Warn().Str("worker", name).Str("type", msg.Type).Msg("Unknown WebSocket message type")
		}
	}
}

// SendAck sends an acknowledgment to a worker for a completed action.
func (h *WSHub) SendAck(workerName string, actionID string) {
	h.mu.RLock()
	conn, ok := h.conns[workerName]
	h.mu.RUnlock()
	if !ok || conn == nil {
		return
	}
	ack := shared.WSAck{Type: "ack", ActionID: actionID}
	data, _ := json.Marshal(ack)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Error().Err(err).Str("worker", workerName).Str("action", actionID).Msg("Failed to send ack")
	}
}

// IsConnected checks if a worker has an active WebSocket connection.
func (h *WSHub) IsConnected(workerName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.conns[workerName]
	return ok && conn != nil
}
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add master/ws_hub.go
git -c commit.gpgsign=false commit -m "feat: add WebSocket hub for worker action status push"
```

---

### Task 8: Create master/scheduler.go

Schedule loop + dispatch loop with affinity-aware dispatching to remote Workers.

**Files:**
- Create: `master/scheduler.go`

- [ ] **Step 1: Create `master/scheduler.go`**

```go
// master/scheduler.go
package master

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// State holds all in-memory state for the master.
type State struct {
	Repos         map[string]shared.Repo
	ReposMu       sync.RWMutex
	Jobs          map[string]*shared.Job
	JobsMu        sync.RWMutex
	ActiveActions map[string]*shared.Action
	ActionsMu     sync.RWMutex
	JobQueue      *Queue
	WorkerMgr     *WorkerManager
	WSHub         *WSHub
	Token         string
}

func (s *State) ScheduleLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			scheduled := 0
			s.JobsMu.Lock()
			for id, job := range s.Jobs {
				if job.Status == shared.JobStatusWaiting && !job.NextAttemptAt.After(now) {
					job.Status = shared.JobStatusScheduled
					job.UpdatedAt = now
					s.JobQueue.Enqueue(id)
					_ = DBEnqueue(id)
					scheduled++
				}
			}
			s.JobsMu.Unlock()
			if scheduled > 0 {
				log.Debug().Int("scheduled", scheduled).Int("queue_len", s.JobQueue.Len()).Msg("Jobs scheduled")
			}
		}
	}
}

func (s *State) DispatchLoop(ctx context.Context) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.dispatchTick()
		}
	}
}

func (s *State) dispatchTick() {
	s.ActionsMu.RLock()
	activeCount := len(s.ActiveActions)
	s.ActionsMu.RUnlock()

	maxConcurrency := s.JobQueue.GetMaxConcurrency()
	if activeCount >= maxConcurrency {
		return
	}

	queueLen := s.JobQueue.Len()
	if queueLen == 0 {
		return
	}

	requeued := 0
	for requeued < queueLen && activeCount < maxConcurrency {
		id, ok := s.JobQueue.Dequeue()
		if !ok {
			break
		}
		_ = DBDequeueOne(id)

		s.JobsMu.RLock()
		job, jobOK := s.Jobs[id]
		s.JobsMu.RUnlock()
		if !jobOK || job.Status != shared.JobStatusScheduled {
			continue
		}

		s.ReposMu.RLock()
		repo, repoOK := s.Repos[id]
		s.ReposMu.RUnlock()
		if !repoOK {
			continue
		}

		// Find matching online worker
		worker := s.selectWorker(&repo)
		if worker == nil {
			// No available worker, put back to tail
			s.JobQueue.Enqueue(id)
			_ = DBEnqueue(id)
			requeued++
			continue
		}

		// Dispatch
		actionID := strconv.FormatInt(time.Now().UnixNano(), 10)
		dispReq := &shared.DispatchRequest{
			Action: shared.DispatchAction{
				ID:               actionID,
				JobID:            repo.RepoID,
				ContainerImage:   repo.SyncParams.Image,
				ContainerCommand: repo.SyncParams.Command,
				ContainerVolumes: repo.SyncParams.Volumes,
				ContainerEnv:     repo.SyncParams.Environments,
				ContainerTimeout: repo.SyncParams.Timeout,
			},
		}

		err := DispatchToWorker(worker, dispReq, s.Token)
		if err != nil {
			log.Error().Err(err).Str("job", id).Str("worker", worker.Name).Msg("Dispatch failed")
			// Put back to tail
			s.JobQueue.Enqueue(id)
			_ = DBEnqueue(id)
			requeued++
			continue
		}

		// Success: create Action record
		now := time.Now()
		action := &shared.Action{
			ID:               actionID,
			JobID:            repo.RepoID,
			Status:           shared.ActionStatusRunning,
			WorkerName:       worker.Name,
			ContainerImage:   repo.SyncParams.Image,
			ContainerCommand: repo.SyncParams.Command,
			ContainerVolumes: repo.SyncParams.Volumes,
			ContainerEnv:     repo.SyncParams.Environments,
			ContainerTimeout: repo.SyncParams.Timeout,
			ContainerStatus:  shared.ContainerStatusStarting,
			CreatedAt:        now,
			StartedAt:        now,
			UpdatedAt:        now,
		}

		s.ActionsMu.Lock()
		s.ActiveActions[actionID] = action
		s.ActionsMu.Unlock()

		s.JobsMu.Lock()
		job.Status = shared.JobStatusRunning
		job.LastAttemptAt = now
		job.UpdatedAt = now
		job.LastActionStatus = shared.ActionStatusRunning
		job.Actions = append(job.Actions, actionID)
		s.JobsMu.Unlock()

		if err := UpsertAction(action); err != nil {
			log.Error().Err(err).Str("action", actionID).Msg("Failed to persist action")
		}
		if err := UpsertJob(job); err != nil {
			log.Error().Err(err).Str("job", id).Msg("Failed to persist job")
		}

		// Re-check worker status after dispatch
		if w, ok := s.WorkerMgr.GetWorker(worker.Name); ok && w.Status != shared.WorkerStatusOnline {
			action.Status = shared.ActionStatusReconciling
			action.UpdatedAt = time.Now()
			_ = UpsertAction(action)
			log.Warn().Str("action", actionID).Str("worker", worker.Name).Msg("Worker went offline after dispatch, action reconciling")
		}

		activeCount++
		log.Info().Str("job", id).Str("worker", worker.Name).Str("action", actionID).Msg("Job dispatched")
	}
}

func (s *State) selectWorker(repo *shared.Repo) *shared.Worker {
	online := s.WorkerMgr.GetOnlineWorkers()
	var candidates []*shared.Worker
	for _, w := range online {
		if MatchWorker(w, repo) {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Pick worker with fewest running actions
	best := candidates[0]
	for _, c := range candidates[1:] {
		if len(c.RunningActions) < len(best.RunningActions) {
			best = c
		}
	}
	return best
}

// HandleActionStatus processes action status from WebSocket.
func (s *State) HandleActionStatus(workerName string, msg *shared.WSMessage) {
	s.ActionsMu.Lock()
	action, ok := s.ActiveActions[msg.ActionID]
	if !ok {
		s.ActionsMu.Unlock()
		// Unknown action: might be from a dispatch where Master crashed before recording.
		// Query worker for full details and create record.
		log.Warn().Str("action", msg.ActionID).Str("worker", workerName).Msg("Received status for unknown action")
		// TODO: query worker action_status and create record
		return
	}

	action.Status = msg.Status
	action.ContainerStatus = msg.ContainerStatus
	action.ContainerExitCode = msg.ExitCode
	action.ContainerExitReason = msg.ExitReason
	action.UpdatedAt = msg.UpdatedAt

	isTerminal := msg.Status == shared.ActionStatusSucceeded || msg.Status == shared.ActionStatusFailed
	if isTerminal {
		action.FinishedAt = msg.UpdatedAt
		delete(s.ActiveActions, msg.ActionID)
	}
	s.ActionsMu.Unlock()

	if err := UpsertAction(action); err != nil {
		log.Error().Err(err).Str("action", msg.ActionID).Msg("Failed to persist action status")
	}

	if isTerminal {
		s.finishJob(action.JobID, msg.Status == shared.ActionStatusSucceeded)
		// Send ack so worker can delete container
		s.WSHub.SendAck(workerName, msg.ActionID)
	}
}

func (s *State) finishJob(jobID string, succeeded bool) {
	now := time.Now()
	s.JobsMu.Lock()
	job, ok := s.Jobs[jobID]
	if !ok {
		s.JobsMu.Unlock()
		return
	}

	if succeeded {
		job.LastSuccessAt = now
		job.LastActionStatus = shared.ActionStatusSucceeded
	} else {
		job.LastFailureAt = now
		job.LastActionStatus = shared.ActionStatusFailed
	}

	s.ReposMu.RLock()
	repo, repoOK := s.Repos[jobID]
	s.ReposMu.RUnlock()

	if repoOK {
		interval := ParseInterval(repo.SyncParams.Interval)
		job.NextAttemptAt = now.Add(interval)
		job.Status = shared.JobStatusWaiting
	} else {
		job.Status = shared.JobStatusOrphan
	}
	job.UpdatedAt = now
	s.JobsMu.Unlock()

	if err := UpsertJob(job); err != nil {
		log.Error().Err(err).Str("job", jobID).Msg("Failed to persist job finish")
	}

	go func() {
		_ = s.UpdateMirrorgoJSON()
		_ = s.UpdateMirrorZJSON()
	}()
}

func ParseInterval(ic shared.IntervalConfig) time.Duration {
	if ic.Value == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(ic.Value)
	if err != nil {
		log.Warn().Err(err).Str("value", ic.Value).Msg("Invalid interval; defaulting to 1h")
		return time.Hour
	}
	return d
}

// OnWorkerOffline is called when a worker goes offline.
// Marks all running actions on that worker as Reconciling.
func (s *State) OnWorkerOffline(workerName string) {
	s.ActionsMu.Lock()
	for _, action := range s.ActiveActions {
		if action.WorkerName == workerName && action.Status == shared.ActionStatusRunning {
			action.Status = shared.ActionStatusReconciling
			action.UpdatedAt = time.Now()
			_ = UpsertAction(action)
			log.Warn().Str("action", action.ID).Str("worker", workerName).Msg("Action moved to Reconciling (worker offline)")
		}
	}
	s.ActionsMu.Unlock()
}

// OnWorkerOnline is called when a worker comes back online.
// Reconciliation happens on first heartbeat (via diffActions in master.go).
func (s *State) OnWorkerOnline(workerName string) {
	log.Info().Str("worker", workerName).Msg("Worker online, awaiting first heartbeat for reconciliation")
}

// ReconcileWorkerActions is called on first heartbeat after worker reconnect.
// Compares master's Reconciling actions for this worker against reported running actions.
func (s *State) ReconcileWorkerActions(workerName string, reportedRunning []string) {
	reported := make(map[string]bool, len(reportedRunning))
	for _, id := range reportedRunning {
		reported[id] = true
	}

	s.ActionsMu.Lock()
	var toFail []*shared.Action
	for _, action := range s.ActiveActions {
		if action.WorkerName != workerName {
			continue
		}
		if action.Status == shared.ActionStatusReconciling {
			if reported[action.ID] {
				action.Status = shared.ActionStatusRunning
				action.UpdatedAt = time.Now()
				_ = UpsertAction(action)
				log.Info().Str("action", action.ID).Msg("Reconciling -> Running (worker reported)")
			} else {
				toFail = append(toFail, action)
			}
		}
	}
	// Remove failed from active
	for _, action := range toFail {
		action.Status = shared.ActionStatusFailed
		action.UpdatedAt = time.Now()
		action.FinishedAt = time.Now()
		action.ContainerExitReason = "container lost after worker reconnect"
		delete(s.ActiveActions, action.ID)
		_ = UpsertAction(action)
		log.Warn().Str("action", action.ID).Msg("Reconciling -> Failed (not reported by worker)")
	}
	s.ActionsMu.Unlock()

	for _, action := range toFail {
		s.finishJob(action.JobID, false)
	}
}

// GetActionByIDFromActiveOrDB checks in-memory first, then DB.
func (s *State) GetActionByIDFromActiveOrDB(id string) *shared.Action {
	s.ActionsMu.RLock()
	if a, ok := s.ActiveActions[id]; ok {
		s.ActionsMu.RUnlock()
		return a
	}
	s.ActionsMu.RUnlock()
	a, err := GetActionByID(id)
	if err != nil {
		return nil
	}
	return a
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add master/scheduler.go
git -c commit.gpgsign=false commit -m "feat: add affinity-aware scheduler with remote dispatch and reconciliation"
```

---

### Task 9: Create master/mirrorz.go

Move MirrorZ generation to master package.

**Files:**
- Create: `master/mirrorz.go`

- [ ] **Step 1: Create `master/mirrorz.go`**

Same logic as current `mirrorz.go`, but adapted to use `State` receiver and `shared` types. Replace global `Repos`/`Jobs` access with `s.Repos`/`s.Jobs` and their mutexes. The `GenerateMirrorZ`, `WriteMirrorZJSON`, `UpdateMirrorZJSON` functions become methods on `*State`. Also move `UpdateMirrorgoJSON`, `getMirrorStatus`, `writeMirrorgoJSON` from `web.go` here.

Key changes:
- `package master` instead of `package main`
- All functions become methods on `*State`
- Use `shared.Repo`, `shared.Job`, `shared.Action`, etc.
- Use `s.GetActionByIDFromActiveOrDB(id)` instead of `GetActionByID(id)`

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add master/mirrorz.go
git -c commit.gpgsign=false commit -m "refactor: move MirrorZ generation to master package"
```

---

### Task 10: Create master/web.go

Public API + log proxy to Workers. Adapted from current `web.go`.

**Files:**
- Create: `master/web.go`

- [ ] **Step 1: Create `master/web.go`**

Adapt current `web.go` handlers to use `State` receiver and `shared` types. Key changes:

- All handlers become methods on `*State`
- Use `s.Repos`, `s.Jobs`, `s.ActiveActions`, `s.JobQueue`, `s.WorkerMgr`, `s.WSHub`
- Log endpoints (`/api/logs/*`) become proxies: look up `action.WorkerName`, get worker addr from WorkerMgr, proxy the request to `worker_addr/api/logs/*`
- Internal endpoints registered under `/api/internal/`:
  - `POST /api/internal/register` -> `s.WorkerMgr.Register`
  - `POST /api/internal/heartbeat` -> `s.WorkerMgr.Heartbeat`
  - `GET /api/internal/ws` -> `s.WSHub.HandleWS`
- New endpoint: `GET /api/workers` -> list workers
- New endpoint: `POST /api/workers/remove?name=` -> remove worker
- Auth middleware wraps the entire mux: PSK on `/api/internal/*`, pass-through on everything else

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add master/web.go
git -c commit.gpgsign=false commit -m "feat: add master web server with log proxy and worker management API"
```

---

### Task 11: Create master/master.go

Master startup entry: init DB, load configs, recover state, start loops, start web server.

**Files:**
- Create: `master/master.go`

- [ ] **Step 1: Create `master/master.go`**

```go
// master/master.go
package master

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

type MasterConfig struct {
	Addr      string
	DBPath    string
	AuthToken string
	ConfigDir string
	BaseDir   string
}

func Run(cfg MasterConfig) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	// Init DB
	if err := InitDB(cfg.DBPath); err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize DB")
	}

	state := &State{
		Repos:         make(map[string]shared.Repo),
		Jobs:          make(map[string]*shared.Job),
		ActiveActions: make(map[string]*shared.Action),
		JobQueue:      NewQueue(),
		WorkerMgr:     NewWorkerManager(cfg.AuthToken),
		WSHub:         NewWSHub(cfg.AuthToken),
		Token:         cfg.AuthToken,
	}

	// Wire callbacks
	state.WorkerMgr.SetCallbacks(state.OnWorkerOnline, state.OnWorkerOffline)
	state.WSHub.OnActionStatus = state.HandleActionStatus

	// 1. Load repo configs
	if err := loadReposFromConfigs(cfg.ConfigDir, state); err != nil {
		log.Fatal().Err(err).Msg("Failed to load repo configs")
	}
	log.Info().Int("repos", len(state.Repos)).Msg("Loaded repo configs")

	// 2. Load state from DB
	if jobs, err := LoadJobsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load jobs")
	} else {
		state.Jobs = jobs
	}

	if actions, err := LoadActiveActionsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load active actions")
	} else {
		state.ActiveActions = actions
	}

	if items, err := LoadQueueItemsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load queue")
	} else if len(items) > 0 {
		state.JobQueue.ReplaceAll(items)
	}

	if paused, maxC, err := DBGetQueueState(); err != nil {
		log.Error().Err(err).Msg("Failed to load queue state")
	} else {
		state.JobQueue.SetPaused(paused)
		state.JobQueue.SetMaxConcurrency(maxC)
	}

	if err := state.WorkerMgr.LoadFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load workers")
	}

	// 3. Recovery: mark Running actions Reconciling, Scheduled jobs Waiting, Workers Offline
	if err := MarkAllRunningActionsReconciling(); err != nil {
		log.Error().Err(err).Msg("Failed to mark actions reconciling")
	}
	// Update in-memory
	state.ActionsMu.Lock()
	for _, a := range state.ActiveActions {
		if a.Status == shared.ActionStatusRunning {
			a.Status = shared.ActionStatusReconciling
		}
	}
	state.ActionsMu.Unlock()

	if err := RevertScheduledJobsToWaiting(); err != nil {
		log.Error().Err(err).Msg("Failed to revert scheduled jobs")
	}
	state.JobsMu.Lock()
	for _, j := range state.Jobs {
		if j.Status == shared.JobStatusScheduled {
			j.Status = shared.JobStatusWaiting
			j.NextAttemptAt = time.Now()
		}
	}
	state.JobsMu.Unlock()

	if err := MarkAllWorkersOffline(); err != nil {
		log.Error().Err(err).Msg("Failed to mark workers offline")
	}

	// 4. Migrate jobs
	migrateJobs(state)

	// 5. Start loops
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go state.ScheduleLoop(ctx)
	go state.DispatchLoop(ctx)
	go state.WorkerMgr.OfflineCheckLoop(ctx)
	go state.StartWebServer(cfg.Addr, cfg.AuthToken)

	log.Info().Str("addr", cfg.Addr).Msg("Master started")

	_ = state.UpdateMirrorgoJSON()

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Info().Msg("Shutting down master")
	cancel()
	flushAllState(state)
}

func loadReposFromConfigs(dir string, state *State) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var repo shared.Repo
		if err := json.Unmarshal(b, &repo); err != nil {
			return err
		}
		state.ReposMu.Lock()
		state.Repos[repo.RepoID] = repo
		state.ReposMu.Unlock()
		log.Info().Str("repo", repo.RepoID).Msg("Loaded repo config")
	}
	return nil
}

func migrateJobs(state *State) {
	orphaned := 0
	state.JobsMu.Lock()
	for id, job := range state.Jobs {
		state.ReposMu.RLock()
		_, ok := state.Repos[id]
		state.ReposMu.RUnlock()
		if !ok {
			job.Status = shared.JobStatusOrphan
			job.UpdatedAt = time.Now()
			orphaned++
		}
	}
	state.JobsMu.Unlock()

	created := 0
	state.ReposMu.RLock()
	for id, repo := range state.Repos {
		if strings.ToLower(strings.TrimSpace(repo.SyncParams.Interval.Type)) != "free" {
			continue
		}
		state.JobsMu.Lock()
		job, ok := state.Jobs[id]
		if !ok {
			job = &shared.Job{RepoID: id}
		}
		if job.Status == shared.JobStatusOrphan || job.Status == "" {
			job.Status = shared.JobStatusWaiting
			job.NextAttemptAt = time.Now()
			job.UpdatedAt = time.Now()
			created++
		}
		state.Jobs[id] = job
		state.JobsMu.Unlock()
	}
	state.ReposMu.RUnlock()

	log.Info().Int("orphaned", orphaned).Int("enabled", created).Msg("Migration completed")
}

func flushAllState(state *State) {
	state.JobsMu.RLock()
	for _, job := range state.Jobs {
		_ = UpsertJob(job)
	}
	state.JobsMu.RUnlock()

	state.ActionsMu.RLock()
	for _, act := range state.ActiveActions {
		_ = UpsertAction(act)
	}
	state.ActionsMu.RUnlock()

	if state.JobQueue != nil {
		_ = DBFlushQueue(state.JobQueue.Snapshot())
	}
	log.Info().Msg("State flush complete")
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./master/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add master/master.go
git -c commit.gpgsign=false commit -m "feat: add master startup with DB init, state recovery, and dispatch loops"
```

---

### Task 12: Create worker/action_cache.go

LRU cache for completed action results.

**Files:**
- Create: `worker/action_cache.go`

- [ ] **Step 1: Create `worker/action_cache.go`**

```go
// worker/action_cache.go
package worker

import (
	"sync"
	"time"

	"github.com/star/mirrorgo/shared"
)

type CachedActionResult struct {
	ActionID   string
	Status     string
	ExitCode   int
	ExitReason string
	StartedAt  time.Time
	FinishedAt time.Time
}

type ActionCache struct {
	mu       sync.Mutex
	items    map[string]*CachedActionResult
	order    []string // oldest first
	maxSize  int
}

func NewActionCache(maxSize int) *ActionCache {
	return &ActionCache{
		items:   make(map[string]*CachedActionResult),
		maxSize: maxSize,
	}
}

func (c *ActionCache) Put(result *CachedActionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.items[result.ActionID]; exists {
		c.items[result.ActionID] = result
		return
	}

	if len(c.order) >= c.maxSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}

	c.items[result.ActionID] = result
	c.order = append(c.order, result.ActionID)
}

func (c *ActionCache) Get(actionID string) (*CachedActionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.items[actionID]
	return r, ok
}

func (c *ActionCache) ToStatusResponse(actionID string) *shared.ActionStatusResponse {
	r, ok := c.Get(actionID)
	if !ok {
		return &shared.ActionStatusResponse{Found: false, ActionID: actionID}
	}
	return &shared.ActionStatusResponse{
		Found:      true,
		ActionID:   r.ActionID,
		Status:     r.Status,
		ExitCode:   r.ExitCode,
		ExitReason: r.ExitReason,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
	}
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./worker/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add worker/action_cache.go
git -c commit.gpgsign=false commit -m "feat: add LRU action result cache for worker"
```

---

### Task 13: Create worker/log.go

Move log directory management to worker package.

**Files:**
- Create: `worker/log.go`

- [ ] **Step 1: Create `worker/log.go`**

Same content as current `log.go`, but `package worker`. Change `LogDir` to be configurable (set from worker startup flags). Keep `CreatLogDir`, `GetLogDir`, `MoveFile`, `copyFile` functions.

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./worker/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add worker/log.go
git -c commit.gpgsign=false commit -m "refactor: move log management to worker package"
```

---

### Task 14: Create worker/docker.go

Docker container management with deferred deletion.

**Files:**
- Create: `worker/docker.go`

- [ ] **Step 1: Create `worker/docker.go`**

Adapt current `docker.go` to `package worker`. Key changes:

- Use `shared.Action`, `shared.Volume`
- `$BASEDIR`/`$REPODIR` substitution uses worker-local config values (passed in at startup)
- `CheckContainer`: do NOT call `DeleteContainer` on exit. Instead, just record exit status. Container deletion happens later via `CleanupAckedContainers`.
- New function `CleanupAckedContainers(ackedIDs map[string]bool)`: deletes containers whose action ID is in the acked set.
- New function `ScanExistingContainers()`: lists Docker containers with `syncing-` prefix, returns their status (running/exited with exit code).

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./worker/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add worker/docker.go
git -c commit.gpgsign=false commit -m "feat: add worker Docker management with deferred container deletion"
```

---

### Task 15: Create worker/ws_client.go

WebSocket client that connects to Master and pushes action status updates.

**Files:**
- Create: `worker/ws_client.go`

- [ ] **Step 1: Create `worker/ws_client.go`**

```go
// worker/ws_client.go
package worker

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

type WSClient struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	masterURL string // ws://master:8080/api/internal/ws?name=xxx&token=xxx
	name      string
	token     string

	// Buffer for messages while disconnected
	bufMu  sync.Mutex
	buffer []*shared.WSMessage

	// Ack callback
	OnAck func(actionID string)
}

func NewWSClient(masterURL, name, token string) *WSClient {
	return &WSClient{
		masterURL: masterURL,
		name:      name,
		token:     token,
	}
}

func (c *WSClient) ConnectLoop() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		err := c.connect()
		if err != nil {
			log.Error().Err(err).Dur("backoff", backoff).Msg("WebSocket connection failed, retrying")
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second // reset on successful connection

		// Flush buffered messages
		c.flushBuffer()

		// Read loop (blocks until disconnect)
		c.readLoop()

		log.Warn().Msg("WebSocket disconnected, reconnecting")
		time.Sleep(time.Second)
	}
}

func (c *WSClient) connect() error {
	url := c.masterURL + "/api/internal/ws?name=" + c.name + "&token=" + c.token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	log.Info().Msg("WebSocket connected to master")
	return nil
}

func (c *WSClient) readLoop() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var ack shared.WSAck
		if err := json.Unmarshal(message, &ack); err != nil {
			continue
		}
		if ack.Type == "ack" && c.OnAck != nil {
			c.OnAck(ack.ActionID)
		}
	}
}

func (c *WSClient) Send(msg *shared.WSMessage) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		// Buffer for later
		c.bufMu.Lock()
		c.buffer = append(c.buffer, msg)
		c.bufMu.Unlock()
		return
	}

	data, _ := json.Marshal(msg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Error().Err(err).Str("action", msg.ActionID).Msg("Failed to send WebSocket message, buffering")
		c.bufMu.Lock()
		c.buffer = append(c.buffer, msg)
		c.bufMu.Unlock()
	}
}

func (c *WSClient) flushBuffer() {
	c.bufMu.Lock()
	buf := c.buffer
	c.buffer = nil
	c.bufMu.Unlock()

	for _, msg := range buf {
		c.Send(msg)
	}
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./worker/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add worker/ws_client.go
git -c commit.gpgsign=false commit -m "feat: add WebSocket client with reconnect backoff and message buffering"
```

---

### Task 16: Create worker/api.go

Worker HTTP API: dispatch endpoint, action status, log endpoints.

**Files:**
- Create: `worker/api.go`

- [ ] **Step 1: Create `worker/api.go`**

Contains:
- `POST /api/internal/dispatch` — accepts dispatch request, starts container, returns OK. Idempotent by action ID.
- `GET /api/internal/action_status?id=xxx` — returns status from LRU cache.
- `/api/logs/list`, `/api/logs/raw`, `/api/logs/stream` — same handlers as current `web.go` log handlers (moved here).

Auth: all endpoints require PSK via `shared.ValidateToken`.

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./worker/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add worker/api.go
git -c commit.gpgsign=false commit -m "feat: add worker HTTP API with dispatch, action status, and log endpoints"
```

---

### Task 17: Create worker/worker.go

Worker startup: register, heartbeat loop, container recovery, action monitoring.

**Files:**
- Create: `worker/worker.go`

- [ ] **Step 1: Create `worker/worker.go`**

Contains:
- `WorkerConfig` struct with all CLI flags
- `Run(cfg WorkerConfig)` function:
  1. Init Docker client
  2. Scan existing `syncing-` containers (recovery)
  3. POST /register to Master
  4. Start WebSocket connection loop (goroutine)
  5. Start heartbeat loop (every 10s, POST /heartbeat)
  6. Start HTTP server for dispatch/logs
  7. Wait for signal
- Heartbeat sends current `running_actions` list (action IDs from in-memory tracking)
- Container monitoring: for each dispatched action, goroutine polls Docker status. On completion, sends WSMessage. On ack from Master, marks container for deletion.
- Periodic cleanup: delete acked containers older than 1 hour.

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build ./worker/`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add worker/worker.go
git -c commit.gpgsign=false commit -m "feat: add worker startup with registration, heartbeat, container monitoring"
```

---

### Task 18: Rewrite main.go with subcommands

Parse `master` / `worker` subcommands, delegate to respective `Run()` functions.

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Rewrite `main.go`**

```go
// main.go
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/star/mirrorgo/master"
	"github.com/star/mirrorgo/worker"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mirrorgo <master|worker> [flags]\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "master":
		cfg := parseMasterFlags(os.Args[2:])
		master.Run(cfg)
	case "worker":
		cfg := parseWorkerFlags(os.Args[2:])
		worker.Run(cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: mirrorgo <master|worker> [flags]\n", os.Args[1])
		os.Exit(1)
	}
}

func parseMasterFlags(args []string) master.MasterConfig {
	cfg := master.MasterConfig{
		Addr:      ":8080",
		DBPath:    "state.db",
		ConfigDir: "Configs",
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr":
			i++; cfg.Addr = args[i]
		case "--db":
			i++; cfg.DBPath = args[i]
		case "--auth-token":
			i++; cfg.AuthToken = args[i]
		case "--configs":
			i++; cfg.ConfigDir = args[i]
		}
	}
	if cfg.AuthToken == "" {
		cfg.AuthToken = os.Getenv("AUTH_TOKEN")
	}
	return cfg
}

func parseWorkerFlags(args []string) worker.WorkerConfig {
	cfg := worker.WorkerConfig{
		Addr:    ":9090",
		BaseDir: "/home/zjusct/mirrorgo",
		RepoDir: "/test1/mirrors/",
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++; cfg.Name = args[i]
		case "--master":
			i++; cfg.MasterURL = args[i]
		case "--auth-token":
			i++; cfg.AuthToken = args[i]
		case "--addr":
			i++; cfg.Addr = args[i]
		case "--labels":
			i++; cfg.Labels = parseLabels(args[i])
		case "--basedir":
			i++; cfg.BaseDir = args[i]
		case "--repodir":
			i++; cfg.RepoDir = args[i]
		}
	}
	if cfg.AuthToken == "" {
		cfg.AuthToken = os.Getenv("AUTH_TOKEN")
	}
	return cfg
}

func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			labels[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return labels
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build .`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add main.go
git -c commit.gpgsign=false commit -m "feat: rewrite main.go with master/worker subcommand routing"
```

---

### Task 19: Delete old root-level files

Remove the old single-node files that have been migrated.

**Files:**
- Delete: `job.go`, `queue.go`, `db.go`, `docker.go`, `log.go`, `web.go`, `mirrorz.go`, `zfs.go`

- [ ] **Step 1: Delete old files**

```bash
git rm job.go queue.go db.go docker.go log.go web.go mirrorz.go zfs.go
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/star/mirrorgo && go build .`
Expected: no errors (main.go only imports master/ and worker/ packages)

- [ ] **Step 3: Commit**

```bash
git -c commit.gpgsign=false commit -m "refactor: remove old single-node source files"
```

---

### Task 20: Update Dockerfile and compose.yml

Update build and deployment config for cluster mode.

**Files:**
- Modify: `Dockerfile`
- Modify: `compose.yml`

- [ ] **Step 1: Update Dockerfile**

Change the build command to build from root (unchanged, `go build -o /out/mirrorgo ./` still works). Expose both 8080 and 9090. Remove `ENTRYPOINT` fixed args (let compose provide the subcommand).

```dockerfile
# ... (build stages unchanged) ...

FROM mirror.star-home.top:4430/library/debian:trixie-slim AS runtime
WORKDIR /
USER 0:0
VOLUME ["/Configs", "/data"]
COPY --from=go-build /out/mirrorgo /mirrorgo
EXPOSE 8080 9090
ENTRYPOINT ["/mirrorgo"]
```

- [ ] **Step 2: Update compose.yml**

Replace with the master/worker service layout from the spec (see Task Dispatch section of spec for the full compose example).

- [ ] **Step 3: Verify docker build**

Run: `cd /Users/star/mirrorgo && docker build -t mirrorgo:test .` (if Docker available)

- [ ] **Step 4: Commit**

```bash
git add Dockerfile compose.yml
git -c commit.gpgsign=false commit -m "feat: update Dockerfile and compose.yml for master/worker deployment"
```

---

### Task 21: Integration smoke test

Verify the entire system works end-to-end.

- [ ] **Step 1: Build binary**

Run: `cd /Users/star/mirrorgo && go build -o mirrorgo .`
Expected: produces `mirrorgo` binary

- [ ] **Step 2: Test help output**

Run: `./mirrorgo` (no args)
Expected: usage message showing `master` and `worker` subcommands

- [ ] **Step 3: Start master in background**

Run: `./mirrorgo master --addr :18080 --db /tmp/test-mirrorgo.db --auth-token testtoken --configs Configs &`
Expected: master starts, logs "Master started"

- [ ] **Step 4: Start worker in background**

Run: `./mirrorgo worker --name test-worker --master http://localhost:18080 --auth-token testtoken --addr :19090 --labels test=true &`
Expected: worker registers, logs "Worker registered", "WebSocket connected"

- [ ] **Step 5: Verify worker registered**

Run: `curl -s http://localhost:18080/api/workers | python3 -m json.tool`
Expected: JSON array with one worker, status "Online"

- [ ] **Step 6: Verify jobs loaded**

Run: `curl -s http://localhost:18080/api/jobs | python3 -m json.tool | head -20`
Expected: JSON array of jobs from Configs/

- [ ] **Step 7: Cleanup**

Kill background processes. Remove `/tmp/test-mirrorgo.db`.

- [ ] **Step 8: Final commit**

```bash
git -c commit.gpgsign=false commit --allow-empty -m "test: verified master-worker cluster startup and registration"
```
