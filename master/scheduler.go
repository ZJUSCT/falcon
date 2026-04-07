package master

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
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
	ConfigDir string // directory containing repo JSON config files
	UIFS      fs.FS  // embedded UI filesystem (set from main.go)
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
		// Repo sync lock: never schedule if there is already an active
		// action (Running or Reconciling) for this repo.
		if s.HasActiveActionForJob(job.RepoID) {
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

		// Repo sync lock (secondary check): skip if an active action already
		// exists, e.g. due to recovery restoring a Reconciling action.
		if s.HasActiveActionForJob(jobID) {
			log.Warn().Str("job", jobID).Msg("active action exists, skipping dispatch")
			_ = DBDequeueOne(jobID)
			// Revert job back to Running (it has an active action).
			s.JobsMu.Lock()
			if j, ok := s.Jobs[jobID]; ok && j.Status == shared.JobStatusScheduled {
				j.Status = shared.JobStatusRunning
				j.UpdatedAt = time.Now()
				_ = UpsertJob(j)
			}
			s.JobsMu.Unlock()
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

		// Create and persist action BEFORE dispatch, so a crash mid-dispatch
		// leaves a recoverable Reconciling action (repo sync lock holds).
		actionID := fmt.Sprintf("%d-%04x", time.Now().UnixNano(), rand.Intn(0xFFFF))
		now := time.Now()
		action := &shared.Action{
			ID:               actionID,
			JobID:            jobID,
			Status:           shared.ActionStatusRunning,
			ContainerName:    "syncing-" + jobID,
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
			s.ActionsMu.Lock()
			delete(s.ActiveActions, actionID)
			s.ActionsMu.Unlock()
			s.JobQueue.Enqueue(jobID)
			requeued++
			continue
		}

		// Update job to Running.
		s.JobsMu.Lock()
		job.Status = shared.JobStatusRunning
		job.LastAttemptAt = now
		job.UpdatedAt = now
		job.Actions = append(job.Actions, actionID)
		if len(job.Actions) > 100 {
			job.Actions = job.Actions[len(job.Actions)-100:]
		}
		jobCopy := *job
		s.JobsMu.Unlock()

		if err := UpsertJob(&jobCopy); err != nil {
			log.Error().Err(err).Str("job", jobID).Msg("failed to persist running job")
		}

		// Now dispatch — action and job are durable. If dispatch fails,
		// resolve the action as Failed (which reverts the job to Waiting).
		dispAction := shared.DispatchAction{
			ID:               actionID,
			JobID:            jobID,
			ContainerImage:   repo.SyncParams.Image,
			ContainerCommand: repo.SyncParams.Command,
			ContainerVolumes: repo.SyncParams.Volumes,
			ContainerEnv:     repo.SyncParams.Environments,
			ContainerTimeout: repo.SyncParams.Timeout,
		}

		if err := s.WSHub.Dispatch(worker.Name, dispAction); err != nil {
			_ = DBDequeueOne(jobID) // always dequeue from persistent queue
			if _, rejected := err.(*DispatchRejectedError); rejected {
				log.Error().Err(err).Str("job", jobID).Str("worker", worker.Name).Msg("dispatch rejected by worker")
				s.resolveAction(actionID, shared.ActionStatusFailed, 0, err.Error(), worker.Name)
			} else {
				log.Warn().Err(err).Str("job", jobID).Str("worker", worker.Name).Str("action", actionID).Msg("dispatch reply lost, marking Reconciling")
				s.ActionsMu.Lock()
				if a, ok := s.ActiveActions[actionID]; ok {
					a.Status = shared.ActionStatusReconciling
					a.UpdatedAt = time.Now()
					actionCopy := *a
					s.ActionsMu.Unlock()
					_ = UpsertAction(&actionCopy)
				} else {
					s.ActionsMu.Unlock()
				}
			}
			continue
		}

		// Remove from persistent queue LAST — after action, job, and dispatch.
		_ = DBDequeueOne(jobID)

		// Re-check that the worker is still online; if not, mark action as Reconciling.
		w, wOK := s.WorkerMgr.GetWorker(worker.Name)
		if !wOK || w.Status != shared.WorkerStatusOnline {
			log.Warn().Str("action", actionID).Str("worker", worker.Name).Msg("worker went offline after dispatch, marking Reconciling")
			s.ActionsMu.Lock()
			action.Status = shared.ActionStatusReconciling
			action.UpdatedAt = time.Now()
			actionCopy := *action
			s.ActionsMu.Unlock()
			_ = UpsertAction(&actionCopy)
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

// selectWorker picks an online, WS-connected worker matching the repo's
// affinity that has the fewest running actions.
func (s *State) selectWorker(repo *shared.Repo) *shared.Worker {
	online := s.WorkerMgr.GetOnlineWorkers()
	var best *shared.Worker
	bestLoad := -1
	for _, w := range online {
		if !MatchWorker(w, repo) {
			continue
		}
		// Only select workers with an active WS connection — dispatch goes via WS.
		if !s.WSHub.IsConnected(w.Name) {
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

// ---------------------------------------------------------------------------
// resolveAction — the single entry point for terminating an action
// ---------------------------------------------------------------------------

// resolveAction moves an action to a terminal state (Succeeded or Failed),
// removes it from ActiveActions, persists it, finishes the job, and sends an
// ack to the worker. All code paths that finish an action MUST go through here.
//
// If the action is not found in ActiveActions, it still sends an ack so the
// worker can clean up orphaned containers.
func (s *State) resolveAction(actionID, status string, exitCode int, exitReason string, workerName string) {
	s.ActionsMu.Lock()
	action, exists := s.ActiveActions[actionID]
	if !exists {
		s.ActionsMu.Unlock()
		log.Warn().Str("action", actionID).Str("worker", workerName).Msg("resolveAction: unknown action, sending ack for cleanup")
		s.WSHub.SendAck(workerName, actionID)
		return
	}

	action.Status = status
	action.ContainerStatus = shared.ContainerStatusExited
	action.ContainerExitCode = exitCode
	action.ContainerExitReason = exitReason
	action.FinishedAt = time.Now()
	action.UpdatedAt = time.Now()

	// Persist BEFORE removing from ActiveActions. If persistence fails,
	// keep the action in memory so recovery can handle it on next restart.
	if err := UpsertAction(action); err != nil {
		log.Error().Err(err).Str("action", actionID).Msg("resolveAction: persist failed, will ack worker but keep action for recovery")
		// Still ack so worker can clean up, but keep in ActiveActions
		// so master restart re-processes it.
		s.ActionsMu.Unlock()
		s.WSHub.SendAck(workerName, actionID)
		return
	}
	delete(s.ActiveActions, actionID)
	s.ActionsMu.Unlock()

	s.finishJob(action.JobID, status == shared.ActionStatusSucceeded)
	s.WSHub.SendAck(workerName, actionID)

	log.Info().Str("action", actionID).Str("status", status).Str("worker", workerName).Msg("action resolved")
}

// HandleActionStatus is called by WSHub when a worker reports action status.
func (s *State) HandleActionStatus(workerName string, msg *shared.WSActionResult) {
	isTerminal := msg.Status == shared.ActionStatusSucceeded || msg.Status == shared.ActionStatusFailed

	if isTerminal {
		s.resolveAction(msg.ActionID, msg.Status, msg.ExitCode, msg.ExitReason, workerName)
		return
	}

	// Non-terminal update (e.g. container status change) — update in place.
	s.ActionsMu.Lock()
	action, exists := s.ActiveActions[msg.ActionID]
	if !exists {
		s.ActionsMu.Unlock()
		return
	}
	action.ContainerStatus = msg.ContainerStatus
	action.UpdatedAt = msg.UpdatedAt
	s.ActionsMu.Unlock()
}

// finishJob updates the job after an action completes and schedules the next attempt.
// Lock ordering: ReposMu before JobsMu (same as web.go repo handlers).
func (s *State) finishJob(jobID string, succeeded bool) {
	now := time.Now()

	// Read repo interval FIRST (ReposMu before JobsMu to avoid deadlock).
	s.ReposMu.RLock()
	repo, repoExists := s.Repos[jobID]
	s.ReposMu.RUnlock()

	interval := time.Hour
	if repoExists {
		interval = ParseInterval(repo.SyncParams.Interval)
	}

	s.JobsMu.Lock()
	job, exists := s.Jobs[jobID]
	if !exists {
		s.JobsMu.Unlock()
		log.Error().Str("job", jobID).Msg("finishJob: job not found")
		return
	}

	if job.Status == shared.JobStatusOrphan {
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

	job.NextAttemptAt = now.Add(interval)
	job.Status = shared.JobStatusWaiting
	job.UpdatedAt = now
	jobCopy := *job // copy before unlock to avoid data race on UpsertJob
	s.JobsMu.Unlock()

	if err := UpsertJob(&jobCopy); err != nil {
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

// WarnStaleReconcilingActions logs warnings for actions that have been in
// Reconciling state longer than the given threshold. This runs periodically
// so admins know to remove dead workers.
func (s *State) WarnStaleReconcilingActions(threshold time.Duration) {
	now := time.Now()
	s.ActionsMu.RLock()
	defer s.ActionsMu.RUnlock()
	for _, action := range s.ActiveActions {
		if action.Status == shared.ActionStatusReconciling && now.Sub(action.UpdatedAt) > threshold {
			log.Warn().
				Str("action", action.ID).
				Str("job", action.JobID).
				Str("worker", action.WorkerName).
				Dur("stale_for", now.Sub(action.UpdatedAt)).
				Msg("action stuck in Reconciling — consider removing the offline worker via POST /api/workers/remove")
		}
	}
}

// StaleReconcilingCheckLoop warns about Reconciling actions every 60s.
func (s *State) StaleReconcilingCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.WarnStaleReconcilingActions(10 * time.Minute)
		}
	}
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

// HandleHeartbeatDiff is called on each heartbeat to reconcile the master's
// view of a worker's actions with what the worker actually reports.
//
// Single-pass logic:
//   - Reconciling + reported  → restore to Running
//   - Reconciling + absent    → resolveAction as Failed
//   - Running     + absent    → query worker, then resolveAction if terminal
//   - Running     + reported  → no action needed
func (s *State) HandleHeartbeatDiff(workerName string, reportedActions []string) {
	reportedSet := make(map[string]struct{}, len(reportedActions))
	for _, id := range reportedActions {
		reportedSet[id] = struct{}{}
	}

	// Single pass under lock: restore reconciling, collect IDs for async work.
	var toQuery []string // absent from heartbeat → query worker before resolving

	s.ActionsMu.Lock()
	for _, action := range s.ActiveActions {
		if action.WorkerName != workerName {
			continue
		}
		_, reported := reportedSet[action.ID]

		switch {
		case action.Status == shared.ActionStatusReconciling && reported:
			action.Status = shared.ActionStatusRunning
			action.UpdatedAt = time.Now()
			_ = UpsertAction(action)
			log.Info().Str("action", action.ID).Str("worker", workerName).Msg("action restored to Running after reconciliation")

		case !reported && (action.Status == shared.ActionStatusReconciling || action.Status == shared.ActionStatusRunning):
			// Both Reconciling and Running actions absent from heartbeat
			// must be queried before resolving — the worker may have the
			// result in PendingAck (e.g. after worker restart).
			toQuery = append(toQuery, action.ID)
		}
	}
	s.ActionsMu.Unlock()

	// Query worker for actions not in heartbeat.
	if len(toQuery) == 0 {
		return
	}
	if !s.WSHub.IsConnected(workerName) {
		return
	}
	for _, actionID := range toQuery {
		asr, err := s.WSHub.QueryActionStatus(workerName, actionID)
		if err != nil {
			log.Warn().Err(err).Str("action", actionID).Str("worker", workerName).Msg("failed to query action status from worker")
			continue
		}
		if asr.Found && asr.Status == shared.ActionStatusRunning {
			continue // still running, wait for WS
		}
		status := shared.ActionStatusFailed
		if asr.Found && asr.Status == shared.ActionStatusSucceeded {
			status = shared.ActionStatusSucceeded
		}
		reason := "action not found on worker"
		exitCode := 0
		if asr.Found {
			reason = asr.ExitReason
			exitCode = asr.ExitCode
		}
		s.resolveAction(actionID, status, exitCode, reason, workerName)
	}
}

// ---------------------------------------------------------------------------
// Lookup helpers
// ---------------------------------------------------------------------------

// HasActiveActionForJob returns true if there is any active action (Running or
// Reconciling) for the given job ID. This serves as the repo sync lock —
// ensuring we never run two containers for the same repo concurrently.
// Caller must NOT hold ActionsMu.
func (s *State) HasActiveActionForJob(jobID string) bool {
	s.ActionsMu.RLock()
	defer s.ActionsMu.RUnlock()
	for _, a := range s.ActiveActions {
		if a.JobID == jobID {
			return true
		}
	}
	return false
}

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

