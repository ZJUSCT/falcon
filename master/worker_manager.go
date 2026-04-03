package master

import (
	"bytes"
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

// WorkerManager handles worker registration, heartbeat processing, offline
// detection, affinity matching, and dispatch/query helpers.
type WorkerManager struct {
	mu      sync.RWMutex
	workers map[string]*shared.Worker
	token   string

	onWorkerOnline  func(name string)
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

// SetCallbacks sets the callbacks for worker state transitions.
func (wm *WorkerManager) SetCallbacks(onOnline, onOffline func(string)) {
	wm.onWorkerOnline = onOnline
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

// Register handles POST /api/internal/register.
func (wm *WorkerManager) Register(w http.ResponseWriter, r *http.Request) {
	var req shared.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, shared.RegisterResponse{OK: false, Message: "invalid request body"})
		return
	}

	if req.Name == "" || req.Addr == "" {
		writeJSON(w, http.StatusBadRequest, shared.RegisterResponse{OK: false, Message: "name and addr are required"})
		return
	}

	wm.mu.Lock()
	existing, exists := wm.workers[req.Name]

	if exists && existing.Status == shared.WorkerStatusOnline {
		wm.mu.Unlock()
		writeJSON(w, http.StatusConflict, shared.RegisterResponse{OK: false, Message: "worker already online"})
		return
	}

	now := time.Now()
	worker := &shared.Worker{
		Name:           req.Name,
		Addr:           req.Addr,
		Labels:         req.Labels,
		Status:         shared.WorkerStatusOnline,
		LastHeartbeat:  now,
		RunningActions: nil,
		RegisteredAt:   now,
	}
	if exists {
		// Re-register: keep the original registration time.
		worker.RegisteredAt = existing.RegisteredAt
	}
	wm.workers[req.Name] = worker
	wm.mu.Unlock()

	if err := UpsertWorker(worker); err != nil {
		log.Error().Err(err).Str("worker", req.Name).Msg("failed to persist worker registration")
		writeJSON(w, http.StatusInternalServerError, shared.RegisterResponse{OK: false, Message: "persistence error"})
		return
	}

	log.Info().Str("worker", req.Name).Str("addr", req.Addr).Msg("worker registered")

	if wm.onWorkerOnline != nil {
		wm.onWorkerOnline(req.Name)
	}

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
	worker.Status = shared.WorkerStatusOnline
	wm.mu.Unlock()

	if err := UpsertWorker(worker); err != nil {
		log.Error().Err(err).Str("worker", req.Name).Msg("failed to persist heartbeat")
	}

	if wm.OnHeartbeat != nil {
		wm.OnHeartbeat(req.Name, req.RunningActions)
	}

	writeJSON(w, http.StatusOK, shared.HeartbeatResponse{OK: true})
}

// CheckOffline scans workers and marks stale ones as Offline. Returns names of
// workers that were transitioned to Offline.
func (wm *WorkerManager) CheckOffline(threshold time.Duration) []string {
	now := time.Now()
	var offlined []string

	wm.mu.Lock()
	for name, w := range wm.workers {
		if w.Status == shared.WorkerStatusOnline && now.Sub(w.LastHeartbeat) > threshold {
			w.Status = shared.WorkerStatusOffline
			offlined = append(offlined, name)
			if err := UpsertWorker(w); err != nil {
				log.Error().Err(err).Str("worker", name).Msg("failed to persist offline status")
			}
		}
	}
	wm.mu.Unlock()

	for _, name := range offlined {
		log.Warn().Str("worker", name).Msg("worker marked offline (heartbeat timeout)")
		if wm.onWorkerOffline != nil {
			wm.onWorkerOffline(name)
		}
	}

	return offlined
}

// OfflineCheckLoop runs CheckOffline every 5 seconds until the context is cancelled.
func (wm *WorkerManager) OfflineCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			wm.CheckOffline(30 * time.Second)
		}
	}
}

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
// HTTP helpers for dispatching to workers
// ---------------------------------------------------------------------------

// DispatchToWorker sends a dispatch request to a worker.
func DispatchToWorker(worker *shared.Worker, req *shared.DispatchRequest, token string) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal dispatch request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	httpReq, err := http.NewRequest("POST", worker.Addr+"/api/internal/dispatch", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("dispatch to %s: %w", worker.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dispatch to %s: status %d: %s", worker.Name, resp.StatusCode, string(respBody))
	}

	var dr shared.DispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return fmt.Errorf("decode dispatch response from %s: %w", worker.Name, err)
	}
	if !dr.OK {
		return fmt.Errorf("worker %s rejected dispatch: %s", worker.Name, dr.Message)
	}
	return nil
}

// QueryActionStatus queries a worker for the status of a specific action.
func QueryActionStatus(worker *shared.Worker, actionID, token string) (*shared.ActionStatusResponse, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	httpReq, err := http.NewRequest("GET", worker.Addr+"/api/internal/action_status?id="+actionID, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("query action status from %s: %w", worker.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("query action status from %s: status %d: %s", worker.Name, resp.StatusCode, string(respBody))
	}

	var asr shared.ActionStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&asr); err != nil {
		return nil, fmt.Errorf("decode action status response from %s: %w", worker.Name, err)
	}
	return &asr, nil
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
