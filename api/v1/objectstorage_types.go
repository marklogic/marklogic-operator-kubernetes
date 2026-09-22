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

// ObjectStorageAuthType selects how credential material is supplied for a provider.
type ObjectStorageAuthType string

const (
	ObjectStorageAuthSecret ObjectStorageAuthType = "secret"
	// ObjectStorageAuthInstanceRole is reserved and rejected by validation. MarkLogic
	// resolves credentials through its own order of precedence rather than the AWS
	// credential provider chain, so IRSA cannot be supported.
	ObjectStorageAuthInstanceRole ObjectStorageAuthType = "instanceRole"
	// ObjectStorageAuthManagedIdentity is reserved and rejected by validation.
	ObjectStorageAuthManagedIdentity ObjectStorageAuthType = "managedIdentity"
)

// ObjectStorageConfig declares cluster-wide object storage access. MarkLogic stores
// one credential set per provider for the whole cluster, so this is reconciled once
// rather than per MarklogicGroup.
type ObjectStorageConfig struct {
	// +optional
	AWS *AWSObjectStorage `json:"aws,omitempty"`
	// +optional
	Azure *AzureObjectStorage `json:"azure,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.authType) || self.authType != 'instanceRole'", message="objectStorage.aws.authType 'instanceRole' is reserved but not supported: MarkLogic does not resolve S3 credentials through the AWS credential provider chain, so IRSA cannot be used. Use authType 'secret'."
// +kubebuilder:validation:XValidation:rule="!has(self.authType) || self.authType != 'secret' || (has(self.secretName) && size(self.secretName) > 0)", message="objectStorage.aws.secretName is required when authType is 'secret'"
type AWSObjectStorage struct {
	// +kubebuilder:validation:Enum=secret;instanceRole
	// +kubebuilder:default:="secret"
	// +optional
	AuthType ObjectStorageAuthType `json:"authType,omitempty"`
	// Name of the Secret holding accessKey and secretKey. An optional sessionToken supports externally refreshed STS credentials.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// Informational only. The credentials API carries no region; S3 region is resolved
	// by MarkLogic from its own configuration.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Region string `json:"region,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.authType) || self.authType != 'managedIdentity'", message="objectStorage.azure.authType 'managedIdentity' is reserved but not implemented in this release. Use authType 'secret'."
// +kubebuilder:validation:XValidation:rule="!has(self.authType) || self.authType != 'secret' || (has(self.secretName) && size(self.secretName) > 0)", message="objectStorage.azure.secretName is required when authType is 'secret'"
type AzureObjectStorage struct {
	// +kubebuilder:validation:Enum=secret;managedIdentity
	// +kubebuilder:default:="secret"
	// +optional
	AuthType ObjectStorageAuthType `json:"authType,omitempty"`
	// Name of the Secret holding storageAccount and storageKey.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// ObjectStoragePhase reports per-provider progress.
// +kubebuilder:validation:Enum=Pending;Applied;Failed;Disabled
type ObjectStoragePhase string

const (
	ObjectStoragePhasePending ObjectStoragePhase = "Pending"
	ObjectStoragePhaseApplied ObjectStoragePhase = "Applied"
	ObjectStoragePhaseFailed  ObjectStoragePhase = "Failed"
	// ObjectStoragePhaseDisabled means the provider is not managed by the operator.
	// Credentials applied by an earlier spec revision may still be active in MarkLogic,
	// because removing a provider block does not revoke them.
	ObjectStoragePhaseDisabled ObjectStoragePhase = "Disabled"
)

// ObjectStorageFailureReason is machine-readable and distinguishes failures that need
// different operator responses. 401 and 403 are kept separate because they point at
// different misconfigurations: a stale admin Secret versus missing MarkLogic privileges.
// +kubebuilder:validation:Enum=BootstrapNotReady;SecretNotFound;SecretKeyMissing;InvalidPayload;AuthenticationFailed;InsufficientPrivilege;ManagementAPIUnreachable;ManagementAPIError
type ObjectStorageFailureReason string

const (
	ObjectStorageReasonBootstrapNotReady        ObjectStorageFailureReason = "BootstrapNotReady"
	ObjectStorageReasonSecretNotFound           ObjectStorageFailureReason = "SecretNotFound"
	ObjectStorageReasonSecretKeyMissing         ObjectStorageFailureReason = "SecretKeyMissing"
	ObjectStorageReasonInvalidPayload           ObjectStorageFailureReason = "InvalidPayload"
	ObjectStorageReasonAuthenticationFailed     ObjectStorageFailureReason = "AuthenticationFailed"
	ObjectStorageReasonInsufficientPrivilege    ObjectStorageFailureReason = "InsufficientPrivilege"
	ObjectStorageReasonManagementAPIUnreachable ObjectStorageFailureReason = "ManagementAPIUnreachable"
	ObjectStorageReasonManagementAPIError       ObjectStorageFailureReason = "ManagementAPIError"
)

type ObjectStorageProviderStatus struct {
	// +optional
	Phase ObjectStoragePhase `json:"phase,omitempty"`
	// +optional
	Reason ObjectStorageFailureReason `json:"reason,omitempty"`
	// Never contains credential material.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`
	// Salted digest of the applied material, never the material itself.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	AppliedFingerprint string `json:"appliedFingerprint,omitempty"`
	// +optional
	AuthType ObjectStorageAuthType `json:"authType,omitempty"`
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`
}

type ObjectStorageStatus struct {
	// +optional
	AWS *ObjectStorageProviderStatus `json:"aws,omitempty"`
	// +optional
	Azure *ObjectStorageProviderStatus `json:"azure,omitempty"`
}

// ReferencedSecretNames returns the Secret names this configuration sources credential
// material from, for use by the controller's Secret watch.
func (c *ObjectStorageConfig) ReferencedSecretNames() []string {
	if c == nil {
		return nil
	}

	names := []string{}
	if c.AWS != nil && c.AWS.SecretName != "" {
		names = append(names, c.AWS.SecretName)
	}
	if c.Azure != nil && c.Azure.SecretName != "" {
		names = append(names, c.Azure.SecretName)
	}
	return names
}
