package master

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// State is the central state holder for the master process.
type State struct {
	Repos   map[string]shared.Repo
	ReposMu sync.RWMutex

	Jobs   map[string]*shared.Job
	JobsMu sync.RWMutex

	ActiveActions map[string]*shared.Action
	ActionsMu     sync.RWMutex

	JobQueue  *Queue
	WorkerMgr *WorkerManager
	WSHub     *WSHub
	Token     string
	BaseDir   string // for mirrorgo.json path
}

// ScheduleLoop checks Waiting jobs every 5s and enqueues those whose
// NextAttemptAt has passed.
func (s *State) ScheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scheduleTick()
		}
	}
}

func (s *State) scheduleTick() {
	now := time.Now()
	s.JobsMu.Lock()
	defer s.JobsMu.Unlock()

	for _, job := range s.Jobs {
		if job.Status != shared.JobStatusWaiting {
			continue
		}
		if job.NextAttemptAt.After(now) {
			continue
		}
		job.Status = shared.JobStatusScheduled
		job.UpdatedAt = now
		if err := UpsertJob(job); err != nil {
			log.Error().Err(err).Str("job", job.RepoID).Msg("failed to persist scheduled job")
		}
		s.JobQueue.Enqueue(job.RepoID)
		if err := DBEnqueue(job.RepoID); err != nil {
			log.Error().Err(err).Str("job", job.RepoID).Msg("failed to persist queue enqueue")
		}
		log.Info().Str("job", job.RepoID).Msg("job scheduled and enqueued")
	}
}

// DispatchLoop runs dispatchTick every 1s.
func (s *State) DispatchLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dispatchTick()
		}
	}
}

func (s *State) dispatchTick() {
	// Check concurrency limit.
	maxConc := s.JobQueue.GetMaxConcurrency()
	s.ActionsMu.RLock()
	runningCount := len(s.ActiveActions)
	s.ActionsMu.RUnlock()
	if runningCount >= maxConc {
		return
	}

	queueLen := s.JobQueue.Len()
	if queueLen == 0 {
		return
	}

	requeued := 0
	for requeued < queueLen {
		jobID, ok := s.JobQueue.Dequeue()
		if !ok {
			break
		}

		// Check the job is still Scheduled.
		s.JobsMu.RLock()
		job, exists := s.Jobs[jobID]
		s.JobsMu.RUnlock()
		if !exists || job.Status != shared.JobStatusScheduled {
			_ = DBDequeueOne(jobID)
			continue
		}

		// Check repo exists.
		s.ReposMu.RLock()
		repo, repoExists := s.Repos[jobID]
		s.ReposMu.RUnlock()
		if !repoExists {
			log.Warn().Str("job", jobID).Msg("repo not found, dropping from queue")
			_ = DBDequeueOne(jobID)
			continue
		}

		// Find a matching online worker.
		worker := s.selectWorker(&repo)
		if worker == nil {
			// No matching worker available — requeue to tail.
			s.JobQueue.Enqueue(jobID)
			requeued++
			continue
		}

		// Build dispatch request.
		actionID := strconv.FormatInt(time.Now().UnixNano(), 10)
		dispReq := &shared.DispatchRequest{
			Action: shared.DispatchAction{
				ID:               actionID,
				JobID:            jobID,
				ContainerImage:   repo.SyncParams.Image,
				ContainerCommand: repo.SyncParams.Command,
				ContainerVolumes: repo.SyncParams.Volumes,
				ContainerEnv:     repo.SyncParams.Environments,
				ContainerTimeout: repo.SyncParams.Timeout,
			},
		}

		err := DispatchToWorker(worker, dispReq, s.Token)
		if err != nil {
			log.Error().Err(err).Str("job", jobID).Str("worker", worker.Name).Msg("dispatch failed, requeuing")
			s.JobQueue.Enqueue(jobID)
			requeued++
			continue
		}

		// Dispatch succeeded — remove from persistent queue.
		_ = DBDequeueOne(jobID)

		now := time.Now()
		action := &shared.Action{
			ID:               actionID,
			JobID:            jobID,
			Status:           shared.ActionStatusRunning,
			ContainerImage:   repo.SyncParams.Image,
			ContainerCommand: repo.SyncParams.Command,
			ContainerVolumes: repo.SyncParams.Volumes,
			ContainerEnv:     repo.SyncParams.Environments,
			ContainerTimeout: repo.SyncParams.Timeout,
			WorkerName:       worker.Name,
			CreatedAt:        now,
			StartedAt:        now,
			UpdatedAt:        now,
		}

		s.ActionsMu.Lock()
		s.ActiveActions[actionID] = action
		s.ActionsMu.Unlock()

		if err := UpsertAction(action); err != nil {
			log.Error().Err(err).Str("action", actionID).Msg("failed to persist action")
		}

		// Update job to Running.
		s.JobsMu.Lock()
		job.Status = shared.JobStatusRunning
		job.LastAttemptAt = now
		job.UpdatedAt = now
		job.Actions = append(job.Actions, actionID)
		// Keep only last 100 action IDs.
		if len(job.Actions) > 100 {
			job.Actions = job.Actions[len(job.Actions)-100:]
		}
		s.JobsMu.Unlock()

		if err := UpsertJob(job); err != nil {
			log.Error().Err(err).Str("job", jobID).Msg("failed to persist running job")
		}

		// Re-check that the worker is still online; if not, mark action as Reconciling.
		w, wOK := s.WorkerMgr.GetWorker(worker.Name)
		if !wOK || w.Status != shared.WorkerStatusOnline {
			log.Warn().Str("action", actionID).Str("worker", worker.Name).Msg("worker went offline after dispatch, marking Reconciling")
			s.ActionsMu.Lock()
			action.Status = shared.ActionStatusReconciling
			action.UpdatedAt = time.Now()
			s.ActionsMu.Unlock()
			_ = UpsertAction(action)
		}

		log.Info().Str("job", jobID).Str("action", actionID).Str("worker", worker.Name).Msg("dispatched action")

		// Re-check concurrency limit.
		s.ActionsMu.RLock()
		runningCount = len(s.ActiveActions)
		s.ActionsMu.RUnlock()
		if runningCount >= maxConc {
			break
		}
	}
}

