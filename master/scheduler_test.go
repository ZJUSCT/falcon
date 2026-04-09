package master

import (
	"testing"
	"time"

	"github.com/star/mirrorgo/shared"
)

func TestDispatchTickCountsReconcilingTowardConcurrency(t *testing.T) {
	s := newTestState(t)
	s.JobQueue.SetMaxConcurrency(3)
	s.Jobs["queued"] = &shared.Job{
		RepoID:    "queued",
		Status:    shared.JobStatusScheduled,
		UpdatedAt: time.Now(),
	}
	s.JobQueue.Enqueue("queued")

	for i := 0; i < 3; i++ {
		id := "active-" + string(rune('a'+i))
		s.ActiveActions[id] = &shared.Action{
			ID:         id,
			JobID:      id,
			Status:     shared.ActionStatusReconciling,
			WorkerName: "worker-1",
			UpdatedAt:  time.Now(),
		}
	}

	s.dispatchTick()

	if got := s.JobQueue.Len(); got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
	if s.Jobs["queued"].Status != shared.JobStatusScheduled {
		t.Fatalf("job status = %q, want %q", s.Jobs["queued"].Status, shared.JobStatusScheduled)
	}
}

func TestSelectWorkerUsesActiveActionLoad(t *testing.T) {
	s := newTestState(t)
	s.WorkerMgr = NewWorkerManager("")
	s.WSHub = NewWSHub("")

	s.WorkerMgr.workers["busy"] = &shared.Worker{
		Name:   "busy",
		Status: shared.WorkerStatusOnline,
	}
	s.WorkerMgr.workers["idle"] = &shared.Worker{
		Name:   "idle",
		Status: shared.WorkerStatusOnline,
	}
	s.WSHub.conns["busy"] = &workerConn{}
	s.WSHub.conns["idle"] = &workerConn{}

	s.ActiveActions["a1"] = &shared.Action{
		ID:         "a1",
		WorkerName: "busy",
		Status:     shared.ActionStatusReconciling,
	}
	s.ActiveActions["a2"] = &shared.Action{
		ID:         "a2",
		WorkerName: "busy",
		Status:     shared.ActionStatusRunning,
	}

	worker := s.selectWorker(&shared.Repo{})
	if worker == nil {
		t.Fatalf("selectWorker returned nil")
	}
	if worker.Name != "idle" {
		t.Fatalf("selected worker = %q, want %q", worker.Name, "idle")
	}
}
