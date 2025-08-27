package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/moby/moby/client"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var jobQueue *Queue

const (
	defaultMaxParallel = 5
)

var dockerClient *client.Client

func main() {

	var err error
	dockerClient, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create docker client")
	}
	defer dockerClient.Close()

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	Repos = map[string]Repo{}
	Jobs = map[string]*Job{}
	ActiveActions = map[string]*Action{}
	jobQueue = NewQueue()

	// Init DB
	if err := initDB("state.db"); err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize SQLite DB")
	}

	// 1. Load repo configs
	if err := loadReposFromConfigs("Configs"); err != nil {
		log.Fatal().Err(err).Msg("Failed to load repo configs")
	}

	log.Info().Msg("Loaded repo configs")

	// 2. Load job and action state from DB
	if err := loadJobsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load jobs from DB")
	}
	if err := loadActiveActionsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load active actions from DB")
	}
	if items, err := loadQueueItemsFromDB(); err != nil {
		log.Error().Err(err).Msg("Failed to load queue from DB")
	} else if len(items) > 0 {
		jobQueue.ReplaceAll(items)
		log.Info().Int("queue_len", len(items)).Msg("Loaded queue from DB")
	}

	// Load queue paused state
	if paused, err := dbGetQueuePaused(); err != nil {
		log.Error().Err(err).Msg("Failed to load queue paused state")
	} else if paused {
		jobQueue.SetPaused(true)
		log.Info().Msg("Queue loaded as paused")
	}

	// 3-4. Init state in-memory and run migrations
	migrateJobs()

	// Reconcile statuses with active actions and queue semantics after migration
	// reconcileAfterLoad()

	log.Info().Msg("Migrated jobs")

	// start running jobs

	// start running jobs
	for id, job := range Jobs {
		if job.Status == JobStatusRunning {
			go runJob(id)
			log.Info().Str("job", id).Msg("recovered job started")
		}
	}

	for _, action := range ActiveActions {
		if action.Status == ActionStatusRunning {
			go RunAction(action)
			log.Info().Str("action", action.ID).Msg("recovered action started")
		}
	}

	// 5. Start scheduling and dispatch loops
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go scheduleLoop(ctx)
	go dispatchLoop(ctx, defaultMaxParallel)
	go startWebServer(":8080")

	// Wait for termination signal and flush state
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Info().Msg("Shutting down: flushing state to DB")
	cancel()
	flushAllState()
}

func loadReposFromConfigs(dir string) error {
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
		var repo Repo
		if err := json.Unmarshal(b, &repo); err != nil {
			return err
		}
		reposMu.Lock()
		Repos[repo.RepoID] = repo
		log.Info().Msgf("Loaded repo: %s", repo.RepoID)
		reposMu.Unlock()
	}
	return nil
}

// // reconcileAfterLoad ensures Jobs have correct status:
// // - If an action for job is active: status Running
// // - Else if job in queue: status Scheduled
// // - Else keep existing (Waiting/Orphan)
// func reconcileAfterLoad() {
// 	// Build set of queued job IDs
// 	queued := map[string]struct{}{}
// 	for _, id := range jobQueue.Snapshot() {
// 		queued[id] = struct{}{}
// 	}
// 	// Build set of jobs that have active actions
// 	activeJobs := map[string]struct{}{}
// 	actionsMu.RLock()
// 	for _, a := range ActiveActions {
// 		if a.Status == ActionStatusRunning {
// 			activeJobs[a.JobID] = struct{}{}
// 		}
// 	}
// 	actionsMu.RUnlock()

// 	jobsMu.Lock()
// 	for id, job := range Jobs {
// 		if _, ok := activeJobs[id]; ok {
// 			job.Status = JobStatusRunning
// 			continue
// 		}
// 		if _, ok := queued[id]; ok {
// 			job.Status = JobStatusScheduled
// 			continue
// 		}
// 	}
// 	jobsMu.Unlock()
// }

// migrateJobs creates or updates Jobs based on Repos
func migrateJobs() {
	// Mark missing jobs as Orphan
	orphaned := 0
	jobsMu.Lock()
	for id, job := range Jobs {
		reposMu.RLock()
		_, ok := Repos[id]
		reposMu.RUnlock()
		if !ok {
			job.Status = JobStatusOrphan
			job.UpdatedAt = time.Now()
			Jobs[id] = job
			orphaned++
		}
	}
	jobsMu.Unlock()

	// Create/enable jobs for repos
	createdOrEnabled := 0
	reposMu.RLock()
	for id := range Repos {
		repo := Repos[id]
		if strings.ToLower(strings.TrimSpace(repo.SyncParams.Interval.Type)) != "free" {
			log.Info().Str("repo", id).Str("interval_type", repo.SyncParams.Interval.Type).Msg("Skipping job generation for non-free interval")
			continue
		}
		jobsMu.Lock()
		job, ok := Jobs[id]
		if !ok {
			job = &Job{RepoID: id}
		}
		if job.Status == JobStatusOrphan || job.Status == "" {
			job.Status = JobStatusWaiting
			job.NextAttemptAt = time.Now()
			job.UpdatedAt = time.Now()
			createdOrEnabled++
		}
		Jobs[id] = job
		jobsMu.Unlock()
	}
	reposMu.RUnlock()

	log.Info().Int("orphaned", orphaned).Int("enabled", createdOrEnabled).Msg("Migration completed")
}

func scheduleLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			scheduled := 0
			jobsMu.Lock()
			for id, job := range Jobs {
				if job.Status == JobStatusWaiting && !job.NextAttemptAt.After(now) {
					job.Status = JobStatusScheduled
					job.UpdatedAt = now
					jobQueue.Enqueue(id)
					_ = dbEnqueue(id)
					scheduled++
				}
			}
			jobsMu.Unlock()
			if scheduled > 0 {
				log.Debug().Int("scheduled", scheduled).Int("queue_len", jobQueue.Len()).Msg("Jobs scheduled")
			}
		}
	}
}

func dispatchLoop(ctx context.Context, maxParallel int) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Check active count
			actionsMu.RLock()
			activeCount := len(ActiveActions)
			actionsMu.RUnlock()
			if activeCount >= maxParallel {
				// log.Debug().Int("active", activeCount).Int("max", maxParallel).Msg("Concurrency limit reached")
				continue
			}
			// Pull from queue
			id, ok := jobQueue.Dequeue()
			if !ok {
				// log.Debug().Msg("Queue empty, nothing to dispatch")
				continue
			}
			_ = dbDequeueOne(id)
			// Start action
			log.Info().Str("job", id).Msg("Dispatching job")
			jobsMu.Lock()
			if Jobs[id].Status != JobStatusScheduled {
				jobsMu.Unlock()
				continue
			}
			jobsMu.Unlock()
			go runJob(id)
		}
	}
}

// A Action Have Following States:
// - ContainerCreating
// - ContainerRunning
// - ContainerExited
// - ContainerFailed (exit code != 0)

func runJob(jobID string) {
	// Set Running
	jobsMu.Lock()
	job, ok := Jobs[jobID]
	if !ok {
		jobsMu.Unlock()
		log.Error().Str("job", jobID).Msg("Job not found")
		return
	}
	jobsMu.Unlock()

	LastactionID := ""
	if len(job.Actions) > 0 {
		LastactionID = job.Actions[len(job.Actions)-1]
	}

	log.Info().Str("job", jobID).Str("last_action", LastactionID).Str("status", job.Status).Msg("runJob")

job_polling:
	switch job.Status {
	case JobStatusScheduled:
		job.Status = JobStatusRunning
		job.LastAttemptAt = time.Now()
		job.UpdatedAt = time.Now()
		// Persist job update
		if err := upsertJob(job); err != nil {
			log.Error().Err(err).Str("job", jobID).Msg("Failed to persist job start")
		}

		log.Info().Str("job", jobID).Msg("Job started")

		repo, ok := Repos[jobID]
		if !ok {
			finishJob(job, false)
			return
		}

		// start Action

		actionID := strconv.FormatInt(time.Now().UnixNano(), 10)
		a := &Action{
			ID:              actionID,
			UpdatedAt:       time.Now(),
			JobID:           job.RepoID,
			Status:          ActionStatusRunning,
			ContainerID:     repo.RepoID + "-" + actionID,
			ContainerName:   "syncing-" + job.RepoID,
			ContainerImage:  repo.SyncParams.Image,
			ContainerStatus: ContainerStatusStarting,

			ContainerVolumes: repo.SyncParams.Volumes,
			ContainerEnv:     repo.SyncParams.Environments,
			ContainerCommand: repo.SyncParams.Command,
			ContainerTimeout: repo.SyncParams.Timeout,

			CreatedAt: time.Now(),
			StartedAt: time.Now(),
		}

		actionsMu.Lock()
		ActiveActions[actionID] = a
		actionsMu.Unlock()

		job.Actions = append(job.Actions, actionID)
		upsertJob(job)

		LastactionID = actionID
		RunAction(a)

	case JobStatusRunning:
		if len(job.Actions) == 0 {
			finishJob(job, false)
			log.Error().Str("job", jobID).Msg("Job has no actions, finishing")
			return
		}

		last := GetActionByID(LastactionID)

		if last == nil {
			finishJob(job, false)
			log.Error().Str("job", jobID).Msg("Job has no actions, finishing")
			return
		}

		if last.Status != ActionStatusRunning {
			finishJob(job, last.Status == ActionStatusSucceeded)
			log.Info().Str("job", jobID).Str("action", LastactionID).Msg("Job finished")
			return
		}

	case JobStatusWaiting:
		log.Error().Str("job", jobID).Msg("Job is waiting, ignoring run request")
		return
	case JobStatusOrphan:
		log.Error().Str("job", jobID).Msg("Job is orphan, ignoring run request")
		return
	}

	time.Sleep(1 * time.Second)
	goto job_polling
}

