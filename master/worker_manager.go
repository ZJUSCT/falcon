package master

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// WorkerManager handles worker registration, heartbeat processing, offline
// detection, affinity matching, and dispatch/query helpers.
type WorkerManager struct {
	mu      sync.RWMutex
	workers map[string]*shared.Worker
	token   string

	onWorkerOffline func(name string)
	OnHeartbeat     func(workerName string, reportedActions []string)
}

// NewWorkerManager creates a new WorkerManager with the given internal auth token.
func NewWorkerManager(token string) *WorkerManager {
	return &WorkerManager{
		workers: make(map[string]*shared.Worker),
		token:   token,
	}
}

// SetOfflineCallback sets the callback for when a worker goes offline.
func (wm *WorkerManager) SetOfflineCallback(onOffline func(string)) {
	wm.onWorkerOffline = onOffline
}

// LoadFromDB loads workers from the database into memory.
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

// MarkAllOffline sets all in-memory workers to Offline status.
func (wm *WorkerManager) MarkAllOffline() {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	for _, w := range wm.workers {
		w.Status = shared.WorkerStatusOffline
	}
}

// MarkOffline sets a worker's status to Offline. Called when WS disconnects.
func (wm *WorkerManager) MarkOffline(name string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if w, ok := wm.workers[name]; ok {
		w.Status = shared.WorkerStatusOffline
		_ = UpsertWorker(w)
	}
}

// MarkOnline sets a worker's status to Online. Called when WS connects.
func (wm *WorkerManager) MarkOnline(name string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if w, ok := wm.workers[name]; ok {
		w.Status = shared.WorkerStatusOnline
		w.LastHeartbeat = time.Now()
		_ = UpsertWorker(w)
		log.Info().Str("worker", name).Msg("worker online (WS connected)")
	}
}

// Register handles POST /api/internal/register.
func (wm *WorkerManager) Register(w http.ResponseWriter, r *http.Request) {
	var req shared.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, shared.RegisterResponse{OK: false, Message: "invalid request body"})
		return
	}

	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, shared.RegisterResponse{OK: false, Message: "name is required"})
		return
	}

	// If the old instance was Online, trigger offline callback first so
	// its Running actions are marked Reconciling before the new instance
	// takes over. This is the restart-recovery path.
	wm.mu.Lock()
	existing, exists := wm.workers[req.Name]
	wasOnline := exists && existing.Status == shared.WorkerStatusOnline
	wm.mu.Unlock()

	if wasOnline && wm.onWorkerOffline != nil {
		log.Warn().Str("worker", req.Name).Msg("worker re-registering while still marked online — running offline handler for old instance")
		wm.onWorkerOffline(req.Name)
	}

	now := time.Now()
	worker := &shared.Worker{
		Name:           req.Name,
		Labels:         req.Labels,
		Vars:           req.Vars,
		Status:         shared.WorkerStatusOffline, // stays Offline until WS connects
		LastHeartbeat:  now,
		RunningActions: nil,
		RegisteredAt:   now,
	}
	wm.mu.Lock()
	if exists {
		worker.RegisteredAt = existing.RegisteredAt
	}
	wm.workers[req.Name] = worker
	wm.mu.Unlock()

	if err := UpsertWorker(worker); err != nil {
		log.Error().Err(err).Str("worker", req.Name).Msg("failed to persist worker registration")
		writeJSON(w, http.StatusInternalServerError, shared.RegisterResponse{OK: false, Message: "persistence error"})
		return
	}

	log.Info().Str("worker", req.Name).Msg("worker registered (online)")

	writeJSON(w, http.StatusOK, shared.RegisterResponse{OK: true})
}

// Heartbeat handles POST /api/internal/heartbeat.
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

	worker.LastHeartbeat = time.Now()
	worker.RunningActions = req.RunningActions
	workerCopy := *worker
	wm.mu.Unlock()

	if err := UpsertWorker(&workerCopy); err != nil {
		log.Error().Err(err).Str("worker", req.Name).Msg("failed to persist heartbeat")
	}

	if wm.OnHeartbeat != nil {
		wm.OnHeartbeat(req.Name, req.RunningActions)
	}

	writeJSON(w, http.StatusOK, shared.HeartbeatResponse{OK: true})
}

// Note: Worker online/offline is now driven entirely by WS connection
// (OnWorkerWSReady/OnWorkerWSLost). The old heartbeat-based CheckOffline
// loop has been removed to avoid conflicting with WS-driven status.

// GetOnlineWorkers returns a snapshot of all online workers (copies).
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

// GetWorker returns a copy of the worker with the given name.
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

// GetAllWorkers returns copies of all workers.
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

// RemoveWorker removes a worker by name. Returns an error if the worker is Online.
func (wm *WorkerManager) RemoveWorker(name string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	w, ok := wm.workers[name]
	if !ok {
		return fmt.Errorf("worker %q not found", name)
	}
	if w.Status == shared.WorkerStatusOnline {
		return fmt.Errorf("cannot remove online worker %q", name)
	}
	delete(wm.workers, name)
	return DeleteWorker(name)
}

// MatchWorker checks whether a worker matches the node affinity rules defined
// in the repo's sync parameters. If no affinity is configured, any worker matches.
func MatchWorker(worker *shared.Worker, repo *shared.Repo) bool {
	if repo.SyncParams.Node != "" {
		if worker.Name != repo.SyncParams.Node {
			return false
		}
	}
	for k, v := range repo.SyncParams.NodeSelector {
		if worker.Labels[k] != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Local helpers
// ---------------------------------------------------------------------------

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error().Err(err).Msg("failed to write JSON response")
	}
}
