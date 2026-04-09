package worker

import (
	"errors"
	"testing"

	"github.com/star/mirrorgo/shared"
)

func TestHandleDispatchWSTracksBeforeStart(t *testing.T) {
	prevTracker := tracker
	prevCache := actionCache
	prevOnNewAction := OnNewAction
	prevDryRun := DryRun
	prevStartContainer := startContainer
	t.Cleanup(func() {
		tracker = prevTracker
		actionCache = prevCache
		OnNewAction = prevOnNewAction
		DryRun = prevDryRun
		startContainer = prevStartContainer
	})

	tracker = NewTracker()
	actionCache = NewActionCache(16)
	OnNewAction = func(act *shared.Action) {}
	DryRun = true

	startContainer = func(act *shared.Action) error {
		if !tracker.Has(act.ID) {
			t.Fatalf("action %q was not tracked before container start", act.ID)
		}
		act.ContainerID = "container-1"
		return nil
	}

	ok, msg := HandleDispatchWS(shared.DispatchAction{
		ID:    "action-1",
		JobID: "repo-1",
	})
	if !ok {
		t.Fatalf("HandleDispatchWS returned ok=false: %s", msg)
	}
	if !tracker.Has("action-1") {
		t.Fatalf("expected action to remain tracked after successful start")
	}
}

func TestHandleDispatchWSRemovesTrackingOnStartFailure(t *testing.T) {
	prevTracker := tracker
	prevCache := actionCache
	prevOnNewAction := OnNewAction
	prevDryRun := DryRun
	prevStartContainer := startContainer
	t.Cleanup(func() {
		tracker = prevTracker
		actionCache = prevCache
		OnNewAction = prevOnNewAction
		DryRun = prevDryRun
		startContainer = prevStartContainer
	})

	tracker = NewTracker()
	actionCache = NewActionCache(16)
	OnNewAction = func(act *shared.Action) {}
	DryRun = true

	startContainer = func(act *shared.Action) error {
		return errors.New("boom")
	}

	ok, msg := HandleDispatchWS(shared.DispatchAction{
		ID:    "action-2",
		JobID: "repo-2",
	})
	if ok {
		t.Fatalf("HandleDispatchWS returned ok=true, want false")
	}
	if msg == "" {
		t.Fatalf("expected failure message")
	}
	if tracker.Has("action-2") {
		t.Fatalf("expected failed startup action to be removed from tracker")
	}
}
