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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectStorageAuthType selects how a provider's credentials are sourced.
// +kubebuilder:validation:Enum=secret;instanceRole;managedIdentity
type ObjectStorageAuthType string

const (
	// ObjectStorageAuthSecret sources credentials from a Kubernetes Secret.
	ObjectStorageAuthSecret ObjectStorageAuthType = "secret"
	// ObjectStorageAuthInstanceRole is reserved for AWS and rejected by validation.
	ObjectStorageAuthInstanceRole ObjectStorageAuthType = "instanceRole"
	// ObjectStorageAuthManagedIdentity is reserved for Azure and rejected by validation.
	ObjectStorageAuthManagedIdentity ObjectStorageAuthType = "managedIdentity"
)

// ObjectStorageConfig declares cluster-wide object storage credentials.
type ObjectStorageConfig struct {
	// AWS S3 credentials.
	AWS *AWSObjectStorage `json:"aws,omitempty"`
	// Azure Blob credentials.
	Azure *AzureObjectStorage `json:"azure,omitempty"`
}

// AWSObjectStorage references the Secret holding AWS S3 credentials.
// +kubebuilder:validation:XValidation:rule="!has(self.authType) || self.authType == 'secret'",message="authType must be 'secret'; instanceRole (IRSA / instance profile) is not supported"
type AWSObjectStorage struct {
	// +kubebuilder:default:=secret
	AuthType ObjectStorageAuthType `json:"authType,omitempty"`
	// SecretName is a Secret in the MarklogicCluster namespace with keys accessKey, secretKey and optional sessionToken.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`
	// Region is informational only and is not applied to MarkLogic.
	// +optional
	Region string `json:"region,omitempty"`
}

// AzureObjectStorage references the Secret holding Azure Blob credentials.
// +kubebuilder:validation:XValidation:rule="!has(self.authType) || self.authType == 'secret'",message="authType must be 'secret'; managedIdentity is not supported"
type AzureObjectStorage struct {
	// +kubebuilder:default:=secret
	AuthType ObjectStorageAuthType `json:"authType,omitempty"`
	// SecretName is a Secret in the MarklogicCluster namespace with keys storageAccount and storageKey.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`
}

// ObjectStoragePhase is the per-provider result of the latest evaluation.
// +kubebuilder:validation:Enum=Pending;Applied;Detached;Failed
type ObjectStoragePhase string

const (
	ObjectStoragePhasePending  ObjectStoragePhase = "Pending"
	ObjectStoragePhaseApplied  ObjectStoragePhase = "Applied"
	ObjectStoragePhaseDetached ObjectStoragePhase = "Detached"
	ObjectStoragePhaseFailed   ObjectStoragePhase = "Failed"
)

// ObjectStorageReason is the machine-readable reason for non-Applied phases.
// +kubebuilder:validation:Enum=BootstrapNotReady;SecretDeletedAfterApply;SecretNotFound;SecretReadFailed;SecretKeyMissing;InvalidPayload;AuthenticationFailed;InsufficientPrivilege;ManagementAPIUnreachable;ManagementAPIError
type ObjectStorageReason string

const (
	ObjectStorageReasonBootstrapNotReady        ObjectStorageReason = "BootstrapNotReady"
	ObjectStorageReasonSecretDeletedAfterApply  ObjectStorageReason = "SecretDeletedAfterApply"
	ObjectStorageReasonSecretNotFound           ObjectStorageReason = "SecretNotFound"
	ObjectStorageReasonSecretReadFailed         ObjectStorageReason = "SecretReadFailed"
	ObjectStorageReasonSecretKeyMissing         ObjectStorageReason = "SecretKeyMissing"
	ObjectStorageReasonInvalidPayload           ObjectStorageReason = "InvalidPayload"
	ObjectStorageReasonAuthenticationFailed     ObjectStorageReason = "AuthenticationFailed"
	ObjectStorageReasonInsufficientPrivilege    ObjectStorageReason = "InsufficientPrivilege"
	ObjectStorageReasonManagementAPIUnreachable ObjectStorageReason = "ManagementAPIUnreachable"
	ObjectStorageReasonManagementAPIError       ObjectStorageReason = "ManagementAPIError"
)

// ObjectStorageSecretRef identifies a Secret object and revision.
type ObjectStorageSecretRef struct {
	Name            string `json:"name,omitempty"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// ObjectStorageProviderStatus is the observed state of one provider.
type ObjectStorageProviderStatus struct {
	Phase  ObjectStoragePhase  `json:"phase,omitempty"`
	Reason ObjectStorageReason `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message,omitempty"`
	// AuthType used for the last successful PUT.
	AuthType string `json:"authType,omitempty"`
	// AppliedFingerprint is an opaque, cluster-specific marker of the last applied material.
	// +kubebuilder:validation:MaxLength=128
	AppliedFingerprint string       `json:"appliedFingerprint,omitempty"`
	LastAppliedTime    *metav1.Time `json:"lastAppliedTime,omitempty"`
	// AppliedSecret identifies the Secret used for the last successful PUT.
	AppliedSecret *ObjectStorageSecretRef `json:"appliedSecret,omitempty"`
	// DetachEligible is the durable eligibility for Secret deletion; absent means false.
	DetachEligible *bool `json:"detachEligible,omitempty"`
	// ObservedGeneration is the cluster generation evaluated for the current result.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedSecret is the Secret reference, identity and revision most recently evaluated.
	ObservedSecret *ObjectStorageSecretRef `json:"observedSecret,omitempty"`
}

// ObjectStorageStatus holds the per-provider results; undeclared providers have no entry.
type ObjectStorageStatus struct {
	AWS   *ObjectStorageProviderStatus `json:"aws,omitempty"`
	Azure *ObjectStorageProviderStatus `json:"azure,omitempty"`
}
