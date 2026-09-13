package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func logsFixture(t *testing.T) (*Server, client.Client, *mirrorv1alpha1.Mirror, *batchv1.Job, *corev1.Pod) {
	t.Helper()
	m := &mirrorv1alpha1.Mirror{ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors", UID: "mirror"}}
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: &metav1.Time{Time: time.Unix(100, 0)}, Phase: "Running"}
	s, c := controlServer(t, m)
	controlled := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "debian-sync-100", Namespace: m.Namespace, UID: "job", CreationTimestamp: metav1.NewTime(time.Unix(100, 0)), Labels: map[string]string{"mirrors.zjusct.io/mirror": m.Name, "app.kubernetes.io/component": "sync"}, OwnerReferences: []metav1.OwnerReference{{UID: m.UID, Name: m.Name, Kind: "Mirror", APIVersion: mirrorv1alpha1.GroupVersion.String(), Controller: &controlled}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: m.Namespace, UID: "pod", OwnerReferences: []metav1.OwnerReference{{UID: job.UID, Name: job.Name, Kind: "Job", APIVersion: "batch/v1", Controller: &controlled}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sync"}}, InitContainers: []corev1.Container{{Name: "prepare"}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "sync", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}, InitContainerStatuses: []corev1.ContainerStatus{{Name: "prepare", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}}}
	for _, o := range []client.Object{job, pod} {
		if err := c.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	return s, c, m, job, pod
}
func logRequest(s *Server, path string, auth bool, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if auth {
		req.AddCookie(&http.Cookie{Name: "falcon_session", Value: s.Auth.cookieValue(1)})
	}
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, req)
	return w
}
func TestSyncLogSourceOwnershipAndFallback(t *testing.T) {
	s, c, m, job, pod := logsFixture(t)
	newer := job.DeepCopy()
	newer.Name = "debian-sync-200"
	newer.UID = "new-job"
	newer.ResourceVersion = ""
	newer.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	if err := c.Create(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	unrelated := newer.DeepCopy()
	unrelated.Name = "debian-sync-300"
	unrelated.UID = "foreign"
	unrelated.ResourceVersion = ""
	unrelated.CreationTimestamp = metav1.NewTime(time.Unix(300, 0))
	unrelated.OwnerReferences[0].UID = "old-mirror"
	if err := c.Create(t.Context(), unrelated); err != nil {
		t.Fatal(err)
	}
	wrongPod := pod.DeepCopy()
	wrongPod.Name = "foreign-pod"
	wrongPod.UID = "foreign-pod"
	wrongPod.ResourceVersion = ""
	wrongPod.OwnerReferences[0].UID = "old-job"
	if err := c.Create(t.Context(), wrongPod); err != nil {
		t.Fatal(err)
	}
	source, err := s.syncLogSource(t.Context(), m.Name, "")
	if err != nil || source.JobUID != "job" || !source.Current || len(source.Pods) != 1 || len(source.Pods[0].Containers) != 2 || !source.Pods[0].Containers[1].Init {
		t.Fatalf("current source: %#v %v", source, err)
	}
	if len(source.Jobs) != 2 || source.Jobs[0].Name != "debian-sync-200" || source.Jobs[1].Name != "debian-sync-100" || !source.Jobs[1].Current || source.Jobs[0].Current {
		t.Fatalf("history must list owned Jobs newest-first with the current flag: %#v", source.Jobs)
	}
	if err := c.Delete(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	source, err = s.syncLogSource(t.Context(), m.Name, "")
	if err != nil || source.JobUID != "new-job" || source.Current || len(source.Pods) != 0 {
		t.Fatalf("fallback source: %#v %v", source, err)
	}
	m.Status.CurrentSync = nil
	if err := c.Status().Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	source, err = s.syncLogSource(t.Context(), m.Name, "")
	if err != nil || source.Job != "" {
		t.Fatalf("foreign Job exposed: %#v %v", source, err)
	}
}

// TestSyncLogSourceJobSelection: ?job= selects any retained Job explicitly;
// a selection that resolves to nothing is an error instead of a silent
// fallback, and the stream endpoint resolves the same selection for its
// jobUID guard.
func TestSyncLogSourceJobSelection(t *testing.T) {
	s, c, m, job, pod := logsFixture(t)
	s.LogStream = func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("history\n")), nil
	}
	history := job.DeepCopy()
	history.Name = "debian-sync-50"
	history.UID = "old-job"
	history.ResourceVersion = ""
	history.CreationTimestamp = metav1.NewTime(time.Unix(50, 0))
	history.Status = batchv1.JobStatus{StartTime: &metav1.Time{Time: time.Unix(50, 0)}, CompletionTime: &metav1.Time{Time: time.Unix(60, 0)}, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Unix(60, 0))}}}
	if err := c.Create(t.Context(), history); err != nil {
		t.Fatal(err)
	}
	historyPod := pod.DeepCopy()
	historyPod.Name = "writer-old"
	historyPod.UID = "old-pod"
	historyPod.ResourceVersion = ""
	historyPod.OwnerReferences[0].UID = history.UID
	historyPod.OwnerReferences[0].Name = history.Name
	if err := c.Create(t.Context(), historyPod); err != nil {
		t.Fatal(err)
	}

	source, err := s.syncLogSource(t.Context(), m.Name, "debian-sync-50")
	if err != nil || source.JobUID != "old-job" || source.Current || len(source.Pods) != 1 || source.Pods[0].Name != "writer-old" {
		t.Fatalf("explicit selection: %#v %v", source, err)
	}
	if source.Jobs[1].Result != "succeeded" || source.Jobs[1].FinishedAt == nil {
		t.Fatalf("history entries must carry outcome and finish time: %#v", source.Jobs)
	}
	if _, err = s.syncLogSource(t.Context(), m.Name, "debian-sync-999"); !errors.Is(err, errJobNotRetained) {
		t.Fatalf("unresolved selection must fail explicitly, got %v", err)
	}

	// The stream endpoint re-resolves the selection, so a pod of the DEFAULT
	// job is rejected while the selected job's pod streams.
	defaultPodQuery := url.Values{"job": {"debian-sync-100"}, "jobUID": {"job"}, "pod": {"writer"}, "podUID": {"pod"}, "container": {"sync"}, "lines": {"100"}, "follow": {"false"}, "timestamps": {"true"}}
	if w := logRequest(s, "/api/mirrors/debian/logs/stream?"+defaultPodQuery.Encode(), true, "admin.example.org"); w.Code != 200 {
		t.Fatalf("selected job's pod must stream: %d %s", w.Code, w.Body)
	}
	foreignQuery := url.Values{"job": {"debian-sync-50"}, "jobUID": {"old-job"}, "pod": {"writer"}, "podUID": {"pod"}, "container": {"sync"}, "lines": {"100"}, "follow": {"false"}, "timestamps": {"true"}}
	if w := logRequest(s, "/api/mirrors/debian/logs/stream?"+foreignQuery.Encode(), true, "admin.example.org"); w.Code != 404 {
		t.Fatalf("a pod of the unselected job must be rejected: %d %s", w.Code, w.Body)
	}
	if w := logRequest(s, "/api/mirrors/debian/logs?job=debian-sync-999", true, "admin.example.org"); w.Code != 404 {
		t.Fatalf("unresolved HTTP selection must 404: %d %s", w.Code, w.Body)
	}
}
func TestSyncLogStreamValidationAndFrames(t *testing.T) {
	s, _, _, _, _ := logsFixture(t)
	calls := 0
	s.LogStream = func(_ context.Context, ns, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
		calls++
		if ns != "mirrors" || pod != "writer" || o.Container != "prepare" || o.Follow || *o.TailLines != 100 || !o.Timestamps || *o.LimitBytes != 2*1024*1024 {
			t.Errorf("unsafe log request: %s %s %#v", ns, pod, o)
		}
		return io.NopCloser(strings.NewReader("中文\n<script>alert(1)</script>\n")), nil
	}
	q := url.Values{"jobUID": {"job"}, "pod": {"writer"}, "podUID": {"pod"}, "container": {"prepare"}, "lines": {"100"}, "follow": {"true"}, "timestamps": {"true"}}
	path := "/api/mirrors/debian/logs/stream?"
	for _, tc := range []struct {
		key, value string
		want       int
	}{{"jobUID", "stale", 409}, {"podUID", "foreign", 404}, {"container", "foreign", 400}, {"lines", "2501", 400}, {"follow", "invalid", 400}, {"timestamps", "invalid", 400}} {
		t.Run(tc.key, func(t *testing.T) {
			bad := url.Values{}
			for k, v := range q {
				bad[k] = append([]string(nil), v...)
			}
			bad.Set(tc.key, tc.value)
			w := logRequest(s, path+bad.Encode(), true, "admin.example.org")
			if w.Code != tc.want {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	for _, tc := range []struct {
		auth bool
		host string
		want int
	}{{false, "admin.example.org", 401}, {false, "public.example.org", 401}} {
		w := logRequest(s, path+q.Encode(), tc.auth, tc.host)
		if w.Code != tc.want {
			t.Fatalf("unauthorized logs: %d", w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached kubelet")
	}
	w := logRequest(s, path+q.Encode(), true, "admin.example.org")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	decoder := json.NewDecoder(w.Body)
	var data struct{ Data []byte }
	var end struct{ End bool }
	if decoder.Decode(&data) != nil || string(data.Data) != "中文\n<script>alert(1)</script>\n" || decoder.Decode(&end) != nil || !end.End {
		t.Fatal("stream framing corrupted logs")
	}
}

type cancellingLogReader struct {
	ctx    context.Context
	closed chan struct{}
}

func (r *cancellingLogReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func (r *cancellingLogReader) Close() error             { close(r.closed); return nil }
func TestSyncLogDisconnectClosesUpstream(t *testing.T) {
	s, _, _, _, _ := logsFixture(t)
	opened := make(chan struct{})
	closed := make(chan struct{})
	s.LogStream = func(ctx context.Context, _, _ string, _ *corev1.PodLogOptions) (io.ReadCloser, error) {
		close(opened)
		return &cancellingLogReader{ctx, closed}, nil
	}
	server := httptest.NewServer(s.AdminHandler())
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/mirrors/debian/logs/stream?jobUID=job&pod=writer&podUID=pod&container=sync&lines=100&follow=true&timestamps=true", nil)
	req.Host = "admin.example.org"
	req.AddCookie(&http.Cookie{Name: "falcon_session", Value: s.Auth.cookieValue(1)})
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-opened
	cancel()
	_ = response.Body.Close()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream log read survived client disconnect")
	}
}

func TestSyncLogWaitingAndReadErrors(t *testing.T) {
	s, c, _, _, pod := logsFixture(t)
	s.LogStream = func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		return nil, io.ErrUnexpectedEOF
	}
	path := "/api/mirrors/debian/logs/stream?jobUID=job&pod=writer&podUID=pod&container=sync&lines=100&follow=true&timestamps=true"
	w := logRequest(s, path, true, "admin.example.org")
	if w.Code != 502 {
		t.Fatalf("log retrieval error must be explicit: %d", w.Code)
	}
	// Removing a Pod must not leave its identity authorized by an earlier lookup.
	if err := c.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	pod.ResourceVersion = ""
	pod.UID = "replacement"
	pod.Status.ContainerStatuses = nil
	if err := c.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	w = logRequest(s, path, true, "admin.example.org")
	if w.Code != 404 {
		t.Fatalf("recreated Pod accepted old UID: %d", w.Code)
	}
	w = logRequest(s, strings.Replace(path, "podUID=pod", "podUID=replacement", 1), true, "admin.example.org")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "has not started") {
		t.Fatalf("waiting container: %d %s", w.Code, w.Body)
	}
}
