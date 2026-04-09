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

// MasterConfig holds the configuration for the master process.
type MasterConfig struct {
	Addr      string
	DBPath    string
	AuthToken string
	ConfigDir string
	BaseDir   string
	UIFS      fs.FS // embedded UI filesystem, passed from main.go
}

// Run is the main entry point for the master process.
func Run(cfg MasterConfig) {
	// 1. Set up zerolog.
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	// 2. Init DB.
	if err := InitDB(cfg.DBPath); err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize SQLite DB")
	}

	// 3. Create State with all components.
	state := &State{
		Repos:         make(map[string]shared.Repo),
		Jobs:          make(map[string]*shared.Job),
		ActiveActions: make(map[string]*shared.Action),
		JobQueue:      NewQueue(),
		WorkerMgr:     NewWorkerManager(cfg.AuthToken),
		WSHub:         NewWSHub(cfg.AuthToken),
		Token:         cfg.AuthToken,
		BaseDir:       cfg.BaseDir,
		ConfigDir:     cfg.ConfigDir,
		UIFS:          cfg.UIFS,
	}

	// 4. Wire callbacks.
	state.WorkerMgr.SetOfflineCallback(state.OnWorkerOffline)
	state.WorkerMgr.OnHeartbeat = state.HandleHeartbeatDiff
	state.WSHub.OnActionStatus = state.HandleActionStatus
	state.WSHub.OnWorkerWSReady = state.WorkerMgr.MarkOnline
	state.WSHub.OnWorkerWSLost = func(workerName string) {
		state.WorkerMgr.MarkOffline(workerName)
		state.OnWorkerOffline(workerName)
	}

	// 5. Load repo configs from ConfigDir.
	if err := loadReposFromConfigs(cfg.ConfigDir, state); err != nil {
		log.Fatal().Err(err).Msg("Failed to load repo configs")
	}
	log.Info().Msg("Loaded repo configs")

	// 6. Load state from DB.
	if jobs, err := LoadJobsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load jobs from DB")
	} else {
		state.JobsMu.Lock()
		for k, v := range jobs {
			state.Jobs[k] = v
		}
		state.JobsMu.Unlock()
	}

	if actions, err := LoadActiveActionsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load active actions from DB")
	} else {
		state.ActionsMu.Lock()
		for k, v := range actions {
			state.ActiveActions[k] = v
		}
		state.ActionsMu.Unlock()
	}

	if items, err := LoadQueueItemsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load queue from DB")
	} else if len(items) > 0 {
		state.JobQueue.ReplaceAll(items)
		log.Info().Int("queue_len", len(items)).Msg("Loaded queue from DB")
	}

	if paused, maxConcurrency, err := DBGetQueueState(); err != nil {
		log.Error().Err(err).Msg("Failed to load queue state")
	} else {
		state.JobQueue.SetPaused(paused)
		state.JobQueue.SetMaxConcurrency(maxConcurrency)
		log.Info().Bool("paused", paused).Int("max_concurrency", maxConcurrency).Msg("Queue state loaded from DB")
	}

	if err := state.WorkerMgr.LoadFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load workers from DB")
	}

	// 7. Recovery steps.
	if err := MarkAllRunningActionsReconciling(); err != nil {
		log.Error().Err(err).Msg("Failed to mark running actions as reconciling")
	} else {
		// Update in-memory actions to match.
		state.ActionsMu.Lock()
		for _, a := range state.ActiveActions {
			if a.Status == shared.ActionStatusRunning {
				a.Status = shared.ActionStatusReconciling
			}
		}
		state.ActionsMu.Unlock()
	}

	// 7b. Recover Scheduled jobs: the queue was already restored from DB above,
	// so Scheduled jobs should stay Scheduled and dispatchTick will process them.
	// Only re-enqueue Scheduled jobs that are missing from the restored queue
	// (e.g. if DBEnqueue failed before a crash).
	{
		queueSet := make(map[string]struct{})
		for _, id := range state.JobQueue.Snapshot() {
			queueSet[id] = struct{}{}
		}
		state.JobsMu.RLock()
		for _, j := range state.Jobs {
			if j.Status == shared.JobStatusScheduled {
				if _, inQueue := queueSet[j.RepoID]; !inQueue {
					state.JobQueue.Enqueue(j.RepoID)
					_ = DBEnqueue(j.RepoID)
					log.Warn().Str("job", j.RepoID).Msg("Scheduled job missing from queue, re-enqueued")
				}
			}
		}
		state.JobsMu.RUnlock()
	}

	if err := MarkAllWorkersOffline(); err != nil {
		log.Error().Err(err).Msg("Failed to mark all workers offline")
	}
	state.WorkerMgr.MarkAllOffline()

	// 7d. Defensive recovery: revert Running jobs that have no active action.
	// This handles the edge case where master crashed after completing an
	// action but before persisting the job state update in finishJob.
	{
		// Build set of job IDs that have active actions.
		activeJobIDs := make(map[string]struct{})
		state.ActionsMu.RLock()
		for _, a := range state.ActiveActions {
			activeJobIDs[a.JobID] = struct{}{}
		}
		state.ActionsMu.RUnlock()

		now := time.Now()
		state.JobsMu.Lock()
		for _, j := range state.Jobs {
			if j.Status == shared.JobStatusRunning {
				if _, has := activeJobIDs[j.RepoID]; !has {
					log.Warn().Str("job", j.RepoID).Msg("Running job has no active action, reverting to Waiting")
					j.Status = shared.JobStatusWaiting
					j.NextAttemptAt = now
					j.UpdatedAt = now
					_ = UpsertJob(j)
				}
			}
		}
		state.JobsMu.Unlock()
	}

	// 8. Migrate jobs: orphan deleted repos, create jobs for new repos.
	migrateJobs(state)
	log.Info().Msg("Migrated jobs")

	// 9. Start goroutines.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go state.ScheduleLoop(ctx)
	go state.DispatchLoop(ctx)
	go state.StaleReconcilingCheckLoop(ctx)
	go state.StartWebServer(cfg.Addr, cfg.AuthToken)

	// 10. Update mirrorgo.json.
	if err := state.UpdateMirrorgoJSON(); err != nil {
		log.Error().Err(err).Msg("Failed to update mirrorgo.json")
	}
	if err := state.UpdateMirrorZJSON(); err != nil {
		log.Error().Err(err).Msg("Failed to update mirrorz.json")
	}

	// 11. Wait for SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Info().Msg("Shutting down: flushing state to DB")

	// 12. Cancel context and flush all state.
	cancel()
	flushAllState(state)
}

