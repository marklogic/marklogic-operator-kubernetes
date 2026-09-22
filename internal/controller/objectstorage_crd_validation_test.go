/*
Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestObjectStorageCELValidation exercises the CRD's CEL rules against a real
// API server, which is the only place they actually run.
func TestObjectStorageCELValidation(t *testing.T) {
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err := testEnvironment.Start()
	if err != nil {
		t.Skipf("envtest is unavailable, skipping CRD validation: %v", err)
	}
	t.Cleanup(func() { _ = testEnvironment.Stop() })

	if err := marklogicv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}
	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("failed to build client: %v", err)
	}

	tests := []struct {
		name        string
		objectStore *marklogicv1.ObjectStorageConfig
		wantError   string
	}{
		{
			name: "aws secret auth with secretName is accepted",
			objectStore: &marklogicv1.ObjectStorageConfig{
				AWS: &marklogicv1.AWSObjectStorage{
					AuthType:   marklogicv1.ObjectStorageAuthSecret,
					SecretName: "ml-s3",
					Region:     "us-east-1",
				},
			},
		},
		{
			name: "azure secret auth with secretName is accepted",
			objectStore: &marklogicv1.ObjectStorageConfig{
				Azure: &marklogicv1.AzureObjectStorage{
					AuthType:   marklogicv1.ObjectStorageAuthSecret,
					SecretName: "ml-azure",
				},
			},
		},
		{
			name: "both providers are accepted together",
			objectStore: &marklogicv1.ObjectStorageConfig{
				AWS:   &marklogicv1.AWSObjectStorage{SecretName: "ml-s3"},
				Azure: &marklogicv1.AzureObjectStorage{SecretName: "ml-azure"},
			},
		},
		{
			name: "aws secret auth without secretName is rejected",
			objectStore: &marklogicv1.ObjectStorageConfig{
				AWS: &marklogicv1.AWSObjectStorage{AuthType: marklogicv1.ObjectStorageAuthSecret},
			},
			wantError: "objectStorage.aws.secretName is required",
		},
		{
			name: "azure secret auth without secretName is rejected",
			objectStore: &marklogicv1.ObjectStorageConfig{
				Azure: &marklogicv1.AzureObjectStorage{AuthType: marklogicv1.ObjectStorageAuthSecret},
			},
			wantError: "objectStorage.azure.secretName is required",
		},
		{
			name: "aws instanceRole is rejected with an explanation",
			objectStore: &marklogicv1.ObjectStorageConfig{
				AWS: &marklogicv1.AWSObjectStorage{
					AuthType:   marklogicv1.ObjectStorageAuthInstanceRole,
					SecretName: "ml-s3",
				},
			},
			wantError: "is reserved but not supported",
		},
		{
			name: "azure managedIdentity is rejected with an explanation",
			objectStore: &marklogicv1.ObjectStorageConfig{
				Azure: &marklogicv1.AzureObjectStorage{
					AuthType:   marklogicv1.ObjectStorageAuthManagedIdentity,
					SecretName: "ml-azure",
				},
			},
			wantError: "is reserved but not implemented",
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("cel-validation-%d", i),
					Namespace: "default",
				},
				Spec: marklogicv1.MarklogicClusterSpec{
					Image: "progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6",
					MarkLogicGroups: []*marklogicv1.MarklogicGroups{
						{Name: "dnode", IsBootstrap: true},
					},
					ObjectStorage: test.objectStore,
				},
			}

			err := apiClient.Create(context.Background(), cluster)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("expected the resource to be accepted, got %v", err)
				}
				_ = apiClient.Delete(context.Background(), cluster)
				return
			}

			if err == nil {
				_ = apiClient.Delete(context.Background(), cluster)
				t.Fatalf("expected rejection containing %q, but the resource was accepted", test.wantError)
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected an error containing %q, got %v", test.wantError, err)
			}
		})
	}
}