func finishJob(job *Job, succeeded bool) {
	now := time.Now()

	if succeeded {
		job.LastSuccessAt = now
	} else {
		job.LastFailureAt = now
	}
	// Compute next attempt from repo interval
	reposMu.RLock()
	repo := Repos[job.RepoID]
	reposMu.RUnlock()
	interval := parseInterval(repo.SyncParams.Interval)
	job.NextAttemptAt = now.Add(interval)
	job.Status = JobStatusWaiting
	job.UpdatedAt = now

	// Persist job update
	if err := upsertJob(job); err != nil {
		log.Error().Err(err).Str("job", job.RepoID).Msg("Failed to persist job finish")
	}

	if succeeded {
		log.Info().Str("job", job.RepoID).Dur("interval", interval).Time("next_attempt", job.NextAttemptAt).Msg("Job succeeded")
	} else {
		log.Warn().Str("job", job.RepoID).Dur("interval", interval).Time("next_attempt", job.NextAttemptAt).Msg("Job failed")
	}
}

func parseInterval(ic IntervalConfig) time.Duration {
	if strings.TrimSpace(ic.Value) == "" {
		log.Warn().Msg("Empty interval value; defaulting to 1h")
		return time.Hour
	}
	d, err := time.ParseDuration(ic.Value)
	if err != nil {
		log.Warn().Err(err).Str("value", ic.Value).Msg("Invalid interval; defaulting to 1h")
		return time.Hour
	}
	return d
}

// flushAllState persists current in-memory Jobs and ActiveActions before exit
func flushAllState() {
	// Persist jobs
	jobsMu.RLock()
	for _, job := range Jobs {
		if err := upsertJob(job); err != nil {
			log.Error().Err(err).Str("job", job.RepoID).Msg("Failed to flush job")
		}
	}
	jobsMu.RUnlock()

	// Persist actions
	actionsMu.RLock()
	for _, act := range ActiveActions {
		if err := upsertAction(act); err != nil {
			log.Error().Err(err).Str("action", act.ID).Msg("Failed to flush action")
		}
	}
	actionsMu.RUnlock()

	// Persist current queue snapshot
	if jobQueue != nil {
		items := jobQueue.Snapshot()
		// Clear and re-insert queue items
		_ = gormDB.Exec("DELETE FROM queue").Error
		for _, id := range items {
			_ = dbEnqueue(id)
		}
		log.Info().Int("queue_len", len(items)).Msg("Flushed queue")
	}
	log.Info().Msg("State flush complete")
}

func RunAction(act *Action) {

	defer func() {
		log.Debug().Str("job", act.JobID).Str("action", act.ID).Msg("Action finished")
	}()

polling:
	switch act.ContainerStatus {
	case ContainerStatusStarting:
		log.Debug().Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Action started")

		if err := upsertAction(act); err != nil {
			log.Error().Err(err).Str("action", act.ID).Msg("Failed to persist action start")
		}

		if err := StartContainer(act); err != nil {
			act.Status = ActionStatusFailed
			act.ContainerStatus = ContainerStatusNotCreated
			act.UpdatedAt = time.Now()
			act.FinishedAt = time.Now()
		} else {
			act.ContainerStatus = ContainerStatusRunning
			act.StartedAt = time.Now()
			act.UpdatedAt = time.Now()
		}

		if err := upsertAction(act); err != nil {
			log.Error().Err(err).Str("action", act.ID).Msg("Failed to persist action start")
		}
	case ContainerStatusRunning:
		fininshed, err := CheckContainer(act)

		if err != nil {
			act.Status = ActionStatusFailed
			act.ContainerStatus = ContainerStatusOrphan
		}
		if fininshed {
			if act.ContainerExitCode == 0 {
				act.Status = ActionStatusSucceeded
			} else {
				act.Status = ActionStatusFailed
			}

			log.Debug().Str("job", act.JobID).Str("action", act.ID).Str("status", act.Status).Msg("Action finished")
			act.FinishedAt = time.Now()
			act.UpdatedAt = time.Now()

			if err := upsertAction(act); err != nil {
				log.Error().Err(err).Str("action", act.ID).Msg("Failed to persist action finish")
			}
		}

	default:
		actionsMu.Lock()
		delete(ActiveActions, act.ID)
		actionsMu.Unlock()
		return

	}

	time.Sleep(1 * time.Second)
	goto polling
}