// selectWorker picks the online worker matching the repo's affinity that has
// the fewest running actions.
func (s *State) selectWorker(repo *shared.Repo) *shared.Worker {
	online := s.WorkerMgr.GetOnlineWorkers()
	var best *shared.Worker
	bestLoad := -1
	for _, w := range online {
		if !MatchWorker(w, repo) {
			continue
		}
		load := len(w.RunningActions)
		if best == nil || load < bestLoad {
			best = w
			bestLoad = load
		}
	}
	return best
}

// HandleActionStatus is called by WSHub when a worker reports action status.
func (s *State) HandleActionStatus(workerName string, msg *shared.WSMessage) {
	s.ActionsMu.Lock()
	action, exists := s.ActiveActions[msg.ActionID]
	if !exists {
		s.ActionsMu.Unlock()
		log.Warn().Str("action", msg.ActionID).Str("worker", workerName).Msg("received status for unknown action (possibly from crash recovery)")
		return
	}

	// Update action fields from message.
	action.Status = msg.Status
	action.ContainerStatus = msg.ContainerStatus
	action.ContainerExitCode = msg.ExitCode
	action.ContainerExitReason = msg.ExitReason
	action.UpdatedAt = msg.UpdatedAt

	isTerminal := msg.Status == shared.ActionStatusSucceeded || msg.Status == shared.ActionStatusFailed
	if isTerminal {
		action.FinishedAt = time.Now()
		delete(s.ActiveActions, msg.ActionID)
	}
	s.ActionsMu.Unlock()

	if err := UpsertAction(action); err != nil {
		log.Error().Err(err).Str("action", msg.ActionID).Msg("failed to persist action status update")
	}

	if isTerminal {
		s.finishJob(action.JobID, msg.Status == shared.ActionStatusSucceeded)
		s.WSHub.SendAck(workerName, msg.ActionID)
	}
}

// finishJob updates the job after an action completes and schedules the next attempt.
func (s *State) finishJob(jobID string, succeeded bool) {
	now := time.Now()

	s.JobsMu.Lock()
	job, exists := s.Jobs[jobID]
	if !exists {
		s.JobsMu.Unlock()
		log.Error().Str("job", jobID).Msg("finishJob: job not found")
		return
	}

	if succeeded {
		job.LastSuccessAt = now
		job.LastActionStatus = shared.ActionStatusSucceeded
	} else {
		job.LastFailureAt = now
		job.LastActionStatus = shared.ActionStatusFailed
	}

	// Compute next attempt from repo interval.
	s.ReposMu.RLock()
	repo, repoExists := s.Repos[jobID]
	s.ReposMu.RUnlock()

	interval := time.Hour // default fallback
	if repoExists {
		interval = ParseInterval(repo.SyncParams.Interval)
	}

	job.NextAttemptAt = now.Add(interval)
	job.Status = shared.JobStatusWaiting
	job.UpdatedAt = now
	s.JobsMu.Unlock()

	if err := UpsertJob(job); err != nil {
		log.Error().Err(err).Str("job", jobID).Msg("failed to persist job finish")
	}

	if succeeded {
		log.Info().Str("job", jobID).Dur("interval", interval).Time("next_attempt", job.NextAttemptAt).Msg("job succeeded")
	} else {
		log.Warn().Str("job", jobID).Dur("interval", interval).Time("next_attempt", job.NextAttemptAt).Msg("job failed")
	}

	// Trigger mirrorgo.json/mirrorz.json update in background.
	go func() {
		if err := s.UpdateMirrorgoJSON(); err != nil {
			log.Error().Err(err).Msg("failed to update mirrorgo.json")
		}
		if err := s.UpdateMirrorZJSON(); err != nil {
			log.Error().Err(err).Msg("failed to update mirrorz.json")
		}
	}()
}

