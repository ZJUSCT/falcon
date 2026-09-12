package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// publishDeploymentFailure projects explicit Kubernetes rollout failures without
// aborting publication. Kubernetes can recover the same Deployment after repair.
// Old-generation conditions and objects owned by someone else are not evidence
// about this owner's requested publication.
func publishDeploymentFailure(ctx context.Context, c client.Client, owner client.Object, protocols ...string) (*conditionFailure, error) {
	for _, protocol := range protocols {
		deployment := &appsv1.Deployment{}
		key := client.ObjectKey{Namespace: owner.GetNamespace(), Name: publishChildName(childBase(owner.GetName()), protocol)}
		if err := c.Get(ctx, key, deployment); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if !metav1.IsControlledBy(deployment, owner) || deployment.Status.ObservedGeneration != deployment.Generation {
			continue
		}
		for _, condition := range deployment.Status.Conditions {
			failed := condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue
			stalled := condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse && condition.Reason == "ProgressDeadlineExceeded"
			if failed || stalled {
				return &conditionFailure{
					reason:  "PublishDeploymentFailed",
					message: fmt.Sprintf("Deployment %s: %s: %s", deployment.Name, condition.Reason, condition.Message),
				}, nil
			}
		}
	}
	return nil, nil
}

func mirrorPublishProtocols(mirror *mirrorv1alpha1.Mirror) []string {
	var protocols []string
	// A redirect-mode http key deploys nothing, so there is no Deployment
	// whose rollout could fail.
	if mirror.Spec.Publish.HTTP.Serving() {
		protocols = append(protocols, PublishProtocolHTTP)
	}
	if mirror.Spec.Publish.Rsync != nil {
		protocols = append(protocols, PublishProtocolRsync)
	}
	return protocols
}
