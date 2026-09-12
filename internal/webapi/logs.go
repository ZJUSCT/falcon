package webapi

import (
	"context"
	"encoding/json"
	"errors"
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

// errJobNotRetained marks an explicit ?job= selection that resolves to no
// retained Job (pruned, foreign, or mistyped); it maps to a 404.
var errJobNotRetained = errors.New("job not retained")

// logStateRunning names the shared running state of containers and Jobs.
const logStateRunning = "running"

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

// LogJob is one retained sync Job in the history list (newest first). The
// selector's default is the current synchronization's Job, else the newest
// retained one; ?job=<name> selects any other entry.
type LogJob struct {
	Name       string       `json:"name"`
	UID        string       `json:"uid"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	Result     string       `json:"result,omitempty"` // pending | running | succeeded | failed
	Current    bool         `json:"current"`
}
type LogSource struct {
	Job       string       `json:"job,omitempty"`
	JobUID    string       `json:"jobUID,omitempty"`
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	Current   bool         `json:"current"`
	Phase     string       `json:"phase,omitempty"`
	Message   string       `json:"message,omitempty"`
	Jobs      []LogJob     `json:"jobs"`
	Pods      []LogPod     `json:"pods"`
}

// logJobResult maps a Job's status to the selection vocabulary.
func logJobResult(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		switch {
		case condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue:
			return "succeeded"
		case condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue:
			return "failed"
		}
	}
	if job.Status.StartTime != nil {
		return logStateRunning
	}
	return "pending"
}

// logJobFinished reports a terminal Job's finish time: completionTime when
// succeeded, the JobFailed condition transition otherwise.
func logJobFinished(job *batchv1.Job) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() {
			return &condition.LastTransitionTime
		}
	}
	return nil
}

// Resolve from owned Jobs, not lastSync: cancelled Jobs may already be gone.
// UID checks exclude stale children from a deleted/recreated Mirror or Job.
// selected names the requested Job ("" picks the current synchronization's
// Job, else the newest retained one).
func (s *Server) syncLogSource(ctx context.Context, name, selected string) (*LogSource, error) {
	m := &mirrorv1alpha1.Mirror{}
	if err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, m); err != nil {
		return nil, err
	}
	source := &LogSource{Jobs: []LogJob{}, Pods: []LogPod{}}
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
	// Newest first, so the history list and the default selection agree.
	sort.Slice(jobs.Items, func(i, j int) bool {
		return jobs.Items[i].CreationTimestamp.After(jobs.Items[j].CreationTimestamp.Time) ||
			jobs.Items[i].CreationTimestamp.Equal(&jobs.Items[j].CreationTimestamp) && jobs.Items[i].Name > jobs.Items[j].Name
	})
	var owned []*batchv1.Job
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if !metav1.IsControlledBy(j, m) {
			continue
		}
		owned = append(owned, j)
		source.Jobs = append(source.Jobs, LogJob{
			Name:       j.Name,
			UID:        string(j.UID),
			StartedAt:  j.Status.StartTime,
			FinishedAt: logJobFinished(j),
			Result:     logJobResult(j),
			Current:    j.Name == currentName,
		})
	}
	var chosen *batchv1.Job
	for _, j := range owned {
		if selected != "" && j.Name == selected {
			chosen = j
			break
		}
	}
	if chosen == nil && selected != "" {
		// An explicit selection must resolve; silently falling back to the
		// default would switch logs under the user.
		return nil, fmt.Errorf("%w: %q is not retained for mirror %s", errJobNotRetained, selected, name)
	}
	if chosen == nil {
		// Default selection: the current synchronization's Job, else the
		// newest retained one (e.g. the current transaction has no Job yet).
		for _, j := range owned {
			if j.Name == currentName {
				chosen = j
				break
			}
		}
		if chosen == nil && len(owned) > 0 {
			chosen = owned[0]
		}
	}
	if chosen != nil {
		source.Current = chosen.Name == currentName && currentName != ""
	}
	if chosen == nil {
		source.Message = "No retained synchronization Job is available."
		if m.Status.CurrentSync != nil {
			source.Message = "The current synchronization has no Job yet; no retained logs are available."
		} else if m.Status.LastAttempt == nil {
			source.Message = "This mirror has not synchronized yet."
		}
		return source, nil
	}
	source.Job = chosen.Name
	source.JobUID = string(chosen.UID)
	source.StartedAt = chosen.Status.StartTime
	if selected == "" && !source.Current && m.Status.CurrentSync != nil {
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
		if !metav1.IsControlledBy(p, chosen) {
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
				return logStateRunning
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
	// ?job= selects a retained Job by name on both endpoints (the stream
	// re-resolves the selection so its jobUID guard matches the choice).
	selected := r.URL.Query().Get("job")
	if selected != "" && strings.Contains(selected, "/") {
		writeJSONError(w, 400, "invalid job name")
		return
	}
	source, err := s.syncLogSource(r.Context(), name, selected)
	if err != nil {
		if errors.Is(err, errJobNotRetained) {
			writeJSONError(w, 404, err.Error())
			return
		}
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
	stream, err := s.LogStream(ctx, s.Namespace, pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &lines, LimitBytes: &limit, Follow: follow && container.State == logStateRunning, Timestamps: timestamps})
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