// loadReposFromConfigs reads JSON repo config files from the given directory
// and populates state.Repos.
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
		log.Info().Msgf("Loaded repo: %s", repo.RepoID)
		state.ReposMu.Unlock()
	}
	return nil
}

// migrateJobs creates or updates Jobs based on Repos. It marks jobs whose
// repos no longer exist as Orphan and creates Waiting jobs for new repos.
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
			state.Jobs[id] = job
			orphaned++
		}
	}
	state.JobsMu.Unlock()

	createdOrEnabled := 0
	state.ReposMu.RLock()
	for id := range state.Repos {
		repo := state.Repos[id]
		if strings.ToLower(strings.TrimSpace(repo.SyncParams.Interval.Type)) != "free" {
			log.Info().Str("repo", id).Str("interval_type", repo.SyncParams.Interval.Type).Msg("Skipping job generation for non-free interval")
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
			createdOrEnabled++
		}
		state.Jobs[id] = job
		state.JobsMu.Unlock()
	}
	state.ReposMu.RUnlock()

	log.Info().Int("orphaned", orphaned).Int("enabled", createdOrEnabled).Msg("Migration completed")
}

// flushAllState persists current in-memory Jobs, ActiveActions, and queue
// snapshot to the database before exit.
func flushAllState(state *State) {
	// Persist jobs.
	state.JobsMu.RLock()
	for _, job := range state.Jobs {
		if err := UpsertJob(job); err != nil {
			log.Error().Err(err).Str("job", job.RepoID).Msg("Failed to flush job")
		}
	}
	state.JobsMu.RUnlock()

	// Persist actions.
	state.ActionsMu.RLock()
	for _, act := range state.ActiveActions {
		if err := UpsertAction(act); err != nil {
			log.Error().Err(err).Str("action", act.ID).Msg("Failed to flush action")
		}
	}
	state.ActionsMu.RUnlock()

	// Persist current queue snapshot.
	if state.JobQueue != nil {
		items := state.JobQueue.Snapshot()
		if err := DBFlushQueue(items); err != nil {
			log.Error().Err(err).Msg("Failed to flush queue")
		} else {
			log.Info().Int("queue_len", len(items)).Msg("Flushed queue")
		}
	}

	// Update mirrorgo.json one last time.
	if err := state.UpdateMirrorgoJSON(); err != nil {
		log.Error().Err(err).Msg("Failed to update mirrorgo.json on shutdown")
	}

	log.Info().Msg("State flush complete")
}
