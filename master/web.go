package master

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// isValidRepoID validates that a repo ID is safe for filesystem use.
func isValidRepoID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if c == '/' || c == '\\' || c == 0 {
			return false
		}
	}
	// Extra safety: ensure resolved path stays under ConfigDir
	return !strings.Contains(id, "..")
}

// ---------------------------------------------------------------------------
// Public API handlers (methods on *State)
// ---------------------------------------------------------------------------

// handleReposDispatch dispatches /api/repos by HTTP method.
func (s *State) handleReposDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleRepos(w, r)
	case http.MethodPost:
		s.handleRepoSave(w, r)
	case http.MethodDelete:
		s.handleRepoDelete(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleRepos — GET /api/repos
func (s *State) handleRepos(w http.ResponseWriter, r *http.Request) {
	s.ReposMu.RLock()
	defer s.ReposMu.RUnlock()

	keys := make([]string, 0, len(s.Repos))
	for k := range s.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]shared.Repo, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.Repos[k])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRepoSave — POST /api/repos
func (s *State) handleRepoSave(w http.ResponseWriter, r *http.Request) {
	var repo shared.Repo
	if err := json.NewDecoder(r.Body).Decode(&repo); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(repo.RepoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo id is required"})
		return
	}
	if !isValidRepoID(repo.RepoID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid repo id: must not contain /, \\, .., or null bytes"})
		return
	}

	// Write to config file
	filePath := filepath.Join(s.ConfigDir, repo.RepoID+".json")
	data, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Rename(tmpPath, filePath); err != nil {
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Update in-memory state
	s.ReposMu.Lock()
	s.Repos[repo.RepoID] = repo
	s.ReposMu.Unlock()

	// Ensure job exists (only for "free" interval type — same logic as migrateJobs)
	isFree := strings.ToLower(strings.TrimSpace(repo.SyncParams.Interval.Type)) == "free"
	s.JobsMu.Lock()
	job, exists := s.Jobs[repo.RepoID]
	if isFree {
		if !exists || job == nil {
			job = &shared.Job{
				RepoID:        repo.RepoID,
				Status:        shared.JobStatusWaiting,
				NextAttemptAt: time.Now(),
				UpdatedAt:     time.Now(),
			}
			s.Jobs[repo.RepoID] = job
		} else if job.Status == shared.JobStatusOrphan {
			job.Status = shared.JobStatusWaiting
			job.NextAttemptAt = time.Now()
			job.UpdatedAt = time.Now()
		}
	} else if exists && job.Status != shared.JobStatusOrphan {
		// Non-free interval: mark as orphan so scheduler ignores it
		job.Status = shared.JobStatusOrphan
		job.UpdatedAt = time.Now()
	}
	var jobCopy *shared.Job
	if job != nil {
		copied := *job
		jobCopy = &copied
	}
	s.JobsMu.Unlock()
	if jobCopy != nil {
		_ = UpsertJob(jobCopy)
		s.refreshMirrorJSONAsync()
	}

	writeJSON(w, http.StatusOK, repo)
}

// handleRepoDelete — DELETE /api/repos?id=<id>
func (s *State) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id parameter"})
		return
	}
	if !isValidRepoID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid repo id: must not contain /, \\, .., or null bytes"})
		return
	}

	// Remove config file
	filePath := filepath.Join(s.ConfigDir, id+".json")
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Remove from in-memory state
	s.ReposMu.Lock()
	delete(s.Repos, id)
	s.ReposMu.Unlock()

	// Mark job as orphan
	s.JobsMu.Lock()
	if job, ok := s.Jobs[id]; ok {
		job.Status = shared.JobStatusOrphan
		job.UpdatedAt = time.Now()
		_ = UpsertJob(job)
	}
	s.JobsMu.Unlock()

	// Remove from queue
	s.JobQueue.Remove(id)
	_ = DBDeleteAllQueueByJob(id)

	s.refreshMirrorJSONAsync()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleJobs — GET /api/jobs
