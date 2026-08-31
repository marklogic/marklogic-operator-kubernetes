// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package controller

import (
	"context"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func secretWatchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add marklogic scheme: %v", err)
	}
	return scheme
}

func clusterWithAuthSecret(name, namespace, secretName string) *marklogicv1.MarklogicCluster {
	return &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: marklogicv1.MarklogicClusterSpec{
			Auth: &marklogicv1.AdminAuth{SecretName: &secretName},
		},
	}
}

func TestSecretToMarklogicClustersEnqueuesReferencingClusters(t *testing.T) {
	t.Parallel()

	scheme := secretWatchScheme(t)
	reconciler := &MarklogicClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterWithAuthSecret("referencing", "ml", "ml-admin"),
			clusterWithAuthSecret("other-secret", "ml", "unrelated-secret"),
			clusterWithAuthSecret("other-namespace", "elsewhere", "ml-admin"),
		).Build(),
		Scheme: scheme,
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ml-admin", Namespace: "ml"}}
	requests := reconciler.secretToMarklogicClusters(context.Background(), secret)

	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d: %v", len(requests), requests)
	}
	if requests[0].Name != "referencing" || requests[0].Namespace != "ml" {
		t.Fatalf("unexpected request %v", requests[0])
	}
}

// Rotating an object storage provider Secret must wake the controller just as
// the admin auth Secret does, otherwise credential rotation cannot work.
func TestSecretToMarklogicClustersEnqueuesObjectStorageSecrets(t *testing.T) {
	t.Parallel()

	scheme := secretWatchScheme(t)
	cluster := clusterWithAuthSecret("cluster", "ml", "ml-admin")
	cluster.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{
		AWS:   &marklogicv1.AWSObjectStorage{SecretName: "ml-s3-credentials"},
		Azure: &marklogicv1.AzureObjectStorage{SecretName: "ml-azure-credentials"},
	}

	reconciler := &MarklogicClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build(),
		Scheme: scheme,
	}

	for _, secretName := range []string{"ml-admin", "ml-s3-credentials", "ml-azure-credentials"} {
		t.Run(secretName, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "ml"}}
			requests := reconciler.secretToMarklogicClusters(context.Background(), secret)
			if len(requests) != 1 || requests[0].Name != "cluster" {
				t.Fatalf("expected cluster to be enqueued for %q, got %v", secretName, requests)
			}
		})
	}
}

func TestSecretToMarklogicClustersIgnoresUnreferencedSecrets(t *testing.T) {
	t.Parallel()

	scheme := secretWatchScheme(t)
	reconciler := &MarklogicClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterWithAuthSecret("cluster", "ml", "ml-admin"),
		).Build(),
		Scheme: scheme,
	}

	tests := map[string]*corev1.Secret{
		"unrelated secret in same namespace": {ObjectMeta: metav1.ObjectMeta{Name: "some-other-secret", Namespace: "ml"}},
		"same name in another namespace":     {ObjectMeta: metav1.ObjectMeta{Name: "ml-admin", Namespace: "elsewhere"}},
	}

	for name, secret := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if requests := reconciler.secretToMarklogicClusters(context.Background(), secret); len(requests) != 0 {
				t.Fatalf("expected no requests, got %v", requests)
			}
		})
	}
}

func TestSecretToMarklogicClustersIgnoresNonSecretObjects(t *testing.T) {
	t.Parallel()

	scheme := secretWatchScheme(t)
	reconciler := &MarklogicClusterReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme: scheme,
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-a-secret", Namespace: "ml"}}
	if requests := reconciler.secretToMarklogicClusters(context.Background(), pod); requests != nil {
		t.Fatalf("expected nil for non-Secret object, got %v", requests)
	}
}

// The controller applies one predicate to every watch, and its default branch
// drops updates. Without an explicit Secret branch the watch would silently
// never fire on rotation, which is the event it exists to catch.
func TestClusterPredicateAllowsSecretDataChanges(t *testing.T) {
	t.Parallel()

	predicate := markLogicClusterCreateUpdateDeletePredicate()

	tests := []struct {
		name     string
		old      *corev1.Secret
		updated  *corev1.Secret
		expected bool
	}{
		{
			name:     "credential material changed",
			old:      &corev1.Secret{Data: map[string][]byte{"password": []byte("old")}},
			updated:  &corev1.Secret{Data: map[string][]byte{"password": []byte("new")}},
			expected: true,
		},
		{
			name:     "key added",
			old:      &corev1.Secret{Data: map[string][]byte{"password": []byte("same")}},
			updated:  &corev1.Secret{Data: map[string][]byte{"password": []byte("same"), "username": []byte("admin")}},
			expected: true,
		},
		{
			name:     "metadata only",
			old:      &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}, Data: map[string][]byte{"password": []byte("same")}},
			updated:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", Labels: map[string]string{"a": "b"}}, Data: map[string][]byte{"password": []byte("same")}},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := predicate.Update(event.UpdateEvent{ObjectOld: test.old, ObjectNew: test.updated})
			if got != test.expected {
				t.Fatalf("expected %v, got %v", test.expected, got)
			}
		})
	}
}

// Regression guard: adding the Secret branch must not change how the predicate
// treats the resources it already watched.
func TestClusterPredicateStillReactsToSpecChanges(t *testing.T) {
	t.Parallel()

	predicate := markLogicClusterCreateUpdateDeletePredicate()

	unchanged := clusterWithAuthSecret("cluster", "ml", "ml-admin")
	changed := clusterWithAuthSecret("cluster", "ml", "rotated-secret")

	if !predicate.Update(event.UpdateEvent{ObjectOld: unchanged, ObjectNew: changed}) {
		t.Fatal("expected spec change to trigger reconcile")
	}
	if predicate.Update(event.UpdateEvent{ObjectOld: unchanged, ObjectNew: unchanged.DeepCopy()}) {
		t.Fatal("expected identical cluster to be filtered out")
	}
	if predicate.Update(event.UpdateEvent{ObjectOld: &corev1.Pod{}, ObjectNew: &corev1.Pod{}}) {
		t.Fatal("expected unrelated types to remain filtered out")
	}
}
