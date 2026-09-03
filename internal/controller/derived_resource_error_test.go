package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

type rejectPVCClient struct {
	client.Client
}

func (c rejectPVCClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		return apierrors.NewInvalid(
			schema.GroupKind{Kind: "PersistentVolumeClaim"},
			obj.GetName(),
			field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), obj.GetName(), "rejected by Kubernetes validation")},
		)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestDerivedResourceInvalidIsReportedOnMirror(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status.WorkPVC = "smoke-sync"
	mirror.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{StartedAt: timePtr(time.Unix(1788393600, 0))}
	scheme := testScheme(t)
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).
		WithObjects(mirror).
		Build()
	recorder := record.NewFakeRecorder(1)
	reconciler := &MirrorReconciler{
		Client:      rejectPVCClient{Client: baseClient},
		Scheme:      scheme,
		Recorder:    recorder,
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile rejected child: %v", err)
	}
	current := getMirror(t, ctx, baseClient, request.NamespacedName)
	condition := findCondition(current.Status.Conditions, conditionDegraded)
	if condition == nil || condition.Status != "True" || condition.Reason != derivedResourceInvalid {
		t.Fatalf("expected DerivedResourceInvalid degradation, got %#v", current.Status)
	}
	if !strings.Contains(condition.Message, "PersistentVolumeClaim") || !strings.Contains(condition.Message, "smoke-sync") {
		t.Fatalf("expected the API server rejection in the condition, got %q", condition.Message)
	}
	waitForEvent(t, recorder, derivedResourceInvalid)
}