func (s *State) handleJobs(w http.ResponseWriter, r *http.Request) {
	s.JobsMu.RLock()
	defer s.JobsMu.RUnlock()

	keys := make([]string, 0, len(s.Jobs))
	for k := range s.Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]shared.Job, 0, len(keys))
	for _, k := range keys {
		out = append(out, *s.Jobs[k])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleJobDelete — POST /api/jobs/delete?id=<id>
// Only Orphan jobs can be deleted.
func (s *State) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}

	s.JobsMu.Lock()
	job, exists := s.Jobs[id]
	if !exists {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	if job.Status != shared.JobStatusOrphan {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only orphan jobs can be deleted"})
		return
	}
	delete(s.Jobs, id)
	s.JobsMu.Unlock()

	_ = DeleteJob(id)
	_ = DBDeleteAllQueueByJob(id)

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleActions — GET /api/actions (active actions)
func (s *State) handleActions(w http.ResponseWriter, r *http.Request) {
	s.ActionsMu.RLock()
	defer s.ActionsMu.RUnlock()

	keys := make([]string, 0, len(s.ActiveActions))
	for k := range s.ActiveActions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]shared.Action, 0, len(keys))
	for _, k := range keys {
		out = append(out, *s.ActiveActions[k])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleActionsLookup — GET /api/actions/lookup?ids=id1,id2,id3
func (s *State) handleActionsLookup(w http.ResponseWriter, r *http.Request) {
	idsParam := r.URL.Query().Get("ids")
	if strings.TrimSpace(idsParam) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing ids query param"})
		return
	}
	rawIDs := strings.Split(idsParam, ",")
	out := make([]shared.Action, 0, len(rawIDs))
	for _, id := range rawIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if a := s.GetActionByIDFromActiveOrDB(id); a != nil {
			out = append(out, *a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleActionsByRepo — GET /api/actions/by_repo?repo_id=<id>&limit=100
func (s *State) handleActionsByRepo(w http.ResponseWriter, r *http.Request) {
	repoID := r.URL.Query().Get("repo_id")
	if strings.TrimSpace(repoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}
	actions, err := GetActionsByRepo(repoID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

// handleActionsRecent — GET /api/actions/recent?limit=100
func (s *State) handleActionsRecent(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}
	actions, err := GetActionsRecent(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

// handleQueue — GET /api/queue
func (s *State) handleQueue(w http.ResponseWriter, r *http.Request) {
	if s.JobQueue == nil {
		writeJSON(w, http.StatusOK, map[string]any{"paused": true, "queue": []string{}, "max_concurrency": 1})
		return
	}
	out := s.JobQueue.Snapshot()
	resp := map[string]any{
		"paused":          s.JobQueue.IsPaused(),
		"max_concurrency": s.JobQueue.GetMaxConcurrency(),
		"queue":           out,
	}
	if out == nil {
		resp["queue"] = []string{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleJobSetNextAttempt — POST /api/jobs/set_next_attempt?repo_id=<id>[&time=<RFC3339>]
// Sets the next attempt time for a Waiting or Scheduled job. If no time is
// given, defaults to now. If the job is Scheduled, it is removed from the
// queue and reverted to Waiting first.
func (s *State) handleJobSetNextAttempt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	repoID := r.URL.Query().Get("repo_id")
	if strings.TrimSpace(repoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}

	nextAt := time.Now()
	if t := r.URL.Query().Get("time"); t != "" {
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid time format, use RFC3339"})
			return
		}
		nextAt = parsed
	}

	s.JobsMu.Lock()
	job, ok := s.Jobs[repoID]
	if !ok {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	if job.Status == shared.JobStatusRunning || job.Status == shared.JobStatusOrphan {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job must be Waiting, Scheduled, or Paused"})
		return
	}

	// If Scheduled, remove from queue.
	if job.Status == shared.JobStatusScheduled {
		s.JobQueue.Remove(repoID)
		_ = DBDeleteAllQueueByJob(repoID)
	}

	// Preserve Paused status; only revert Scheduled to Waiting.
	if job.Status != shared.JobStatusPaused {
		job.Status = shared.JobStatusWaiting
	}
	job.NextAttemptAt = nextAt
	job.UpdatedAt = time.Now()
	jobCopy := *job
	s.JobsMu.Unlock()

	if err := UpsertJob(&jobCopy); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.refreshMirrorJSONAsync()
	writeJSON(w, http.StatusOK, &jobCopy)
}

// handleJobPause — POST /api/jobs/pause?repo_id=<id>&paused=true|false
// Pauses or unpauses a job. A paused job will not be scheduled.
// If the job is Scheduled, it is removed from the queue first.
func (s *State) handleJobPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	repoID := r.URL.Query().Get("repo_id")
	if strings.TrimSpace(repoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	pause := r.URL.Query().Get("paused") != "false" // default true

	s.JobsMu.Lock()
	job, ok := s.Jobs[repoID]
	if !ok {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}

	if pause {
		if job.Status == shared.JobStatusOrphan || job.Status == shared.JobStatusPaused {
			s.JobsMu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job cannot be paused in current status"})
			return
		}
		// If Scheduled, remove from queue.
		if job.Status == shared.JobStatusScheduled {
			s.JobQueue.Remove(repoID)
			_ = DBDeleteAllQueueByJob(repoID)
		}
		// Running jobs: set to Paused now; finishJob will preserve it
		// when the current action completes.
		job.Status = shared.JobStatusPaused
	} else {
		if job.Status != shared.JobStatusPaused {
			s.JobsMu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job is not paused"})
			return
		}
		// If an action is still running, resume as Running (not Waiting)
		// so the state machine stays consistent.
		if s.HasActiveActionForJob(repoID) {
			job.Status = shared.JobStatusRunning
		} else {
			job.Status = shared.JobStatusWaiting
			// Don't override NextAttemptAt — preserve the value set by
			// finishJob (now+interval) or by the user via set_next_attempt.
			// scheduleTick will pick it up when it's due.
		}
	}
	job.UpdatedAt = time.Now()
	jobCopy := *job
	s.JobsMu.Unlock()

	if err := UpsertJob(&jobCopy); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.refreshMirrorJSONAsync()
	writeJSON(w, http.StatusOK, &jobCopy)
}

// handleQueuePause — POST /api/queue/pause
func (s *State) handleQueuePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.JobQueue.SetPaused(true)
	_ = DBSetQueueState(true, s.JobQueue.GetMaxConcurrency())
	writeJSON(w, http.StatusOK, map[string]any{"paused": true})
}

// handleQueueContinue — POST /api/queue/continue
func (s *State) handleQueueContinue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.JobQueue.SetPaused(false)
	_ = DBSetQueueState(false, s.JobQueue.GetMaxConcurrency())
	writeJSON(w, http.StatusOK, map[string]any{"paused": false})
}

// handleQueueSetMaxConcurrency — POST /api/queue/set_max_concurrency?max=<number>
func (s *State) handleQueueSetMaxConcurrency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	maxStr := strings.TrimSpace(r.URL.Query().Get("max"))
	if maxStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing max parameter"})
		return
	}
	max, err := strconv.Atoi(maxStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid max value, must be a number"})
		return
	}
	if max < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max concurrency must be at least 1"})
		return
	}
	s.JobQueue.SetMaxConcurrency(max)
	_ = DBSetQueueState(s.JobQueue.IsPaused(), max)
	writeJSON(w, http.StatusOK, map[string]any{"max_concurrency": max})
}

// queueMoveResponse persists the queue after a move and writes the response.
func (s *State) queueMoveResponse(w http.ResponseWriter, ok bool) {
	if ok {
		if err := DBFlushQueue(s.JobQueue.Snapshot()); err != nil {
			log.Error().Err(err).Msg("failed to persist queue after move")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": s.JobQueue.Snapshot(), "max_concurrency": s.JobQueue.GetMaxConcurrency()})
}

// handleQueueMoveToHead — POST /api/queue/move_to_head?repo_id=<id>
func (s *State) handleQueueMoveToHead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	s.queueMoveResponse(w, s.JobQueue.MoveToHead(id))
}

// handleQueueMoveToTail — POST /api/queue/move_to_tail?repo_id=<id>
func (s *State) handleQueueMoveToTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	s.queueMoveResponse(w, s.JobQueue.MoveToTail(id))
}

// handleQueueMoveBefore — POST /api/queue/move_before?target_id=<id>&ref_id=<id>
func (s *State) handleQueueMoveBefore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target_id"))
	ref := strings.TrimSpace(r.URL.Query().Get("ref_id"))
	if target == "" || ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing target_id or ref_id"})
		return
	}
	s.queueMoveResponse(w, s.JobQueue.MoveBefore(target, ref))
}

// handleQueueMoveAfter — POST /api/queue/move_after?target_id=<id>&ref_id=<id>
func (s *State) handleQueueMoveAfter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target_id"))
	ref := strings.TrimSpace(r.URL.Query().Get("ref_id"))
	if target == "" || ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing target_id or ref_id"})
		return
	}
	s.queueMoveResponse(w, s.JobQueue.MoveAfter(target, ref))
}