// ParseInterval parses an IntervalConfig into a time.Duration.
func ParseInterval(ic shared.IntervalConfig) time.Duration {
	if strings.TrimSpace(ic.Value) == "" {
		log.Warn().Msg("empty interval value; defaulting to 1h")
		return time.Hour
	}
	d, err := time.ParseDuration(ic.Value)
	if err != nil {
		log.Warn().Err(err).Str("value", ic.Value).Msg("invalid interval; defaulting to 1h")
		return time.Hour
	}
	return d
}

// ---------------------------------------------------------------------------
// Worker online/offline handlers
// ---------------------------------------------------------------------------

// OnWorkerOffline marks all Running actions on the given worker as Reconciling.
func (s *State) OnWorkerOffline(workerName string) {
	s.ActionsMu.Lock()
	defer s.ActionsMu.Unlock()
	for _, action := range s.ActiveActions {
		if action.WorkerName == workerName && action.Status == shared.ActionStatusRunning {
			action.Status = shared.ActionStatusReconciling
			action.UpdatedAt = time.Now()
			if err := UpsertAction(action); err != nil {
				log.Error().Err(err).Str("action", action.ID).Msg("failed to persist reconciling status")
			}
			log.Warn().Str("action", action.ID).Str("worker", workerName).Msg("action marked Reconciling (worker offline)")
		}
	}
}

// OnWorkerOnline logs the event. Actual reconciliation happens on first heartbeat.
func (s *State) OnWorkerOnline(workerName string) {
	log.Info().Str("worker", workerName).Msg("worker came online")
}

// ReconcileWorkerActions compares Reconciling actions for a worker against the
// reported running list. Actions reported as running are restored to Running;
// actions not reported are marked Failed and their jobs are finished.
func (s *State) ReconcileWorkerActions(workerName string, reportedRunning []string) {
	reportedSet := make(map[string]struct{}, len(reportedRunning))
	for _, id := range reportedRunning {
		reportedSet[id] = struct{}{}
	}

	s.ActionsMu.Lock()
	var toFail []*shared.Action
	for _, action := range s.ActiveActions {
		if action.WorkerName != workerName || action.Status != shared.ActionStatusReconciling {
			continue
		}
		if _, found := reportedSet[action.ID]; found {
			// Worker still has this action running — restore.
			action.Status = shared.ActionStatusRunning
			action.UpdatedAt = time.Now()
			if err := UpsertAction(action); err != nil {
				log.Error().Err(err).Str("action", action.ID).Msg("failed to persist restored Running status")
			}
			log.Info().Str("action", action.ID).Str("worker", workerName).Msg("action restored to Running after reconciliation")
		} else {
			// Worker does not know about this action — mark Failed.
			action.Status = shared.ActionStatusFailed
			action.FinishedAt = time.Now()
			action.UpdatedAt = time.Now()
			action.ContainerExitReason = "lost during worker offline"
			toFail = append(toFail, action)
			delete(s.ActiveActions, action.ID)
			if err := UpsertAction(action); err != nil {
				log.Error().Err(err).Str("action", action.ID).Msg("failed to persist failed reconciliation status")
			}
			log.Warn().Str("action", action.ID).Str("worker", workerName).Msg("action failed after reconciliation (not reported by worker)")
		}
	}
	s.ActionsMu.Unlock()

	// Finish jobs for failed actions outside the lock.
	for _, action := range toFail {
		s.finishJob(action.JobID, false)
	}
}

// ---------------------------------------------------------------------------
// Lookup helper
// ---------------------------------------------------------------------------

// GetActionByIDFromActiveOrDB looks up an action in ActiveActions first, then
// falls back to the database.
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

// ---------------------------------------------------------------------------
// Placeholder stubs — will be replaced in master/mirrorz.go (Task 9)
// ---------------------------------------------------------------------------

func (s *State) UpdateMirrorgoJSON() error { return nil }
func (s *State) UpdateMirrorZJSON() error  { return nil }
