package main

import (
	"database/sql"
	"time"

	"github.com/rs/zerolog/log"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var gormDB *gorm.DB

// GORM models
type JobModel struct {
	RepoID        string    `gorm:"primaryKey;column:repo_id"`
	Status        string    `gorm:"column:status"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	LastSuccessAt time.Time `gorm:"column:last_success_at"`
	LastFailureAt time.Time `gorm:"column:last_failure_at"`
	LastAttemptAt time.Time `gorm:"column:last_attempt_at"`
	NextAttemptAt time.Time `gorm:"column:next_attempt_at"`

	Actions StringList `gorm:"column:actions"`
}

func (JobModel) TableName() string { return "jobs" }

type ActionModel struct {
	ID                  string     `gorm:"primaryKey;column:id"`
	JobID               string     `gorm:"column:job_id;index"`
	Status              string     `gorm:"column:status"`
	Message             string     `gorm:"column:message"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
	ContainerID         string     `gorm:"column:container_id"`
	ContainerName       string     `gorm:"column:container_name"`
	ContainerImage      string     `gorm:"column:container_image"`
	ContainerStatus     string     `gorm:"column:container_status"`
	ContainerExitCode   int        `gorm:"column:container_exit_code"`
	ContainerExitSignal int        `gorm:"column:container_exit_signal"`
	ContainerExitReason string     `gorm:"column:container_exit_reason"`
	ContainerVolumes    VolumeList `gorm:"column:container_volumes;type:TEXT"`
	ContainerEnv        StringList `gorm:"column:container_env;type:TEXT"`
	ContainerCommand    StringList `gorm:"column:container_command;type:TEXT"`
	ContainerTimeout    string     `gorm:"column:container_timeout"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	StartedAt           time.Time  `gorm:"column:started_at"`
	FinishedAt          time.Time  `gorm:"column:finished_at"`
}

func (ActionModel) TableName() string { return "actions" }

// Queue state persistence
type QueueStateModel struct {
	ID             uint `gorm:"primaryKey;column:id"`
	Paused         bool `gorm:"column:paused"`
	MaxConcurrency int  `gorm:"column:max_concurrency;default:1"`
}

func (QueueStateModel) TableName() string { return "queue_state" }

// initDB initializes GORM with SQLite and runs migrations
func initDB(path string) error {
	var err error
	gormDB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return err
	}
	// Set pragmas on the underlying connection for better write performance
	if raw, err := gormDB.DB(); err == nil {
		_, _ = raw.Exec("PRAGMA journal_mode=WAL;")
		_, _ = raw.Exec("PRAGMA synchronous=NORMAL;")
	}
	// Auto-migrate tables
	if err := gormDB.AutoMigrate(&JobModel{}, &ActionModel{}, &QueueItemModel{}, &QueueStateModel{}); err != nil {
		return err
	}
	return nil
}

func loadJobsFromDB() error {
	var rows []JobModel
	if err := gormDB.Find(&rows).Error; err != nil {
		return err
	}
	count := 0
	for _, m := range rows {
		j := Job{
			RepoID:        m.RepoID,
			Status:        m.Status,
			UpdatedAt:     m.UpdatedAt,
			LastSuccessAt: m.LastSuccessAt,
			LastFailureAt: m.LastFailureAt,
			LastAttemptAt: m.LastAttemptAt,
			NextAttemptAt: m.NextAttemptAt,
			Actions:       m.Actions,
		}

		jobsMu.Lock()
		Jobs[j.RepoID] = &j
		jobsMu.Unlock()
		count++
	}
	log.Info().Int("jobs", count).Msg("Loaded jobs from DB")
	return nil
}

func loadActiveActionsFromDB() error {
	var rows []ActionModel
	if err := gormDB.Where("status = ?", ActionStatusRunning).Find(&rows).Error; err != nil {
		return err
	}
	count := 0
	for _, m := range rows {
		a := Action{
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
		}
		actionsMu.Lock()
		ActiveActions[a.ID] = &a
		actionsMu.Unlock()
		count++
	}
	log.Info().Int("actions", count).Msg("Loaded running actions from DB")
	return nil
}

func upsertJob(j *Job) error {
	m := JobModel{
		RepoID:        j.RepoID,
		Status:        j.Status,
		UpdatedAt:     j.UpdatedAt,
		LastSuccessAt: j.LastSuccessAt,
		LastFailureAt: j.LastFailureAt,
		LastAttemptAt: j.LastAttemptAt,
		NextAttemptAt: j.NextAttemptAt,
		Actions:       j.Actions,
	}

	log.Debug().Interface("job", m).Msg("upsertJob")
	err := gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error

	// Update mirrorgo.json on job status changes
	if err == nil {
		// Use goroutine to avoid blocking the main operation
		go func() {
			if updateErr := UpdateMirrorgoJSON(); updateErr != nil {
				log.Error().Err(updateErr).Str("job", j.RepoID).Msg("Failed to update mirrorgo.json")
			}
		}()
	}

	return err
}

func upsertAction(a *Action) error {
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
	}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

// Helper to access underlying *sql.DB if needed
func rawDB() (*sql.DB, error) { return gormDB.DB() }

// Persistent queue table
type QueueItemModel struct {
	ID         uint      `gorm:"primaryKey;column:id"`
	JobID      string    `gorm:"column:job_id;index"`
	EnqueuedAt time.Time `gorm:"column:enqueued_at"`
}

func (QueueItemModel) TableName() string { return "queue" }

// Queue persistence helpers
func dbEnqueue(jobID string) error {
	return gormDB.Create(&QueueItemModel{JobID: jobID, EnqueuedAt: time.Now()}).Error
}

func dbDequeueOne(jobID string) error {
	var qi QueueItemModel
	tx := gormDB.Where("job_id = ?", jobID).Order("id asc").First(&qi)
	if tx.Error != nil {
		return tx.Error
	}
	return gormDB.Delete(&qi).Error
}

func dbDeleteAllQueueByJob(jobID string) error {
	return gormDB.Where("job_id = ?", jobID).Delete(&QueueItemModel{}).Error
}

func loadQueueItemsFromDB() ([]string, error) {
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

// GetActionByID returns a pointer to an Action by ID, checking live map first, then DB.
// If found in DB, a new instance is created and returned; otherwise returns nil.
func GetActionByID(id string) *Action {
	actionsMu.RLock()
	if a, ok := ActiveActions[id]; ok {
		actionsMu.RUnlock()
		return a
	}
	actionsMu.RUnlock()
	var m ActionModel
	if err := gormDB.First(&m, "id = ?", id).Error; err != nil {
		return nil
	}
	a := &Action{
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
	}
	return a
}

// Queue paused state helpers
func dbGetQueuePaused() (bool, error) {
	var row QueueStateModel
	// single row with id=1
	if err := gormDB.First(&row, 1).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, nil
		}
		return false, err
	}
	return row.Paused, nil
}

func dbSetQueuePaused(paused bool) error {
	row := QueueStateModel{ID: 1, Paused: paused}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func dbGetQueueMaxConcurrency() (int, error) {
	var row QueueStateModel
	// single row with id=1
	if err := gormDB.First(&row, 1).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return 1, nil // 默认值为1
		}
		return 1, err
	}
	return row.MaxConcurrency, nil
}

func dbSetQueueMaxConcurrency(maxConcurrency int) error {
	row := QueueStateModel{ID: 1, MaxConcurrency: maxConcurrency}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func dbGetQueueState() (bool, int, error) {
	var row QueueStateModel
	// single row with id=1
	if err := gormDB.First(&row, 1).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, 1, nil // 默认值：未暂停，最大并发数为1
		}
		return false, 1, err
	}
	return row.Paused, row.MaxConcurrency, nil
}

func dbSetQueueState(paused bool, maxConcurrency int) error {
	row := QueueStateModel{ID: 1, Paused: paused, MaxConcurrency: maxConcurrency}
	return gormDB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}
