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
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

// ObjectStorageProvider names a declared object storage provider.
type ObjectStorageProvider string

const (
	ObjectStorageAWS   ObjectStorageProvider = "aws"
	ObjectStorageAzure ObjectStorageProvider = "azure"
)

// SecretMeta is the current identity of a provider Secret; credential data is never needed or accepted.
type SecretMeta struct {
	Name            string
	UID             string
	ResourceVersion string
}

// ObjectStorageReady reports whether a provider satisfies every Status Freshness condition for the
// given cluster and current Secret metadata. A phase-only Applied check is insufficient. The second
// return value names the first unmet condition and never contains credential material.
func ObjectStorageReady(cluster *marklogicv1.MarklogicCluster, provider ObjectStorageProvider, secret SecretMeta) (bool, string) {
	secretName, authType, declared := declaredBinding(cluster, provider)
	if !declared {
		return false, "provider is not declared"
	}
	if secret.Name != secretName {
		return false, "current Secret does not match the declared reference"
	}

	var entry *marklogicv1.ObjectStorageProviderStatus
	if cluster.Status.ObjectStorage != nil {
		if provider == ObjectStorageAWS {
			entry = cluster.Status.ObjectStorage.AWS
		} else {
			entry = cluster.Status.ObjectStorage.Azure
		}
	}
	if entry == nil {
		return false, "no provider status has been published"
	}
	if entry.Phase != marklogicv1.ObjectStoragePhaseApplied {
		return false, "phase is not Applied"
	}
	if entry.ObservedGeneration != cluster.Generation {
		return false, "observedGeneration is stale"
	}
	observed := entry.ObservedSecret
	if observed == nil || observed.Name != secret.Name || observed.UID != secret.UID || observed.ResourceVersion != secret.ResourceVersion {
		return false, "observedSecret does not match the current Secret"
	}
	if entry.DetachEligible == nil || !*entry.DetachEligible {
		return false, "detachEligible is not true"
	}
	applied := entry.AppliedSecret
	if entry.AuthType == "" || entry.AppliedFingerprint == "" || entry.LastAppliedTime == nil || applied == nil ||
		applied.Name == "" || applied.UID == "" || applied.ResourceVersion == "" {
		return false, "last-success record is incomplete"
	}
	if entry.AuthType != authType || applied.Name != secretName || applied.UID != secret.UID {
		return false, "last-success record does not match the intended binding"
	}
	return true, ""
}

func declaredBinding(cluster *marklogicv1.MarklogicCluster, provider ObjectStorageProvider) (secretName, authType string, declared bool) {
	config := cluster.Spec.ObjectStorage
	if config == nil {
		return "", "", false
	}
	switch provider {
	case ObjectStorageAWS:
		if config.AWS != nil {
			return config.AWS.SecretName, authTypeOrSecret(config.AWS.AuthType), true
		}
	case ObjectStorageAzure:
		if config.Azure != nil {
			return config.Azure.SecretName, authTypeOrSecret(config.Azure.AuthType), true
		}
	}
	return "", "", false
}

func authTypeOrSecret(authType marklogicv1.ObjectStorageAuthType) string {
	if authType == "" {
		return string(marklogicv1.ObjectStorageAuthSecret)
	}
	return string(authType)
}
