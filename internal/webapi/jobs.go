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

// JobEntry is the wire shape of GET /api/jobs.
//
// Field names are kept compatible with the legacy GET /api/jobs response
// (legacy-docker branch shared.Job) wherever the semantics carry over:
//
//	id, status, updated_at, last_success_at, last_failure_at,
//	last_attempt_at, next_attempt_at, last_action_status, actions
//
// Deliberate adaptations relative to the legacy shape:
//
//   - There is no scheduler queue/concurrency/worker machinery anymore
//     (the controller starts a Job immediately when nextSyncAt is due), so the
//     legacy "Scheduled" status never occurs: Ready/idle maps to "Waiting".
//   - "Orphan" has no meaning anymore (repo config and job record are the same
//     CR) and never occurs.
//   - "actions" (list of the last 100 action IDs) has no equivalent: there is
//     no action history store. The field is kept (always empty) so old UI code
//     can still index it.
//   - "updated_at" is best-effort: the controller does not track when the
//     status was last mutated, so an in-flight sync uses currentSync.startedAt;
//     otherwise it uses the last completed sync's finish time (falling back to
//     that sync's start time).
//   - Timestamps are zero (0001-01-01T00:00:00Z) when unknown — the legacy UI
//     already special-cased the zero time for last_success_at.
//
// New fields (not in the legacy shape, prefixed documentation here):
//
//	kind, namespace, phase, active_pvc, last_finished_at
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
	Kind           string    `json:"kind"` // "Mirror" or "ProxyMirror"
	Namespace      string    `json:"namespace,omitempty"`
	Phase          string    `json:"phase"` // presentation state derived from conditions/currentSync
	ActivePVC      string    `json:"active_pvc,omitempty"`
	LastFinishedAt time.Time `json:"last_finished_at"`
}

// legacyStatusForMirrorPhase maps the derived presentation phase onto the
// legacy job status vocabulary. See JobEntry for the rationale.
func legacyStatusForMirrorPhase(phase string) string {
	switch phase {
	case mirrorv1alpha1.PhaseSyncing, mirrorv1alpha1.PhasePublishing, mirrorv1alpha1.PhaseInitializing:
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
	if m.Status.CurrentSync != nil {
		if degraded != nil && degraded.Status == metav1.ConditionTrue && (progressing == nil || progressing.Status != metav1.ConditionTrue) {
			return mirrorv1alpha1.PhaseDegraded
		}
		if progressing != nil && progressing.Status == metav1.ConditionTrue {
			switch progressing.Reason {
			case "SynchronizationStarted", "SyncQueued":
				return mirrorv1alpha1.PhaseInitializing
			case "Snapshotting", "PublishRollout":
				return mirrorv1alpha1.PhasePublishing
			case "SyncJobRunning":
				return mirrorv1alpha1.PhaseSyncing
			}
		}
		return mirrorv1alpha1.PhaseSyncing
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
		Status:        legacyStatusForMirrorPhase(phase),
		Kind:          "Mirror",
		Namespace:     m.Namespace,
		Phase:         phase,
		ActivePVC:     m.Status.ActivePVC,
		Actions:       []string{}, // legacy field, no action history anymore
		NextAttemptAt: timeOrZero(m.Status.NextSyncAt),
	}
	if current := m.Status.CurrentSync; current != nil {
		started := timeOrZero(current.StartedAt)
		entry.LastAttemptAt = started
		entry.UpdatedAt = started
		entry.LastActionStatus = "Running"
	}
	if last := m.Status.LastSync; last != nil {
		started := timeOrZero(last.StartedAt)
		finished := timeOrZero(last.FinishedAt)
		if m.Status.CurrentSync == nil {
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
		ID:        p.Name,
		Status:    phase,
		Kind:      "ProxyMirror",
		Namespace: p.Namespace,
		Phase:     phase,
		Actions:   []string{},
	}
}

func timeOrZero(t *metav1.Time) time.Time {
	if t == nil || t.IsZero() {
		return time.Time{}
	}
	return t.Time
}
