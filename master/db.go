package master

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/star/mirrorgo/shared"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var gormDB *gorm.DB

// JSONMap is a map[string]string that serializes as JSON text for GORM.
type JSONMap map[string]string

func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return "null", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (m *JSONMap) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, m)
	case string:
		return json.Unmarshal([]byte(data), m)
	case nil:
		*m = nil
		return nil
	default:
		return fmt.Errorf("unsupported Scan type for JSONMap: %T", src)
	}
}

// GORM models

type JobModel struct {
	RepoID        string    `gorm:"primaryKey;column:repo_id"`
	Status        string    `gorm:"column:status"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	LastSuccessAt time.Time `gorm:"column:last_success_at"`
	LastFailureAt time.Time `gorm:"column:last_failure_at"`
	LastAttemptAt time.Time `gorm:"column:last_attempt_at"`
	NextAttemptAt time.Time `gorm:"column:next_attempt_at"`

	Actions          shared.StringList `gorm:"column:actions;type:TEXT"`
	LastActionStatus string            `gorm:"column:last_action_status"`
}

func (JobModel) TableName() string { return "jobs" }

type ActionModel struct {
	ID                  string            `gorm:"primaryKey;column:id"`
	JobID               string            `gorm:"column:job_id;index"`
	Status              string            `gorm:"column:status"`
	Message             string            `gorm:"column:message"`
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
	WorkerName          string            `gorm:"column:worker_name"`
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

type WorkerModel struct {
	Name           string            `gorm:"primaryKey;column:name"`
	Labels         JSONMap            `gorm:"column:labels;type:TEXT"`
	Vars           JSONMap            `gorm:"column:vars;type:TEXT"`
	Status         string            `gorm:"column:status"`
	LastHeartbeat  time.Time         `gorm:"column:last_heartbeat"`
	RunningActions shared.StringList `gorm:"column:running_actions;type:TEXT"`
	RegisteredAt   time.Time         `gorm:"column:registered_at"`
}

func (WorkerModel) TableName() string { return "workers" }

// InitDB opens SQLite at path, sets WAL/NORMAL pragmas, and auto-migrates all tables.
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

// ---------------------------------------------------------------------------
// Job CRUD
// ---------------------------------------------------------------------------

// LoadJobsFromDB loads all jobs from the database and returns them as a map keyed by RepoID.
func LoadJobsFromDB() (map[string]*shared.Job, error) {
	var rows []JobModel
	if err := gormDB.Find(&rows).Error; err != nil {
		return nil, err
	}
	jobs := make(map[string]*shared.Job, len(rows))
	for _, m := range rows {
		j := &shared.Job{
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
		jobs[j.RepoID] = j
	}
	return jobs, nil
}

// UpsertJob upserts a job record. It does NOT trigger mirrorgo.json/mirrorz.json updates.
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

// DeleteJob removes a job record by repo ID.
func DeleteJob(repoID string) error {
	return gormDB.Where("repo_id = ?", repoID).Delete(&JobModel{}).Error
}

// ---------------------------------------------------------------------------
// Action CRUD
// ---------------------------------------------------------------------------

// actionModelToAction converts an ActionModel to a shared.Action.
func actionModelToAction(m *ActionModel) *shared.Action {
	return &shared.Action{
		ID:                  m.ID,
		UpdatedAt:           m.UpdatedAt,
		JobID:               m.JobID,
		Status:              m.Status,
		Message:             m.Message,
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
		WorkerName:          m.WorkerName,
	}
}

// UpsertAction upserts an action record.
func UpsertAction(a *shared.Action) error {
	m := ActionModel{
		ID:                  a.ID,
		JobID:               a.JobID,
		Status:              a.Status,
		Message:             a.Message,
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
		WorkerName:          a.WorkerName,
	}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

// LoadActiveActionsFromDB loads all Running and Reconciling actions.
func LoadActiveActionsFromDB() (map[string]*shared.Action, error) {
	var rows []ActionModel
	if err := gormDB.Where("status IN ?", []string{shared.ActionStatusRunning, shared.ActionStatusReconciling}).Find(&rows).Error; err != nil {
		return nil, err
	}
	actions := make(map[string]*shared.Action, len(rows))
	for i := range rows {
		a := actionModelToAction(&rows[i])
		actions[a.ID] = a
	}
	return actions, nil
}

// GetActionByID returns a single action by ID from the database.
func GetActionByID(id string) (*shared.Action, error) {
	var m ActionModel
	if err := gormDB.First(&m, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return actionModelToAction(&m), nil
}

// GetActionsByRepo returns the most recent actions for a given repo, limited by limit.
func GetActionsByRepo(repoID string, limit int) ([]shared.Action, error) {
	var rows []ActionModel
	if err := gormDB.Where("job_id = ?", repoID).Order("created_at desc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]shared.Action, len(rows))
	for i := range rows {
		out[i] = *actionModelToAction(&rows[i])
	}
	return out, nil
}

// GetActionsRecent returns the most recent actions across all repos.
func GetActionsRecent(limit int) ([]shared.Action, error) {
	var rows []ActionModel
	if err := gormDB.Order("created_at desc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]shared.Action, len(rows))
	for i := range rows {
		out[i] = *actionModelToAction(&rows[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Queue persistence
// ---------------------------------------------------------------------------

// DBEnqueue adds a job ID to the persistent queue.
func DBEnqueue(jobID string) error {
	return gormDB.Create(&QueueItemModel{JobID: jobID, EnqueuedAt: time.Now()}).Error
}

// DBDequeueOne removes the oldest queue entry for the given job ID.
func DBDequeueOne(jobID string) error {
	var qi QueueItemModel
	tx := gormDB.Where("job_id = ?", jobID).Order("id asc").First(&qi)
	if tx.Error != nil {
		return tx.Error
	}
	return gormDB.Delete(&qi).Error
}

// DBDeleteAllQueueByJob removes all queue entries for a given job ID.
func DBDeleteAllQueueByJob(jobID string) error {
	return gormDB.Where("job_id = ?", jobID).Delete(&QueueItemModel{}).Error
}

// LoadQueueItemsFromDB returns all queued job IDs in order.
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

// DBFlushQueue deletes all queue entries and re-inserts the provided items.
func DBFlushQueue(items []string) error {
	return gormDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("1=1").Delete(&QueueItemModel{}).Error; err != nil {
			return err
		}
		now := time.Now()
		for _, id := range items {
			if err := tx.Create(&QueueItemModel{JobID: id, EnqueuedAt: now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Queue state
// ---------------------------------------------------------------------------

// DBGetQueueState returns the paused flag and max concurrency from the DB.
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

// DBSetQueueState persists the paused flag and max concurrency.
func DBSetQueueState(paused bool, maxConcurrency int) error {
	row := QueueStateModel{ID: 1, Paused: paused, MaxConcurrency: maxConcurrency}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// ---------------------------------------------------------------------------
// Worker CRUD
// ---------------------------------------------------------------------------

// UpsertWorker upserts a worker record.
func UpsertWorker(w *shared.Worker) error {
	m := WorkerModel{
		Name:           w.Name,
		Labels:         JSONMap(w.Labels),
		Vars:           JSONMap(w.Vars),
		Status:         w.Status,
		LastHeartbeat:  w.LastHeartbeat,
		RunningActions: w.RunningActions,
		RegisteredAt:   w.RegisteredAt,
	}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

// LoadWorkersFromDB loads all workers from the database.
func LoadWorkersFromDB() (map[string]*shared.Worker, error) {
	var rows []WorkerModel
	if err := gormDB.Find(&rows).Error; err != nil {
		return nil, err
	}
	workers := make(map[string]*shared.Worker, len(rows))
	for _, m := range rows {
		w := &shared.Worker{
			Name:           m.Name,
			Labels:         map[string]string(m.Labels),
			Vars:           map[string]string(m.Vars),
			Status:         m.Status,
			LastHeartbeat:  m.LastHeartbeat,
			RunningActions: m.RunningActions,
			RegisteredAt:   m.RegisteredAt,
		}
		workers[w.Name] = w
	}
	return workers, nil
}

// DeleteWorker removes a worker by name.
func DeleteWorker(name string) error {
	return gormDB.Where("name = ?", name).Delete(&WorkerModel{}).Error
}

// ---------------------------------------------------------------------------
// Bulk recovery helpers
// ---------------------------------------------------------------------------

// MarkAllRunningActionsReconciling sets all Running actions to Reconciling status.
func MarkAllRunningActionsReconciling() error {
	return gormDB.Model(&ActionModel{}).
		Where("status = ?", shared.ActionStatusRunning).
		Update("status", shared.ActionStatusReconciling).Error
}


// MarkAllWorkersOffline sets all Online workers to Offline.
func MarkAllWorkersOffline() error {
	return gormDB.Model(&WorkerModel{}).
		Where("status = ?", shared.WorkerStatusOnline).
		Update("status", shared.WorkerStatusOffline).Error
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

// RawDB returns the underlying *sql.DB handle.
func RawDB() (*sql.DB, error) {
	return gormDB.DB()
}
