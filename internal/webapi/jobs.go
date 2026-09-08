package webapi

import (
	"context"
	"net/http"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// Legacy job status vocabulary of the legacy Docker/SQLite scheduler
// (legacy-docker branch, shared/types.go):
//
//	Waiting / Scheduled / Running / Paused / Orphan
//
// There is no "Failed" state: after a failed attempt the old scheduler put the
// job straight back to Waiting with a new next_attempt_at, so Degraded maps to
// Waiting too.
const (
	legacyStatusWaiting = "Waiting"
	legacyStatusRunning = "Running"
	legacyStatusPaused  = "Paused"
)

// JobEntry is the wire shape of GET /api/jobs. Conditions and SyncPhase expose
// the independent publication observations and sync execution state. Legacy
// status/timestamp fields remain available for the overview and existing clients.
type JobEntry struct {
	// Legacy fields.
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	UpdatedAt        time.Time `json:"updated_at"`
	LastSuccessAt    time.Time `json:"last_success_at"`
	LastFailureAt    time.Time `json:"last_failure_at"`
	LastAttemptAt    time.Time `json:"last_attempt_at"`
	NextAttemptAt    time.Time `json:"next_attempt_at"`
	LastActionStatus string    `json:"last_action_status"`
	Actions          []string  `json:"actions"`

	// New fields.
	Conditions     []metav1.Condition `json:"conditions"`
	SyncPhase      string             `json:"sync_phase,omitempty"`
	Kind           string             `json:"kind"` // "Mirror" or "ProxyMirror"
	Namespace      string             `json:"namespace,omitempty"`
	Phase          string             `json:"phase"` // presentation state derived from conditions/currentSync
	ActivePVC      string             `json:"active_pvc,omitempty"`
	LastFinishedAt time.Time          `json:"last_finished_at"`
	Paused         bool               `json:"paused"`
	SyncBusy       bool               `json:"sync_busy"`
	CanAbort       bool               `json:"can_abort"`
}

// legacyStatusForMirrorPhase maps the derived presentation phase onto the
// legacy job status vocabulary. See JobEntry for the rationale.
func legacyStatusForMirrorPhase(phase string) string {
	switch phase {
	case mirrorv1alpha1.PhaseSyncing, mirrorv1alpha1.PhasePublishing, mirrorv1alpha1.PhaseInitializing, mirrorv1alpha1.SyncPhaseCancelling:
		return legacyStatusRunning
	case mirrorv1alpha1.PhasePaused:
		return legacyStatusPaused
	default:
		// Ready (idle until nextSyncAt), Pending (never synced), Degraded
		// (last attempt failed, retry scheduled) and "" (not yet reconciled)
		// all behave like the legacy Waiting state.
		return legacyStatusWaiting
	}
}

func mirrorPresentationPhase(m *mirrorv1alpha1.Mirror) string {
	progressing := meta.FindStatusCondition(m.Status.Conditions, "Progressing")
	degraded := meta.FindStatusCondition(m.Status.Conditions, "Degraded")
	if current := m.Status.CurrentSync; current != nil {
		if current.Phase == mirrorv1alpha1.SyncPhaseCancelling {
			return mirrorv1alpha1.SyncPhaseCancelling
		}
		if current.Phase == mirrorv1alpha1.SyncPhaseSnapshotting {
			return mirrorv1alpha1.SyncPhaseSnapshotting
		}
		if current.Phase == mirrorv1alpha1.SyncPhasePending {
			if m.Spec.Sync.Paused && m.Status.PausedAt != nil {
				return mirrorv1alpha1.PhasePaused
			}
			return mirrorv1alpha1.PhaseInitializing
		}
		return mirrorv1alpha1.PhaseSyncing
	}
	if m.Status.Publication != nil {
		return mirrorv1alpha1.PhasePublishing
	}

	if m.Spec.Sync.Paused {
		return mirrorv1alpha1.PhasePaused
	}
	if degraded != nil && degraded.Status == metav1.ConditionTrue {
		return mirrorv1alpha1.PhaseDegraded
	}
	if progressing != nil && progressing.Status == metav1.ConditionTrue {
		return mirrorv1alpha1.PhasePublishing
	}
	if readyForCurrentGeneration(m.Status.Conditions, m.Generation) {
		return mirrorv1alpha1.PhaseReady
	}
	return mirrorv1alpha1.PhasePending
}

func proxyPresentationPhase(p *mirrorv1alpha1.ProxyMirror) string {
	if condition := meta.FindStatusCondition(p.Status.Conditions, "Degraded"); condition != nil && condition.Status == metav1.ConditionTrue {
		return mirrorv1alpha1.PhaseDegraded
	}
	if condition := meta.FindStatusCondition(p.Status.Conditions, "Progressing"); condition != nil && condition.Status == metav1.ConditionTrue {
		return mirrorv1alpha1.PhasePublishing
	}
	if readyForCurrentGeneration(p.Status.Conditions, p.Generation) {
		return mirrorv1alpha1.PhaseReady
	}
	return mirrorv1alpha1.PhasePending
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	entries, err := s.listJobs(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) listJobs(ctx context.Context) ([]JobEntry, error) {
	var mirrors mirrorv1alpha1.MirrorList
	if err := s.Client.List(ctx, &mirrors); err != nil {
		return nil, err
	}
	var proxies mirrorv1alpha1.ProxyMirrorList
	if err := s.Client.List(ctx, &proxies); err != nil {
		return nil, err
	}

	entries := make([]JobEntry, 0, len(mirrors.Items)+len(proxies.Items))
	for i := range mirrors.Items {
		entries = append(entries, mirrorJobEntry(&mirrors.Items[i]))
	}
	for i := range proxies.Items {
		entries = append(entries, proxyJobEntry(&proxies.Items[i]))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		if entries[i].Namespace != entries[j].Namespace {
			return entries[i].Namespace < entries[j].Namespace
		}
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

// mirrorJobEntry maps a Mirror CR onto the legacy job shape.
func mirrorJobEntry(m *mirrorv1alpha1.Mirror) JobEntry {
	phase := mirrorPresentationPhase(m)
	entry := JobEntry{
		ID:            m.Name,
		Conditions:    append([]metav1.Condition{}, m.Status.Conditions...),
		SyncPhase:     m.Status.Sync.Phase,
		Status:        legacyStatusForMirrorPhase(phase),
		Kind:          "Mirror",
		Namespace:     m.Namespace,
		Phase:         phase,
		ActivePVC:     m.Status.ActivePVC,
		Actions:       []string{}, // legacy field, no action history anymore
		NextAttemptAt: timeOrZero(m.Status.NextSyncAt),
		Paused:        m.Spec.Sync.Paused,
		SyncBusy:      m.Status.CurrentSync != nil || m.SyncRequested(),
	}
	if m.Spec.Sync.Paused {
		entry.NextAttemptAt = time.Time{}
	}
	if current := m.Status.CurrentSync; current != nil {
		entry.CanAbort = !m.AbortRequested() && (current.Phase == mirrorv1alpha1.SyncPhasePending || current.Phase == mirrorv1alpha1.SyncPhaseRunning)
		started := timeOrZero(current.StartedAt)
		if started.IsZero() {
			started = timeOrZero(current.QueuedAt)
		}
		entry.LastAttemptAt = started
		entry.UpdatedAt = started
		entry.LastActionStatus = current.Phase
	}
	if last := m.Status.LastSync; last != nil {
		started := timeOrZero(last.StartedAt)
		finished := timeOrZero(last.FinishedAt)
		if m.Status.CurrentSync == nil || m.Status.CurrentSync.Phase == mirrorv1alpha1.SyncPhaseSnapshotting {
			entry.LastAttemptAt = started
			entry.UpdatedAt = started
			entry.LastActionStatus = last.Phase
		}
		entry.LastFinishedAt = finished
		switch last.Phase {
		case mirrorv1alpha1.SyncPhaseSucceeded:
			entry.LastSuccessAt = finished
		case mirrorv1alpha1.SyncPhaseFailed:
			entry.LastFailureAt = finished
		}
		// updated_at is best-effort: last known sync finish, else start.
		if m.Status.CurrentSync == nil && !finished.IsZero() {
			entry.UpdatedAt = finished
		}
	}
	return entry
}

// proxyJobEntry maps a ProxyMirror CR onto the job shape. ProxyMirror is a new
// concept with no legacy equivalent: there is no sync job, so every timestamp
// is zero and `status` carries the derived Ready/Pending/Degraded presentation
// state instead of the legacy sync vocabulary — the frontend can branch on
// `kind`.
func proxyJobEntry(p *mirrorv1alpha1.ProxyMirror) JobEntry {
	phase := proxyPresentationPhase(p)
	return JobEntry{
		ID:         p.Name,
		Conditions: append([]metav1.Condition{}, p.Status.Conditions...),
		Status:     phase,
		Kind:       "ProxyMirror",
		Namespace:  p.Namespace,
		Phase:      phase,
		Actions:    []string{},
	}
}

func timeOrZero(t *metav1.Time) time.Time {
	if t == nil || t.IsZero() {
		return time.Time{}
	}
	return t.Time
}
