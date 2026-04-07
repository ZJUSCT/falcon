package worker

import (
	"sync"
	"time"

	"github.com/star/mirrorgo/shared"
)

// Phase represents a worker-side action lifecycle stage.
type Phase int

const (
	PhaseRunning        Phase = iota // container is running (or being monitored)
	PhasePendingAck                  // container exited, WS result sent, waiting for master ack
	PhasePendingCleanup              // master acked, waiting for deferred container cleanup
)

func (p Phase) String() string {
	switch p {
	case PhaseRunning:
		return "Running"
	case PhasePendingAck:
		return "PendingAck"
	case PhasePendingCleanup:
		return "PendingCleanup"
	default:
		return "Unknown"
	}
}

// TrackedAction wraps a shared.Action with worker-side lifecycle state.
type TrackedAction struct {
	Action  *shared.Action
	Phase   Phase
	AckedAt time.Time // set when Phase transitions to PendingCleanup
}

// Tracker is the single source of truth for action state on the worker.
type Tracker struct {
	mu      sync.RWMutex
	actions map[string]*TrackedAction
}

func NewTracker() *Tracker {
	return &Tracker{
		actions: make(map[string]*TrackedAction),
	}
}

// Add inserts a new tracked action in the given phase.
func (t *Tracker) Add(act *shared.Action, phase Phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.actions[act.ID] = &TrackedAction{Action: act, Phase: phase}
}

// Get returns the tracked action for the given ID, or nil.
func (t *Tracker) Get(id string) *TrackedAction {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.actions[id]
}

// Has returns true if the action ID is tracked in any phase.
func (t *Tracker) Has(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.actions[id]
	return ok
}

// Finish transitions an action from Running to PendingAck.
// Updates the action's status and finishedAt. Returns false if not found or
// not in Running phase.
func (t *Tracker) Finish(id string, status string, exitCode int, exitReason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ta, ok := t.actions[id]
	if !ok || ta.Phase != PhaseRunning {
		return false
	}
	ta.Action.Status = status
	ta.Action.ContainerExitCode = exitCode
	ta.Action.ContainerExitReason = exitReason
	ta.Action.ContainerStatus = shared.ContainerStatusExited
	ta.Action.FinishedAt = time.Now()
	ta.Phase = PhasePendingAck
	return true
}

// Ack transitions an action from PendingAck to PendingCleanup.
// Returns false if not found or not in PendingAck phase.
func (t *Tracker) Ack(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ta, ok := t.actions[id]
	if !ok || ta.Phase != PhasePendingAck {
		return false
	}
	ta.Phase = PhasePendingCleanup
	ta.AckedAt = time.Now()
	return true
}

// Remove deletes a tracked action. Called after successful cleanup.
func (t *Tracker) Remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.actions, id)
}

// RunningIDs returns IDs of actions in the Running phase (for heartbeat).
func (t *Tracker) RunningIDs() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var ids []string
	for id, ta := range t.actions {
		if ta.Phase == PhaseRunning {
			ids = append(ids, id)
		}
	}
	return ids
}

// PendingAckActions returns all actions in PendingAck phase.
// Used by WSClient on reconnect to replay unacked results.
func (t *Tracker) PendingAckActions() []*shared.Action {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []*shared.Action
	for _, ta := range t.actions {
		if ta.Phase == PhasePendingAck {
			out = append(out, ta.Action)
		}
	}
	return out
}

// DueCleanup returns actions in PendingCleanup phase whose AckedAt is older
// than the given grace period.
func (t *Tracker) DueCleanup(grace time.Duration) []*shared.Action {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()
	var out []*shared.Action
	for _, ta := range t.actions {
		if ta.Phase == PhasePendingCleanup && now.Sub(ta.AckedAt) > grace {
			out = append(out, ta.Action)
		}
	}
	return out
}

// ToStatusResponse builds an ActionStatusResponse for the /action_status API.
func (t *Tracker) ToStatusResponse(id string) *shared.ActionStatusResponse {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ta, ok := t.actions[id]
	if !ok {
		return &shared.ActionStatusResponse{Found: false, ActionID: id}
	}
	resp := &shared.ActionStatusResponse{
		Found:    true,
		ActionID: id,
		Status:   ta.Action.Status,
		StartedAt: ta.Action.StartedAt,
	}
	if ta.Phase != PhaseRunning {
		resp.ExitCode = ta.Action.ContainerExitCode
		resp.ExitReason = ta.Action.ContainerExitReason
		resp.FinishedAt = ta.Action.FinishedAt
	}
	return resp
}
