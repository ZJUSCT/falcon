package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// PVCUsageReader reports the on-disk usage of one PVC as observed by the
// kubelet of a given node (the usedBytes of the volume bound to that claim in
// the node's stats summary). It backs Mirror status.sizeBytes.
type PVCUsageReader interface {
	// PVCUsedBytes returns the used bytes of the PVC named pvcName in
	// namespace namespace as reported by nodeName's kubelet. ok=false means
	// the PVC (or its usedBytes) is absent from the summary — that is not an
	// error: the kubelet only reports volumes mounted by pods on its node.
	PVCUsedBytes(ctx context.Context, nodeName, namespace, pvcName string) (usedBytes int64, ok bool, err error)
}

// KubeletUsageReader is the production PVCUsageReader: it fetches a node's
// kubelet stats summary through the API server node proxy
// (GET /api/v1/nodes/<node>/proxy/stats/summary; client-go v0.36.1 has no
// typed method for it) and caches one summary per node behind a short TTL —
// a summary covers every pod of the node, so reconciles of Mirrors sharing a
// node must not refetch it.
type KubeletUsageReader struct {
	client kubernetes.Interface
	ttl    time.Duration
	// now is injectable for cache-TTL tests.
	now func() time.Time
	// fetch returns the raw summary body of a node; injectable for tests,
	// production uses the node proxy RESTClient.
	fetch func(ctx context.Context, nodeName string) ([]byte, error)

	mu      sync.Mutex
	entries map[string]statsSummaryEntry
}

type statsSummaryEntry struct {
	summary   *statsv1alpha1.Summary
	fetchedAt time.Time
}

// NewKubeletUsageReader returns a reader caching per-node stats summaries for
// ttl (a minute is plenty: a published PVC's content is immutable, so its
// usage never changes).
func NewKubeletUsageReader(clientset kubernetes.Interface, ttl time.Duration) *KubeletUsageReader {
	reader := &KubeletUsageReader{
		client:  clientset,
		ttl:     ttl,
		now:     time.Now,
		entries: map[string]statsSummaryEntry{},
	}
	reader.fetch = func(ctx context.Context, nodeName string) ([]byte, error) {
		// Bounded so a hung node proxy cannot stall a reconcile.
		return clientset.CoreV1().RESTClient().Get().
			Resource("nodes").
			Name(nodeName).
			SubResource("proxy").
			Suffix("stats", "summary").
			Timeout(10 * time.Second).
			Do(ctx).Raw()
	}
	return reader
}

// PVCUsedBytes implements PVCUsageReader on top of the cached node summary.
func (r *KubeletUsageReader) PVCUsedBytes(ctx context.Context, nodeName, namespace, pvcName string) (int64, bool, error) {
	summary, err := r.summary(ctx, nodeName)
	if err != nil {
		return 0, false, err
	}
	for i := range summary.Pods {
		for _, volume := range summary.Pods[i].VolumeStats {
			ref := volume.PVCRef
			if ref == nil || ref.Namespace != namespace || ref.Name != pvcName {
				continue
			}
			if volume.UsedBytes == nil {
				return 0, false, nil
			}
			return int64(*volume.UsedBytes), true, nil
		}
	}
	return 0, false, nil
}

// summary returns the node's stats summary, served from the cache while it is
// fresh. Failures are not cached: the next call refetches.
func (r *KubeletUsageReader) summary(ctx context.Context, nodeName string) (*statsv1alpha1.Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.entries[nodeName]; ok && r.now().Sub(entry.fetchedAt) < r.ttl {
		return entry.summary, nil
	}
	raw, err := r.fetch(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("fetch stats summary of node %s: %w", nodeName, err)
	}
	// The summary type is registered in no shared scheme, so the response is
	// decoded explicitly instead of via Do(...).Into(...).
	summary := &statsv1alpha1.Summary{}
	if err := json.Unmarshal(raw, summary); err != nil {
		return nil, fmt.Errorf("decode stats summary of node %s: %w", nodeName, err)
	}
	r.entries[nodeName] = statsSummaryEntry{summary: summary, fetchedAt: r.now()}
	return summary, nil
}

// publishPVCUsage best-effort computes the kubelet-reported disk usage of the
// given publish PVC: a running publish pod of the Mirror identifies the node
// whose kubelet sees the mounted PVC, then the UsageReader reads the volume's
// usedBytes from that node's stats summary. Nothing here may disturb
// reconciliation: every miss (UsageReader unset, no running publish pod —
// sync-only mirrors never have one, the summary not reporting the PVC yet) is
// logged and reported as unknown.
func (r *MirrorReconciler) publishPVCUsage(ctx context.Context, mirror *mirrorv1alpha1.Mirror, pvcName string) (int64, bool) {
	if r.UsageReader == nil {
		return 0, false
	}
	logger := log.FromContext(ctx)
	base := childBase(mirror.Name)
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(mirror.Namespace), client.MatchingLabels{MirrorLabel: base}); err != nil {
		logger.Info("publish PVC usage accounting skipped: cannot list publish pods", "mirror", mirror.Name, "error", err.Error())
		return 0, false
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Any running publish pod works: every service entry mounts the same
		// publish PVC, so its node's kubelet reports the volume's usage.
		if !strings.HasPrefix(pod.Labels[ComponentLabel], publishRolePrefix) ||
			pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		size, ok, err := r.UsageReader.PVCUsedBytes(ctx, pod.Spec.NodeName, mirror.Namespace, pvcName)
		if err != nil {
			logger.Info("publish PVC usage accounting skipped", "mirror", mirror.Name, "pvc", pvcName, "node", pod.Spec.NodeName, "error", err.Error())
			return 0, false
		}
		return size, ok
	}
	// Expected for sync-only mirrors (no publish workload at all) and while a
	// fresh publish rollout has not started yet — kept at debug level.
	logger.V(1).Info("publish PVC usage accounting skipped: no running publish pod", "mirror", mirror.Name, "pvc", pvcName)
	return 0, false
}
