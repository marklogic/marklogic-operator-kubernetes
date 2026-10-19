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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestDynamicCredentialRotationCleanupReleasesTerminatingPodFinalizer(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}

	replicas := int32(2)
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "database"},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:      "dynamic",
			Replicas:  &replicas,
			IsDynamic: true,
		},
	}
	labels := getSelectorLabelsByComponent("dynamic", true)
	terminating := credentialRevisionPod("dynamic-0", labels, "old")
	terminating.Namespace = group.Namespace
	terminating.Finalizers = []string{dynamicHostCleanupFinalizer}
	deletionTimestamp := metav1.Now()
	terminating.DeletionTimestamp = &deletionTimestamp
	stable := credentialRevisionPod("dynamic-1", labels, "old")
	stable.Namespace = group.Namespace
	stable.Finalizers = []string{dynamicHostCleanupFinalizer}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminating, stable).Build()
	oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group}

	for reconcileCount := 0; reconcileCount < 5; reconcileCount++ {
		cleanupResult := oc.reconcileDynamicCredentialRotationCleanup()
		if cleanupResult.Completed() {
			_, err := cleanupResult.Output()
			if err != nil {
				t.Fatalf("cleanup reconcile %d returned error: %v", reconcileCount+1, err)
			}
		}
	}

	current := &corev1.Pod{}
	err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(terminating), current)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get terminating pod: %v", err)
	}
	if err == nil {
		for _, finalizer := range current.Finalizers {
			if finalizer == dynamicHostCleanupFinalizer {
				t.Fatal("expected rotation cleanup to release the terminating pod finalizer")
			}
		}
	}

	currentStable := &corev1.Pod{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stable), currentStable); err != nil {
		t.Fatalf("get stable pod: %v", err)
	}
	if !containsString(currentStable.Finalizers, dynamicHostCleanupFinalizer) {
		t.Fatal("expected cleanup finalizer to remain on the non-terminating pod")
	}
}
