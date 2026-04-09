package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/star/mirrorgo/shared"
)

func TestCleanupDueActionsRemovesAckedAction(t *testing.T) {
	tr := NewTracker()
	act := &shared.Action{ID: "action-1"}
	tr.Add(act, PhasePendingCleanup)
	tr.actions[act.ID].AckedAt = time.Now().Add(-time.Second)

	prevCleanup := cleanupContainer
	cleanupContainer = func(a *shared.Action) error {
		if a.ID != act.ID {
			t.Fatalf("cleanup action id = %q, want %q", a.ID, act.ID)
		}
		return nil
	}
	t.Cleanup(func() {
		cleanupContainer = prevCleanup
	})

	cleanupDueActions(tr)

	if tr.Has(act.ID) {
		t.Fatalf("expected acked action to be removed after cleanup")
	}
}

func TestCleanupDueActionsKeepsActionOnFailure(t *testing.T) {
	tr := NewTracker()
	act := &shared.Action{ID: "action-2"}
	tr.Add(act, PhasePendingCleanup)
	tr.actions[act.ID].AckedAt = time.Now().Add(-time.Second)

	prevCleanup := cleanupContainer
	cleanupContainer = func(a *shared.Action) error {
		return errors.New("boom")
	}
	t.Cleanup(func() {
		cleanupContainer = prevCleanup
	})

	cleanupDueActions(tr)

	if !tr.Has(act.ID) {
		t.Fatalf("expected action to stay tracked when cleanup fails")
	}
}
