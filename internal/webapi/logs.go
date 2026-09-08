package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

type logFrame struct {
	Data  []byte `json:"data,omitempty"`
	End   bool   `json:"end,omitempty"`
	Error string `json:"error,omitempty"`
}

type LogContainer struct {
	Name  string `json:"name"`
	Init  bool   `json:"init"`
	State string `json:"state"`
}
type LogPod struct {
	Name       string         `json:"name"`
	UID        string         `json:"uid"`
	Containers []LogContainer `json:"containers"`
}
type LogSource struct {
	Job       string       `json:"job,omitempty"`
	JobUID    string       `json:"jobUID,omitempty"`
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	Current   bool         `json:"current"`
	Phase     string       `json:"phase,omitempty"`
	Message   string       `json:"message,omitempty"`
	Pods      []LogPod     `json:"pods"`
}

// Resolve from owned Jobs, not lastSync: cancelled Jobs may already be gone.
// UID checks exclude stale children from a deleted/recreated Mirror or Job.
func (s *Server) syncLogSource(ctx context.Context, name string) (*LogSource, error) {
	m := &mirrorv1alpha1.Mirror{}
	if err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, m); err != nil {
		return nil, err
	}
	source := &LogSource{Pods: []LogPod{}}
	currentName := ""
	if c := m.Status.CurrentSync; c != nil {
		source.Phase = c.Phase
		if c.QueuedAt != nil {
			currentName = fmt.Sprintf("%s-sync-%d", m.Name, c.QueuedAt.Unix())
		}
	}
	jobs := &batchv1.JobList{}
	if err := s.APIReader.List(ctx, jobs, client.InNamespace(s.Namespace), client.MatchingLabels{"mirrors.zjusct.io/mirror": m.Name, "app.kubernetes.io/component": "sync"}); err != nil {
		return nil, err
	}
	var selected *batchv1.Job
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if !metav1.IsControlledBy(j, m) {
			continue
		}
		if j.Name == currentName {
			selected = j
			source.Current = true
			break
		}
		if selected == nil || j.CreationTimestamp.After(selected.CreationTimestamp.Time) || j.CreationTimestamp.Equal(&selected.CreationTimestamp) && j.Name > selected.Name {
			selected = j
		}
	}
	if selected == nil {
		source.Message = "No retained synchronization Job is available."
		if m.Status.CurrentSync != nil {
			source.Message = "The current synchronization has no Job yet; no retained logs are available."
		} else if m.Status.LastAttempt == nil {
			source.Message = "This mirror has not synchronized yet."
		}
		return source, nil
	}
	source.Job = selected.Name
	source.JobUID = string(selected.UID)
	source.StartedAt = selected.Status.StartTime
	if !source.Current && m.Status.CurrentSync != nil {
		source.Message = "The current synchronization has no Job; showing the latest retained Job."
	}
	pods := &corev1.PodList{}
	if err := s.APIReader.List(ctx, pods, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	for i := range pods.Items {
		p := &pods.Items[i]
		if !metav1.IsControlledBy(p, selected) {
			continue
		}
		entry := LogPod{Name: p.Name, UID: string(p.UID), Containers: []LogContainer{}}
		for _, c := range p.Spec.Containers {
			entry.Containers = append(entry.Containers, LogContainer{Name: c.Name, State: logContainerState(c.Name, p.Status.ContainerStatuses)})
		}
		for _, c := range p.Spec.InitContainers {
			entry.Containers = append(entry.Containers, LogContainer{Name: c.Name, Init: true, State: logContainerState(c.Name, p.Status.InitContainerStatuses)})
		}
		source.Pods = append(source.Pods, entry)
	}
	if len(source.Pods) == 0 {
		source.Message = "No Pod logs are available for this Job; its Pods may not exist yet or may have been removed."
	}
	return source, nil
}
func logContainerState(name string, statuses []corev1.ContainerStatus) string {
	for _, s := range statuses {
		if s.Name == name {
			if s.State.Running != nil {
				return "running"
			}
			if s.State.Terminated != nil {
				return "terminated"
			}
		}
	}
	return "waiting"
}

func (s *Server) handleMirrorLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.APIReader == nil || s.Namespace == "" {
		writeJSONError(w, 503, "synchronization logs are unavailable")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/mirrors/")
	streaming := strings.HasSuffix(name, "/logs/stream")
	if streaming {
		name = strings.TrimSuffix(name, "/logs/stream")
	} else {
		name = strings.TrimSuffix(name, "/logs")
	}
	if name == "" || strings.Contains(name, "/") {
		writeJSONError(w, 404, "not found")
		return
	}
	source, err := s.syncLogSource(r.Context(), name)
	if err != nil {
		writeActionError(w, err)
		return
	}
	if !streaming {
		writeJSON(w, 200, source)
		return
	}
	if s.LogStream == nil {
		writeJSONError(w, 503, "synchronization log streaming is unavailable")
		return
	}
	q := r.URL.Query()
	lines, err := strconv.ParseInt(q.Get("lines"), 10, 64)
	if err != nil || lines < 1 || lines > 2500 {
		writeJSONError(w, 400, "lines must be between 1 and 2500")
		return
	}
	follow, err := strconv.ParseBool(q.Get("follow"))
	if err != nil {
		writeJSONError(w, 400, "invalid follow option")
		return
	}
	timestamps, err := strconv.ParseBool(q.Get("timestamps"))
	if err != nil {
		writeJSONError(w, 400, "invalid timestamps option")
		return
	}
	if source.JobUID == "" || q.Get("jobUID") != source.JobUID {
		writeJSONError(w, 409, "log source changed; refresh the selection")
		return
	}
	var pod *LogPod
	for i := range source.Pods {
		p := &source.Pods[i]
		if p.Name == q.Get("pod") && p.UID == q.Get("podUID") {
			pod = p
			break
		}
	}
	if pod == nil {
		writeJSONError(w, 404, "Pod is no longer available for the selected Job")
		return
	}
	var container *LogContainer
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name == q.Get("container") {
			container = c
			break
		}
	}
	if container == nil {
		writeJSONError(w, 400, "container does not belong to the selected Pod")
		return
	}
	if container.State == "waiting" {
		writeJSONError(w, 409, "container has not started; logs are not available yet")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	limit := int64(2 * 1024 * 1024)
	stream, err := s.LogStream(ctx, s.Namespace, pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &lines, LimitBytes: &limit, Follow: follow && container.State == "running", Timestamps: timestamps})
	if err != nil {
		writeJSONError(w, 502, "unable to read container logs: "+err.Error())
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	// Each frame carries bytes (base64 in JSON); the browser incrementally decodes
	// UTF-8, including characters split across reads. Explicit EOF/error frames
	// distinguish a completed stream from a disconnected HTTP connection.
	enc := json.NewEncoder(w)
	flush := http.NewResponseController(w)
	if err := flush.Flush(); err != nil {
		return
	}
	buf := make([]byte, 16*1024)
	for {
		n, readErr := stream.Read(buf)
		if n > 0 {
			if err := enc.Encode(logFrame{Data: buf[:n]}); err != nil {
				return
			}
			if err := flush.Flush(); err != nil {
				return
			}
		}
		if readErr != nil {
			switch {
			case ctx.Err() != nil:
				_ = enc.Encode(logFrame{Error: "Log connection ended; reconnect to continue."})
			case readErr == io.EOF:
				_ = enc.Encode(logFrame{End: true})
			default:
				_ = enc.Encode(logFrame{Error: "Log connection interrupted; reconnect to continue."})
			}
			_ = flush.Flush()
			return
		}
	}
}
