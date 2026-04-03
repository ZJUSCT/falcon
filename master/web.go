package master

import (
	"encoding/json"
	"io"
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

	// Write to config file
	filePath := filepath.Join(s.ConfigDir, repo.RepoID+".json")
	data, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Update in-memory state
	s.ReposMu.Lock()
	s.Repos[repo.RepoID] = repo
	s.ReposMu.Unlock()

	// Ensure job exists
	s.JobsMu.Lock()
	job, exists := s.Jobs[repo.RepoID]
	if !exists {
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
	s.JobsMu.Unlock()
	_ = UpsertJob(job)

	writeJSON(w, http.StatusOK, repo)
}

// handleRepoDelete — DELETE /api/repos?id=<id>
func (s *State) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id parameter"})
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

// handleJobNextAttemptNow — POST /api/jobs/next_attempt_now?repo_id=<id>
func (s *State) handleJobNextAttemptNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	repoID := r.URL.Query().Get("repo_id")
	if strings.TrimSpace(repoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	s.JobsMu.Lock()
	job, ok := s.Jobs[repoID]
	if !ok {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	if job.Status != shared.JobStatusWaiting {
		s.JobsMu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job is not in Waiting status"})
		return
	}
	job.NextAttemptAt = time.Now()
	job.UpdatedAt = time.Now()
	s.JobsMu.Unlock()

	if err := UpsertJob(job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, job)
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
	ok := s.JobQueue.MoveToHead(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": s.JobQueue.Snapshot(), "max_concurrency": s.JobQueue.GetMaxConcurrency()})
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
	ok := s.JobQueue.MoveToTail(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": s.JobQueue.Snapshot(), "max_concurrency": s.JobQueue.GetMaxConcurrency()})
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
	ok := s.JobQueue.MoveBefore(target, ref)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": s.JobQueue.Snapshot(), "max_concurrency": s.JobQueue.GetMaxConcurrency()})
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
	ok := s.JobQueue.MoveAfter(target, ref)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": s.JobQueue.Snapshot(), "max_concurrency": s.JobQueue.GetMaxConcurrency()})
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
	s.JobsMu.Lock()
	if job, ok := s.Jobs[id]; ok {
		if job.Status == shared.JobStatusScheduled {
			job.Status = shared.JobStatusWaiting
			job.UpdatedAt = time.Now()
			job.NextAttemptAt = time.Now().Add(time.Hour * 999999)
		}
	}
	s.JobsMu.Unlock()
	s.JobsMu.RLock()
	if job, ok := s.Jobs[id]; ok {
		s.JobsMu.RUnlock()
		_ = UpsertJob(job)
	} else {
		s.JobsMu.RUnlock()
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

	// Fail any remaining Reconciling actions for that worker.
	s.ActionsMu.Lock()
	var toFail []*shared.Action
	for _, action := range s.ActiveActions {
		if action.WorkerName == name && action.Status == shared.ActionStatusReconciling {
			action.Status = shared.ActionStatusFailed
			action.FinishedAt = time.Now()
			action.UpdatedAt = time.Now()
			action.ContainerExitReason = "worker removed"
			toFail = append(toFail, action)
			delete(s.ActiveActions, action.ID)
			_ = UpsertAction(action)
		}
	}
	s.ActionsMu.Unlock()

	for _, action := range toFail {
		s.finishJob(action.JobID, false)
	}

	writeJSON(w, http.StatusOK, map[string]string{"ok": "worker removed"})
}

// ---------------------------------------------------------------------------
// Log proxy handlers (proxy to worker)
// ---------------------------------------------------------------------------

// handleLogsList — GET /api/logs/list?action_id=<id>
func (s *State) handleLogsList(w http.ResponseWriter, r *http.Request) {
	s.proxyLogRequest(w, r)
}

// handleLogsRaw — GET /api/logs/raw?action_id=<id>&file=<name>
func (s *State) handleLogsRaw(w http.ResponseWriter, r *http.Request) {
	s.proxyLogRequest(w, r)
}

// handleLogsStream — GET /api/logs/stream?action_id=<id>&file=<name>&from=<start|end>
func (s *State) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	if strings.TrimSpace(actionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing action_id"})
		return
	}

	action := s.GetActionByIDFromActiveOrDB(actionID)
	if action == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "action not found"})
		return
	}

	worker, ok := s.WorkerMgr.GetWorker(action.WorkerName)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker not available"})
		return
	}

	// Build the proxy URL.
	proxyURL := worker.Addr + r.URL.Path + "?" + r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, proxyURL, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create proxy request"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)

	client := &http.Client{} // no timeout for streaming
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to reach worker: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// Copy all response headers.
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the response body with flushing for SSE.
	flusher, flushOK := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return
			}
			if flushOK {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// proxyLogRequest proxies a log list/raw request to the appropriate worker.
func (s *State) proxyLogRequest(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	if strings.TrimSpace(actionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing action_id"})
		return
	}

	action := s.GetActionByIDFromActiveOrDB(actionID)
	if action == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "action not found"})
		return
	}

	worker, ok := s.WorkerMgr.GetWorker(action.WorkerName)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker not available"})
		return
	}

	proxyURL := worker.Addr + r.URL.Path + "?" + r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, proxyURL, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create proxy request"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to reach worker: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// Forward response headers and body.
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	mux.HandleFunc("/api/jobs/next_attempt_now", s.handleJobNextAttemptNow)
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
