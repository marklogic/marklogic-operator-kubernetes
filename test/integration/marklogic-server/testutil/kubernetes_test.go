// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"encoding/json"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestMarshalKubernetesObjectAddsGroupVersionKind(t *testing.T) {
	testCases := []struct {
		name       string
		object     runtime.Object
		apiVersion string
		kind       string
	}{
		{
			name:       "core secret",
			object:     &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "test-secret"}},
			apiVersion: "v1",
			kind:       "Secret",
		},
		{
			name:       "apps deployment",
			object:     &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-deployment"}},
			apiVersion: "apps/v1",
			kind:       "Deployment",
		},
		{
			name:       "MarkLogic cluster",
			object:     &marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}},
			apiVersion: "marklogic.progress.com/v1",
			kind:       "MarklogicCluster",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			contents, err := marshalKubernetesObject(testCase.object)
			if err != nil {
				t.Fatalf("marshalKubernetesObject returned an error: %v", err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(contents, &manifest); err != nil {
				t.Fatalf("decode manifest: %v", err)
			}
			if manifest["apiVersion"] != testCase.apiVersion {
				t.Fatalf("apiVersion = %q, want %q", manifest["apiVersion"], testCase.apiVersion)
			}
			if manifest["kind"] != testCase.kind {
				t.Fatalf("kind = %q, want %q", manifest["kind"], testCase.kind)
			}
		})
	}
}