// handleQueueDelete — POST /api/queue/delete?repo_id=<id>
func (s *State) handleQueueDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	removed := 0
	for s.JobQueue.Remove(id) {
		removed++
	}
	if err := DBDeleteAllQueueByJob(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// If job was Scheduled, revert to Waiting
	var jobCopy *shared.Job
	s.JobsMu.Lock()
	if job, ok := s.Jobs[id]; ok {
		if job.Status == shared.JobStatusScheduled {
			job.Status = shared.JobStatusWaiting
			job.UpdatedAt = time.Now()
			job.NextAttemptAt = time.Now().Add(time.Hour * 999999)
		}
		cp := *job
		jobCopy = &cp
	}
	s.JobsMu.Unlock()
	if jobCopy != nil {
		_ = UpsertJob(jobCopy)
		s.refreshMirrorJSONAsync()
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "queue": s.JobQueue.Snapshot(), "paused": s.JobQueue.IsPaused(), "max_concurrency": s.JobQueue.GetMaxConcurrency()})
}

// handleMirrors — GET /api/mirrors
func (s *State) handleMirrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	// trigger an update of the mirror status in background
	go func() { _ = s.UpdateMirrorgoJSON() }()

	mirrors, err := s.getMirrorStatus()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to get mirror status"})
		return
	}
	writeJSON(w, http.StatusOK, mirrors)
}

