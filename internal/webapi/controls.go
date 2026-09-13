package webapi

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

type mirrorAction struct {
	Action string `json:"action"`
}

// Mutations require a signed administrator session and an exact HTTPS origin.
// JSON-only bodies additionally exclude cross-site HTML form submissions.
func (s *Server) handleMirrorAction(w http.ResponseWriter, r *http.Request) {
	if s.Writer == nil || s.APIReader == nil || s.Namespace == "" {
		writeJSONError(w, 503, "mirror administration is unavailable")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSONError(w, 415, "application/json is required")
		return
	}
	if r.Header.Get("Origin") != "https://"+r.Host {
		writeJSONError(w, 403, "same-origin request required")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/mirrors/")
	if !strings.HasSuffix(name, "/actions") {
		writeJSONError(w, 404, "not found")
		return
	}
	name = strings.TrimSuffix(name, "/actions")
	if name == "" || strings.Contains(name, "/") {
		writeJSONError(w, 404, "not found")
		return
	}
	var action mirrorAction
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&action); err != nil {
		writeJSONError(w, 400, "invalid action")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSONError(w, 400, "invalid action")
		return
	}
	m := &mirrorv1alpha1.Mirror{}
	if err := s.APIReader.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: name}, m); err != nil {
		writeActionError(w, err)
		return
	}
	if !m.DeletionTimestamp.IsZero() {
		writeJSONError(w, 409, "mirror is being deleted")
		return
	}
	before := m.DeepCopy()
	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	switch action.Action {
	case "pause", "resume":
		m.SetSyncPaused(action.Action == "pause")
		err = s.Writer.Patch(r.Context(), m, patch)
	case "sync", "abort":
		key := mirrorv1alpha1.SyncRequestAnnotation
		if action.Action == "abort" {
			key = mirrorv1alpha1.AbortRequestAnnotation
		}
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[key] = "true"
		err = s.Writer.Patch(r.Context(), m, patch)
	default:
		writeJSONError(w, 400, "unknown mirror action")
		return
	}
	if err != nil {
		writeActionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mirrorJobEntry(m))
}

func writeActionError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeJSONError(w, 404, "mirror not found")
	case apierrors.IsConflict(err):
		writeJSONError(w, 409, "mirror changed; refresh and retry the action")
	default:
		writeJSONError(w, 500, err.Error())
	}
}
