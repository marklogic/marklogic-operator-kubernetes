// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconsileMarklogicGroupHandlerSkipsServiceCreateWhenDynamicGroupDeleting(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}

	now := metav1.Now()
	group := &marklogicv1.MarklogicGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicGroup"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "dynamic",
			Namespace:         "ml-dynamic-host",
			DeletionTimestamp: &now,
			Finalizers:        []string{dynamicGroupCleanupFinalizer},
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:      "dynamic",
			IsDynamic: true,
			Service: marklogicv1.Service{
				Type: corev1.ServiceTypeClusterIP,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
		WithObjects(group).
		Build()

	oc := &OperatorContext{
		Ctx:            context.Background(),
		Client:         fakeClient,
		Scheme:         scheme,
		MarklogicGroup: group,
		Recorder:       record.NewFakeRecorder(10),
	}

	if _, err := oc.ReconsileMarklogicGroupHandler(); err != nil {
		t.Fatalf("ReconsileMarklogicGroupHandler returned error: %v", err)
	}

	current := &marklogicv1.MarklogicGroup{}
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "dynamic", Namespace: "ml-dynamic-host"}, current)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("failed to fetch MarklogicGroup: %v", err)
		}
	} else {
		for _, finalizer := range current.Finalizers {
			if finalizer == dynamicGroupCleanupFinalizer {
				t.Fatalf("expected dynamic cleanup finalizer to be removed during delete reconcile")
			}
		}
	}

	svc := &corev1.Service{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "dynamic", Namespace: "ml-dynamic-host"}, svc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no service to be created for deleting dynamic group, got err=%v", err)
	}
}