// handleMirrorZ — GET /mirrorz.json
func (s *State) handleMirrorZ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	doc := s.GenerateMirrorZ()
	go func() { _ = s.WriteMirrorZJSON(doc) }()
	writeJSON(w, http.StatusOK, doc)
}

// ---------------------------------------------------------------------------
// NEW: Worker management handlers
// ---------------------------------------------------------------------------

// handleWorkers — GET /api/workers
func (s *State) handleWorkers(w http.ResponseWriter, r *http.Request) {
	workers := s.WorkerMgr.GetAllWorkers()
	writeJSON(w, http.StatusOK, workers)
}

// handleWorkersRemove — POST /api/workers/remove?name=<name>
func (s *State) handleWorkersRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing name parameter"})
		return
	}
	if err := s.WorkerMgr.RemoveWorker(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Fail any remaining Reconciling/Running actions for that worker.
	var actionIDs []string
	s.ActionsMu.RLock()
	for _, action := range s.ActiveActions {
		if action.WorkerName == name {
			actionIDs = append(actionIDs, action.ID)
		}
	}
	s.ActionsMu.RUnlock()

	for _, id := range actionIDs {
		s.resolveAction(id, shared.ActionStatusFailed, 0, "worker removed", name)
	}

	writeJSON(w, http.StatusOK, map[string]string{"ok": "worker removed"})
}

// ---------------------------------------------------------------------------
// Log proxy handlers (proxy to worker)
// ---------------------------------------------------------------------------

// resolveActionWorker finds the worker name for an action.
func (s *State) resolveActionWorker(actionID string) (string, error) {
	if strings.TrimSpace(actionID) == "" {
		return "", fmt.Errorf("missing action_id")
	}
	action := s.GetActionByIDFromActiveOrDB(actionID)
	if action == nil {
		return "", fmt.Errorf("action not found")
	}
	if !s.WSHub.IsConnected(action.WorkerName) {
		return "", fmt.Errorf("worker not connected")
	}
	return action.WorkerName, nil
}

