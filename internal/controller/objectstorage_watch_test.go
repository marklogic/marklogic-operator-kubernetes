// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package controller

import (
	"context"
	"reflect"
	"sort"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func testCluster(namespace, name string, aws, azure string) *marklogicv1.MarklogicCluster {
	cluster := &marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if aws != "" || azure != "" {
		cluster.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{}
		if aws != "" {
			cluster.Spec.ObjectStorage.AWS = &marklogicv1.AWSObjectStorage{SecretName: aws}
		}
		if azure != "" {
			cluster.Spec.ObjectStorage.Azure = &marklogicv1.AzureObjectStorage{SecretName: azure}
		}
	}
	return cluster
}

func TestClusterPredicateUpdateFiltering(t *testing.T) {
	t.Parallel()
	update := markLogicClusterCreateUpdateDeletePredicate().Update

	withAnnotations := func(annotations map[string]string) *marklogicv1.MarklogicCluster {
		cluster := testCluster("ns", "c", "aws", "")
		cluster.Annotations = annotations
		return cluster
	}

	tests := []struct {
		name string
		old  *marklogicv1.MarklogicCluster
		new  *marklogicv1.MarklogicCluster
		want bool
	}{
		{"identical", withAnnotations(nil), withAnnotations(nil), false},
		{"nil versus empty annotations", withAnnotations(nil), withAnnotations(map[string]string{}), false},
		{"banzaicloud tracking annotation only", withAnnotations(map[string]string{"banzaicloud.com/last-applied": "a"}), withAnnotations(map[string]string{"banzaicloud.com/last-applied": "b"}), false},
		{"kubectl tracking annotation only", withAnnotations(nil), withAnnotations(map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}), false},
		{"user annotation", withAnnotations(map[string]string{"example.com/x": "1"}), withAnnotations(map[string]string{"example.com/x": "2"}), true},
		{"reconcile-request annotation", withAnnotations(nil), withAnnotations(map[string]string{"marklogic.progress.com/reconcile-request": "1"}), true},
		{"label change", testCluster("ns", "c", "aws", ""), func() *marklogicv1.MarklogicCluster {
			c := testCluster("ns", "c", "aws", "")
			c.Labels = map[string]string{"a": "b"}
			return c
		}(), true},
		{"spec change", testCluster("ns", "c", "aws", ""), testCluster("ns", "c", "aws2", ""), true},
		{"status only", testCluster("ns", "c", "aws", ""), func() *marklogicv1.MarklogicCluster {
			c := testCluster("ns", "c", "aws", "")
			c.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: &marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseApplied}}
			return c
		}(), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldCopy, newCopy := test.old.DeepCopy(), test.new.DeepCopy()
			if got := update(event.UpdateEvent{ObjectOld: test.old, ObjectNew: test.new}); got != test.want {
				t.Fatalf("Update = %v, want %v", got, test.want)
			}
			if !reflect.DeepEqual(oldCopy, test.old) || !reflect.DeepEqual(newCopy, test.new) {
				t.Fatalf("the predicate must treat informer objects as read-only")
			}
		})
	}
}

func TestClusterPredicateKeepsOwnedGroupFiltering(t *testing.T) {
	t.Parallel()
	predicate := markLogicClusterCreateUpdateDeletePredicate()
	group := &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
	changed := group.DeepCopy()
	changed.Spec.Replicas = new(int32)

	if predicate.Update(event.UpdateEvent{ObjectOld: group, ObjectNew: changed}) {
		t.Fatalf("owned MarklogicGroup updates stay filtered")
	}
	if !predicate.Create(event.CreateEvent{Object: group}) || !predicate.Delete(event.DeleteEvent{Object: group}) {
		t.Fatalf("owned MarklogicGroup create/delete stay admitted")
	}
}

func TestSecretRevisionPredicate(t *testing.T) {
	t.Parallel()
	predicate := secretRevisionPredicate()
	secret := func(revision string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", ResourceVersion: revision}}
	}

	if !predicate.Update(event.UpdateEvent{ObjectOld: secret("1"), ObjectNew: secret("2")}) {
		t.Fatalf("every resourceVersion change, including metadata-only updates, must be admitted")
	}
	if predicate.Update(event.UpdateEvent{ObjectOld: secret("2"), ObjectNew: secret("2")}) {
		t.Fatalf("resyncs with an unchanged revision are not events")
	}
	if !predicate.Create(event.CreateEvent{Object: secret("1")}) || !predicate.Delete(event.DeleteEvent{Object: secret("1")}) {
		t.Fatalf("Secret create and delete must be admitted")
	}
	if predicate.Generic(event.GenericEvent{Object: secret("1")}) {
		t.Fatalf("generic events are ignored")
	}
}

func TestObjectStorageSecretNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cluster *marklogicv1.MarklogicCluster
		want    []string
	}{
		{"none", testCluster("ns", "c", "", ""), nil},
		{"aws only", testCluster("ns", "c", "a", ""), []string{"a"}},
		{"azure only", testCluster("ns", "c", "", "z"), []string{"z"}},
		{"separate sorted", testCluster("ns", "c", "z", "a"), []string{"a", "z"}},
		{"shared is deduplicated", testCluster("ns", "c", "shared", "shared"), []string{"shared"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := objectStorageSecretNames(test.cluster)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("names = %v, want %v", got, test.want)
			}
		})
	}
	if objectStorageSecretNames(&corev1.Secret{}) != nil {
		t.Fatalf("non-cluster objects index nothing")
	}
}

func TestSecretToClustersMapsOnlyReferencingClustersInNamespace(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&marklogicv1.MarklogicCluster{}, objectStorageSecretIndex, objectStorageSecretNames).
		WithObjects(
			testCluster("ns-a", "aws-only", "creds", ""),
			testCluster("ns-a", "azure-only", "", "creds"),
			testCluster("ns-a", "shared", "creds", "creds"),
			testCluster("ns-a", "other-secret", "unrelated", ""),
			testCluster("ns-a", "none", "", ""),
			testCluster("ns-b", "same-name-other-namespace", "creds", ""),
		).
		Build()
	reconciler := &MarklogicClusterReconciler{Client: c}

	requests := reconciler.secretToClusters(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns-a"}})
	var got []types.NamespacedName
	for _, request := range requests {
		got = append(got, request.NamespacedName)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].String() < got[j].String() })
	want := []types.NamespacedName{{Namespace: "ns-a", Name: "aws-only"}, {Namespace: "ns-a", Name: "azure-only"}, {Namespace: "ns-a", Name: "shared"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued %v, want %v (each referencing cluster exactly once, none from other namespaces)", got, want)
	}

	if unreferenced := reconciler.secretToClusters(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "nobody", Namespace: "ns-a"}}); len(unreferenced) != 0 {
		t.Fatalf("an unreferenced Secret enqueues nothing, got %v", unreferenced)
	}
	if foreign := reconciler.secretToClusters(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns-c"}}); len(foreign) != 0 {
		t.Fatalf("a Secret in a namespace without clusters enqueues nothing, got %v", foreign)
	}
}
