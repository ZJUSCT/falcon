package controller

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestPublishDeploymentFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		condition  appsv1.DeploymentCondition
		stale      bool
		foreign    bool
		wantFailed bool
	}{
		{name: "deadline", condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}, wantFailed: true},
		{name: "replica failure", condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate"}, wantFailed: true},
		{name: "recovered", condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionFalse}},
		{name: "still progressing", condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue}},
		{name: "old generation", condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue}, stale: true},
		{name: "other owner", condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue}, foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mirror := testMirror()
			owner := metav1.NewControllerRef(mirror, mirrorv1alpha1.GroupVersion.WithKind("Mirror"))
			if tc.foreign {
				owner.UID = "another-owner"
			}
			tc.condition.Message = "upstream diagnostic"
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: "smoke-publish-http", Generation: 2, OwnerReferences: []metav1.OwnerReference{*owner}},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{tc.condition}},
			}
			if tc.stale {
				deployment.Status.ObservedGeneration = 1
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(deployment).Build()
			failure, err := publishDeploymentFailure(t.Context(), c, mirror, PublishProtocolHTTP)
			if err != nil || (failure != nil) != tc.wantFailed {
				t.Fatalf("failure=%#v, err=%v", failure, err)
			}
			if failure != nil && (failure.reason != "PublishDeploymentFailed" || !strings.Contains(failure.message, deployment.Name) || !strings.Contains(failure.message, tc.condition.Reason) || !strings.Contains(failure.message, tc.condition.Message)) {
				t.Fatalf("failure lost Kubernetes context: %#v", failure)
			}
		})
	}
}