// handleLogsList — GET /api/logs/list?action_id=<id>
func (s *State) handleLogsList(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	workerName, err := s.resolveActionWorker(actionID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	entries, err := s.WSHub.LogList(workerName, actionID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// entries is already JSON, write it directly wrapped in the expected format.
	_, _ = w.Write([]byte(`{"action_id":"` + actionID + `","entries":`))
	_, _ = w.Write(entries)
	_, _ = w.Write([]byte(`}`))
}

// handleLogsRaw — GET /api/logs/raw?action_id=<id>&file=<name>
func (s *State) handleLogsRaw(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	file := r.URL.Query().Get("file")
	workerName, err := s.resolveActionWorker(actionID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	content, err := s.WSHub.LogRaw(workerName, actionID, file)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

// handleLogsStream — GET /api/logs/stream?action_id=<id>&file=<name>
func (s *State) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	file := r.URL.Query().Get("file")
	workerName, err := s.resolveActionWorker(actionID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	_, dataCh, stop, err := s.WSHub.LogStream(workerName, actionID, file)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer stop()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	_, _ = w.Write([]byte("data: MIRRORGO LOG STREAM: Connected\n\n"))
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, ok := <-dataCh:
			if !ok {
				return
			}
			lines := strings.Split(chunk, "\n")
			for _, line := range lines {
				if _, err := w.Write([]byte("data: " + line + "\n\n")); err != nil {
					return
				}
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// StartWebServer
// ---------------------------------------------------------------------------

// StartWebServer creates an HTTP mux, registers all routes, and starts
// listening. It blocks until the server exits.
func (s *State) StartWebServer(addr, authToken string) {
	mux := http.NewServeMux()

	// Public API routes.
	mux.HandleFunc("/api/repos", s.handleReposDispatch)
	mux.HandleFunc("/api/jobs", s.handleJobs)
	mux.HandleFunc("/api/jobs/delete", s.handleJobDelete)
	mux.HandleFunc("/api/jobs/set_next_attempt", s.handleJobSetNextAttempt)
	mux.HandleFunc("/api/jobs/next_attempt_now", s.handleJobSetNextAttempt) // legacy alias
	mux.HandleFunc("/api/jobs/pause", s.handleJobPause)
	mux.HandleFunc("/api/actions", s.handleActions)
	mux.HandleFunc("/api/actions/lookup", s.handleActionsLookup)
	mux.HandleFunc("/api/actions/by_repo", s.handleActionsByRepo)
	mux.HandleFunc("/api/actions/recent", s.handleActionsRecent)
	mux.HandleFunc("/api/logs/list", s.handleLogsList)
	mux.HandleFunc("/api/logs/raw", s.handleLogsRaw)
	mux.HandleFunc("/api/logs/stream", s.handleLogsStream)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/queue/pause", s.handleQueuePause)
	mux.HandleFunc("/api/queue/continue", s.handleQueueContinue)
	mux.HandleFunc("/api/queue/set_max_concurrency", s.handleQueueSetMaxConcurrency)
	mux.HandleFunc("/api/queue/move_to_head", s.handleQueueMoveToHead)
	mux.HandleFunc("/api/queue/move_to_tail", s.handleQueueMoveToTail)
	mux.HandleFunc("/api/queue/move_before", s.handleQueueMoveBefore)
	mux.HandleFunc("/api/queue/move_after", s.handleQueueMoveAfter)
	mux.HandleFunc("/api/queue/delete", s.handleQueueDelete)
	mux.HandleFunc("/api/mirrors", s.handleMirrors)
	mux.HandleFunc("/api/workers", s.handleWorkers)
	mux.HandleFunc("/api/workers/remove", s.handleWorkersRemove)

	// ZFS API routes.
	mux.HandleFunc("/api/zfs/refresh", s.handleZFSRefresh)
	mux.HandleFunc("/api/zfs/report", s.handleZFSReport)
	mux.HandleFunc("/api/zfs/pools", s.handleZFSPools)
	mux.HandleFunc("/api/zfs/datasets", s.handleZFSDatasets)
	mux.HandleFunc("/api/zfs/snapshots", s.handleZFSSnapshots)
	mux.HandleFunc("/api/zfs/snapshots/create", s.handleZFSCreateSnapshot)
	mux.HandleFunc("/api/zfs/snapshots/destroy", s.handleZFSDestroySnapshot)
	mux.HandleFunc("/api/zfs/datasets/create", s.handleZFSCreateDataset)
	mux.HandleFunc("/api/zfs/datasets/set_property", s.handleZFSSetProperty)

	// MirrorZ JSON endpoint.
	mux.HandleFunc("/mirrorz.json", s.handleMirrorZ)

	// Internal API routes (protected by auth middleware).
	mux.HandleFunc("/api/internal/register", s.WorkerMgr.Register)
	mux.HandleFunc("/api/internal/heartbeat", s.WorkerMgr.Heartbeat)
	mux.HandleFunc("/api/internal/ws", s.WSHub.HandleWS)

	// Static files from embedded UI.
	if s.UIFS != nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.ServeFileFS(w, r, s.UIFS, "index.html")
				return
			}
			http.ServeFileFS(w, r, s.UIFS, strings.TrimPrefix(r.URL.Path, "/"))
		})
	}

	handler := shared.InternalAuthMiddleware(authToken, mux)

	log.Info().Str("addr", addr).Msg("Starting web server")
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal().Err(err).Msg("Web server exited")
	}
}
