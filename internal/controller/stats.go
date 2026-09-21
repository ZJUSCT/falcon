package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
// typed method for it). Each lookup fetches a fresh summary because kubelet
// may begin reporting a newly mounted PVC shortly after an earlier miss.
type KubeletUsageReader struct {
	// fetch returns the raw summary body of a node; injectable for tests,
	// production uses the node proxy RESTClient.
	fetch func(ctx context.Context, nodeName string) ([]byte, error)
}

// NewKubeletUsageReader returns a reader of kubelet stats summaries.
func NewKubeletUsageReader(clientset kubernetes.Interface) *KubeletUsageReader {
	reader := &KubeletUsageReader{}
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

// PVCUsedBytes implements PVCUsageReader on top of a fresh node summary.
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

// summary fetches and decodes the node's current stats summary.
func (r *KubeletUsageReader) summary(ctx context.Context, nodeName string) (*statsv1alpha1.Summary, error) {
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
	nodes := map[string]struct{}{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !strings.HasPrefix(pod.Labels[ComponentLabel], publishRolePrefix) ||
			pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" ||
			!podUsesPublishPVC(pod, pvcName) {
			continue
		}
		if _, seen := nodes[pod.Spec.NodeName]; seen {
			continue
		}
		nodes[pod.Spec.NodeName] = struct{}{}
		size, ok, err := r.UsageReader.PVCUsedBytes(ctx, pod.Spec.NodeName, mirror.Namespace, pvcName)
		if err != nil {
			logger.Info("publish PVC usage accounting failed on candidate node", "mirror", mirror.Name, "pvc", pvcName, "node", pod.Spec.NodeName, "error", err.Error())
			continue
		}
		if ok {
			return size, true
		}
	}
	// Expected for sync-only mirrors (no publish workload at all) and while a
	// fresh publish rollout or kubelet volume stats has not appeared yet.
	logger.V(1).Info("publish PVC usage accounting unavailable: no eligible node reports the PVC", "mirror", mirror.Name, "pvc", pvcName)
	return 0, false
}

func podUsesPublishPVC(pod *corev1.Pod, pvcName string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == PublishDataVolumeName && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvcName {
			return true
		}
	}
	return false
}
