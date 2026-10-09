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

package utils

import (
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func freshCluster() (*marklogicv1.MarklogicCluster, SecretMeta) {
	now := metav1.Now()
	truth := true
	cluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Generation: 3},
		Spec: marklogicv1.MarklogicClusterSpec{ObjectStorage: &marklogicv1.ObjectStorageConfig{
			AWS: &marklogicv1.AWSObjectStorage{AuthType: marklogicv1.ObjectStorageAuthSecret, SecretName: "aws-creds"},
		}},
		Status: marklogicv1.MarklogicClusterStatus{ObjectStorage: &marklogicv1.ObjectStorageStatus{AWS: &marklogicv1.ObjectStorageProviderStatus{
			Phase:              marklogicv1.ObjectStoragePhaseApplied,
			ObservedGeneration: 3,
			ObservedSecret:     &marklogicv1.ObjectStorageSecretRef{Name: "aws-creds", UID: "uid-1", ResourceVersion: "20"},
			DetachEligible:     &truth,
			AuthType:           "secret",
			AppliedFingerprint: "opaque-digest",
			LastAppliedTime:    &now,
			AppliedSecret:      &marklogicv1.ObjectStorageSecretRef{Name: "aws-creds", UID: "uid-1", ResourceVersion: "10"},
		}}},
	}
	return cluster, SecretMeta{Name: "aws-creds", UID: "uid-1", ResourceVersion: "20"}
}

func TestObjectStorageReady(t *testing.T) {
	t.Parallel()
	falsy := false

	tests := []struct {
		name   string
		mutate func(*marklogicv1.MarklogicCluster, *SecretMeta)
		ready  bool
	}{
		{"complete fresh success", func(*marklogicv1.MarklogicCluster, *SecretMeta) {}, true},
		{"historical applied revision may differ after a metadata-only update", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.AppliedSecret.ResourceVersion = "1"
		}, true},
		{"provider not declared", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Spec.ObjectStorage.AWS = nil }, false},
		{"no status block", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Status.ObjectStorage = nil }, false},
		{"no provider entry", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Status.ObjectStorage.AWS = nil }, false},
		{"phase only is insufficient: pending", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.Phase = marklogicv1.ObjectStoragePhasePending
		}, false},
		{"detached", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.Phase = marklogicv1.ObjectStoragePhaseDetached
		}, false},
		{"stale generation", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Generation = 4 }, false},
		{"stale secret revision", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { s.ResourceVersion = "21" }, false},
		{"recreated secret uid", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { s.UID = "uid-2" }, false},
		{"different secret reference", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { s.Name = "other" }, false},
		{"absent eligibility", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Status.ObjectStorage.AWS.DetachEligible = nil }, false},
		{"false eligibility (checkpoint)", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.DetachEligible = &falsy
		}, false},
		{"missing fingerprint", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.AppliedFingerprint = ""
		}, false},
		{"missing applied time", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Status.ObjectStorage.AWS.LastAppliedTime = nil }, false},
		{"missing applied secret", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Status.ObjectStorage.AWS.AppliedSecret = nil }, false},
		{"incomplete applied secret", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.AppliedSecret.ResourceVersion = ""
		}, false},
		{"legacy applied status", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			legacy := &marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseApplied, ObservedGeneration: 3, ObservedSecret: c.Status.ObjectStorage.AWS.ObservedSecret}
			c.Status.ObjectStorage.AWS = legacy
		}, false},
		{"applied binding differs by uid", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) {
			c.Status.ObjectStorage.AWS.AppliedSecret.UID = "uid-old"
		}, false},
		{"applied auth type differs", func(c *marklogicv1.MarklogicCluster, s *SecretMeta) { c.Status.ObjectStorage.AWS.AuthType = "other" }, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cluster, secret := freshCluster()
			test.mutate(cluster, &secret)
			ready, reason := ObjectStorageReady(cluster, ObjectStorageAWS, secret)
			if ready != test.ready {
				t.Fatalf("ready = %v (%s), want %v", ready, reason, test.ready)
			}
			if ready && reason != "" {
				t.Fatalf("a ready result carries no reason")
			}
			if !ready && reason == "" {
				t.Fatalf("a not-ready result names the unmet condition")
			}
		})
	}
}

func TestObjectStorageReadyChecksTheRequestedProviderOnly(t *testing.T) {
	t.Parallel()
	cluster, secret := freshCluster()
	cluster.Spec.ObjectStorage.Azure = &marklogicv1.AzureObjectStorage{SecretName: "azure-creds"}
	if ready, _ := ObjectStorageReady(cluster, ObjectStorageAzure, SecretMeta{Name: "azure-creds", UID: "u", ResourceVersion: "1"}); ready {
		t.Fatalf("Azure has no published status yet")
	}
	if ready, reason := ObjectStorageReady(cluster, ObjectStorageAWS, secret); !ready {
		t.Fatalf("AWS should be unaffected by Azure: %s", reason)
	}
}
