// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery/fake"
	clientfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestPreflightRejectsMissingPermission(t *testing.T) {
	client := clientfake.NewClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false, Reason: "test permission denied"}}, nil
	})
	run := &Run{client: client}
	if err := run.preflight(context.Background(), t, false); err == nil || !strings.Contains(err.Error(), "requires create namespaces") {
		t.Fatalf("expected permission rejection, got %v", err)
	}
}

func TestPreflightSelectsStorageAndRejectsAmbiguousDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, selected string
		defaults       int
		wantErr        bool
	}{{"one default", "", 1, false}, {"ambiguous default", "", 2, true}, {"explicit selection", "second", 2, false}, {"missing class", "missing", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INTEGRATION_OPERATOR_NAMESPACE", "operator")
			t.Setenv("INTEGRATION_OPERATOR_DEPLOYMENT", "controller")
			t.Setenv("INTEGRATION_STORAGE_CLASS", tc.selected)
			policy := corev1.PersistentVolumeReclaimDelete
			objects := []runtime.Object{&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "operator", Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, AvailableReplicas: 1}}}
			for i, name := range []string{"first", "second"} {
				if i < tc.defaults {
					objects = append(objects, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, ReclaimPolicy: &policy})
				}
			}
			client := clientfake.NewClientset(objects...)
			client.Discovery().(*fake.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: "marklogic.progress.com/v1", APIResources: []metav1.APIResource{{Name: "marklogicclusters"}}}}
			client.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
			})
			run := &Run{client: client}
			err := run.preflight(context.Background(), t, true)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected preflight outcome: %v", err)
			}
			if !tc.wantErr {
				want := tc.selected
				if want == "" {
					want = "first"
				}
				if run.StorageClass != want {
					t.Fatalf("selected %q want %q", run.StorageClass, want)
				}
			}
		})
	}
}
