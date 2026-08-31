// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/objectstorage"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// NewObjectStorageManagementClient is a package variable so tests can substitute a stub.
var NewObjectStorageManagementClient = func(opts mlmanage.ClientOptions) mlmanage.Client {
	return mlmanage.NewClient(opts)
}

// Keys expected inside the user-supplied provider Secrets.
const (
	awsAccessKeySecretKey    = "accessKey"
	awsSecretKeySecretKey    = "secretKey"
	azureStorageAccountKey   = "storageAccount"
	azureStorageKeySecretKey = "storageKey"
)

const (
	providerAWS   = "aws"
	providerAzure = "azure"
)

// ReconcileObjectStorage applies cluster-wide object storage credentials. Providers are
// reconciled independently so one failing never blocks the other.
func (cc *ClusterContext) ReconcileObjectStorage() result.ReconcileResult {
	config := cc.MarklogicCluster.Spec.ObjectStorage
	if config == nil || (config.AWS == nil && config.Azure == nil) {
		return cc.disableObjectStorage()
	}

	logger := cc.ReqLogger.WithValues("reconcile", "objectStorage")

	client, err := cc.objectStorageClient()
	if err != nil {
		logger.Info("Object storage configuration is waiting on the bootstrap host", "reason", err.Error())
		cc.setObjectStorageStatus(
			pendingStatus(config.AWS != nil, marklogicv1.ObjectStorageReasonBootstrapNotReady, err.Error()),
			pendingStatus(config.Azure != nil, marklogicv1.ObjectStorageReasonBootstrapNotReady, err.Error()),
		)
		if statusErr := cc.persistObjectStorageStatus(); statusErr != nil {
			return result.Error(statusErr)
		}
		return result.RequeueSoon(10)
	}

	awsStatus := cc.reconcileAWSObjectStorage(client, config.AWS)
	azureStatus := cc.reconcileAzureObjectStorage(client, config.Azure)
	cc.setObjectStorageStatus(awsStatus, azureStatus)

	if statusErr := cc.persistObjectStorageStatus(); statusErr != nil {
		return result.Error(statusErr)
	}

	if objectStorageNeedsRequeue(awsStatus) || objectStorageNeedsRequeue(azureStatus) {
		return result.RequeueSoon(30)
	}
	return result.Continue()
}

func (cc *ClusterContext) reconcileAWSObjectStorage(client mlmanage.Client, spec *marklogicv1.AWSObjectStorage) *marklogicv1.ObjectStorageProviderStatus {
	if spec == nil {
		return &marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseDisabled}
	}

	secret, failure := cc.loadProviderSecret(spec.SecretName)
	if failure != nil {
		return failure
	}

	accessKey, hasAccessKey := secretValue(secret, awsAccessKeySecretKey)
	secretKey, hasSecretKey := secretValue(secret, awsSecretKeySecretKey)
	if !hasAccessKey || !hasSecretKey {
		return failedStatus(marklogicv1.ObjectStorageReasonSecretKeyMissing,
			fmt.Sprintf("secret %q must contain non-empty %q and %q", spec.SecretName, awsAccessKeySecretKey, awsSecretKeySecretKey))
	}

	material := objectstorage.AWSMaterial{AccessKey: accessKey, SecretKey: secretKey}
	fingerprint := material.Fingerprint(cc.objectStorageSalt())
	if applied := cc.alreadyApplied(providerAWS, fingerprint); applied != nil {
		return applied
	}

	if err := client.EnsureAWSCredentials(cc.Ctx, mlmanage.AWSCredentials{AccessKey: accessKey, SecretKey: secretKey}); err != nil {
		return cc.applyFailure(providerAWS, err)
	}

	cc.recordObjectStorageEvent(corev1.EventTypeNormal, "ObjectStorageApplied", "AWS object storage credentials applied")
	cc.ReqLogger.Info("Applied object storage credentials", "provider", providerAWS)
	return appliedStatus(fingerprint)
}

func (cc *ClusterContext) reconcileAzureObjectStorage(client mlmanage.Client, spec *marklogicv1.AzureObjectStorage) *marklogicv1.ObjectStorageProviderStatus {
	if spec == nil {
		return &marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseDisabled}
	}

	secret, failure := cc.loadProviderSecret(spec.SecretName)
	if failure != nil {
		return failure
	}

	storageAccount, hasAccount := secretValue(secret, azureStorageAccountKey)
	storageKey, hasKey := secretValue(secret, azureStorageKeySecretKey)
	if !hasAccount || !hasKey {
		return failedStatus(marklogicv1.ObjectStorageReasonSecretKeyMissing,
			fmt.Sprintf("secret %q must contain non-empty %q and %q", spec.SecretName, azureStorageAccountKey, azureStorageKeySecretKey))
	}

	material := objectstorage.AzureMaterial{StorageAccount: storageAccount, StorageKey: storageKey}
	fingerprint := material.Fingerprint(cc.objectStorageSalt())
	if applied := cc.alreadyApplied(providerAzure, fingerprint); applied != nil {
		return applied
	}

	if err := client.EnsureAzureCredentials(cc.Ctx, mlmanage.AzureCredentials{StorageAccount: storageAccount, StorageKey: storageKey}); err != nil {
		return cc.applyFailure(providerAzure, err)
	}

	cc.recordObjectStorageEvent(corev1.EventTypeNormal, "ObjectStorageApplied", "Azure object storage credentials applied")
	cc.ReqLogger.Info("Applied object storage credentials", "provider", providerAzure)
	return appliedStatus(fingerprint)
}

// objectStorageSalt scopes fingerprints to this cluster so that two clusters holding
// identical credentials do not publish identical digests in status.
func (cc *ClusterContext) objectStorageSalt() string {
	return string(cc.MarklogicCluster.UID)
}

func (cc *ClusterContext) loadProviderSecret(secretName string) (*corev1.Secret, *marklogicv1.ObjectStorageProviderStatus) {
	secretName = strings.TrimSpace(secretName)
	if secretName == "" {
		return nil, failedStatus(marklogicv1.ObjectStorageReasonSecretNotFound, "secretName is required when authType is 'secret'")
	}

	secret := &corev1.Secret{}
	nsName := types.NamespacedName{Name: secretName, Namespace: cc.MarklogicCluster.Namespace}
	if err := cc.Client.Get(cc.Ctx, nsName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, failedStatus(marklogicv1.ObjectStorageReasonSecretNotFound, fmt.Sprintf("secret %q not found", secretName))
		}
		return nil, failedStatus(marklogicv1.ObjectStorageReasonSecretNotFound, fmt.Sprintf("failed to read secret %q", secretName))
	}
	return secret, nil
}

// alreadyApplied returns the existing status when the material is unchanged, so an
// unchanged configuration does not trigger a repeated write or a rotation event.
func (cc *ClusterContext) alreadyApplied(provider, fingerprint string) *marklogicv1.ObjectStorageProviderStatus {
	current := cc.currentProviderStatus(provider)
	if current == nil ||
		current.Phase != marklogicv1.ObjectStoragePhaseApplied ||
		current.AppliedFingerprint != fingerprint {
		return nil
	}
	return current.DeepCopy()
}

func (cc *ClusterContext) currentProviderStatus(provider string) *marklogicv1.ObjectStorageProviderStatus {
	status := cc.MarklogicCluster.Status.ObjectStorage
	if status == nil {
		return nil
	}
	if provider == providerAWS {
		return status.AWS
	}
	return status.Azure
}

func (cc *ClusterContext) applyFailure(provider string, err error) *marklogicv1.ObjectStorageProviderStatus {
	reason := objectStorageFailureReason(err)
	cc.recordObjectStorageEvent(corev1.EventTypeWarning, "ObjectStorageApplyFailed",
		fmt.Sprintf("%s object storage credentials failed: %s", provider, reason))
	cc.ReqLogger.Error(err, "Failed to apply object storage credentials", "provider", provider, "reason", reason)
	return failedStatus(reason, err.Error())
}

// objectStorageFailureReason maps transport and HTTP outcomes to actionable reasons.
// 401 and 403 stay distinct: the former points at the operator's admin Secret, the
// latter at missing MarkLogic privileges.
func objectStorageFailureReason(err error) marklogicv1.ObjectStorageFailureReason {
	var credentialsErr *mlmanage.CredentialsError
	if !errors.As(err, &credentialsErr) {
		return marklogicv1.ObjectStorageReasonManagementAPIError
	}

	switch credentialsErr.StatusCode {
	case 0:
		return marklogicv1.ObjectStorageReasonManagementAPIUnreachable
	case http.StatusBadRequest:
		return marklogicv1.ObjectStorageReasonInvalidPayload
	case http.StatusUnauthorized:
		return marklogicv1.ObjectStorageReasonAuthenticationFailed
	case http.StatusForbidden:
		return marklogicv1.ObjectStorageReasonInsufficientPrivilege
	}
	if credentialsErr.StatusCode >= http.StatusInternalServerError {
		return marklogicv1.ObjectStorageReasonManagementAPIUnreachable
	}
	return marklogicv1.ObjectStorageReasonManagementAPIError
}

// objectStorageNeedsRequeue reports whether the outcome is worth retrying. A malformed
// payload or a privilege gap will not resolve on its own, so those are not requeued.
func objectStorageNeedsRequeue(status *marklogicv1.ObjectStorageProviderStatus) bool {
	if status == nil {
		return false
	}
	switch status.Phase {
	case marklogicv1.ObjectStoragePhaseApplied, marklogicv1.ObjectStoragePhaseDisabled:
		return false
	}
	switch status.Reason {
	case marklogicv1.ObjectStorageReasonInvalidPayload, marklogicv1.ObjectStorageReasonInsufficientPrivilege:
		return false
	default:
		return true
	}
}

func secretValue(secret *corev1.Secret, key string) (string, bool) {
	raw, present := secret.Data[key]
	if !present {
		return "", false
	}
	value := strings.TrimSpace(string(raw))
	return value, value != ""
}

func appliedStatus(fingerprint string) *marklogicv1.ObjectStorageProviderStatus {
	now := metav1.Now()
	return &marklogicv1.ObjectStorageProviderStatus{
		Phase:              marklogicv1.ObjectStoragePhaseApplied,
		AppliedFingerprint: fingerprint,
		AuthType:           marklogicv1.ObjectStorageAuthSecret,
		LastAppliedTime:    &now,
	}
}

func failedStatus(reason marklogicv1.ObjectStorageFailureReason, message string) *marklogicv1.ObjectStorageProviderStatus {
	return &marklogicv1.ObjectStorageProviderStatus{
		Phase:    marklogicv1.ObjectStoragePhaseFailed,
		Reason:   reason,
		Message:  truncateStatusMessage(message),
		AuthType: marklogicv1.ObjectStorageAuthSecret,
	}
}

func pendingStatus(declared bool, reason marklogicv1.ObjectStorageFailureReason, message string) *marklogicv1.ObjectStorageProviderStatus {
	if !declared {
		return &marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseDisabled}
	}
	return &marklogicv1.ObjectStorageProviderStatus{
		Phase:    marklogicv1.ObjectStoragePhasePending,
		Reason:   reason,
		Message:  truncateStatusMessage(message),
		AuthType: marklogicv1.ObjectStorageAuthSecret,
	}
}

// truncateStatusMessage keeps status within the CRD's MaxLength.
func truncateStatusMessage(message string) string {
	const maxLength = 512
	if len(message) <= maxLength {
		return message
	}
	return message[:maxLength-3] + "..."
}

func (cc *ClusterContext) setObjectStorageStatus(aws, azure *marklogicv1.ObjectStorageProviderStatus) {
	cc.MarklogicCluster.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: aws, Azure: azure}
}

// disableObjectStorage clears status when no provider is declared. Note this does not
// revoke credentials already applied to MarkLogic; see the spec's Validation Rules.
func (cc *ClusterContext) disableObjectStorage() result.ReconcileResult {
	if cc.MarklogicCluster.Status.ObjectStorage == nil {
		return result.Continue()
	}
	cc.MarklogicCluster.Status.ObjectStorage = nil
	if err := cc.persistObjectStorageStatus(); err != nil {
		return result.Error(err)
	}
	return result.Continue()
}

func (cc *ClusterContext) persistObjectStorageStatus() error {
	return cc.Client.Status().Update(cc.Ctx, cc.MarklogicCluster)
}

func (cc *ClusterContext) recordObjectStorageEvent(eventType, reason, message string) {
	if cc.Recorder == nil {
		return
	}
	cc.Recorder.Event(cc.MarklogicCluster, eventType, reason, message)
}

// objectStorageClient returns a Management API client for the bootstrap host, or an
// error describing why the cluster is not ready for credential configuration.
func (cc *ClusterContext) objectStorageClient() (mlmanage.Client, error) {
	mlc := cc.MarklogicCluster

	bootstrapHost, err := cc.bootstrapHostFQDN()
	if err != nil {
		return nil, err
	}

	adminSecretName := mlc.ObjectMeta.Name + "-admin"
	if mlc.Spec.Auth != nil && mlc.Spec.Auth.SecretName != nil && strings.TrimSpace(*mlc.Spec.Auth.SecretName) != "" {
		adminSecretName = strings.TrimSpace(*mlc.Spec.Auth.SecretName)
	}

	secret := &corev1.Secret{}
	nsName := types.NamespacedName{Name: adminSecretName, Namespace: mlc.Namespace}
	if err := cc.Client.Get(cc.Ctx, nsName, secret); err != nil {
		return nil, fmt.Errorf("admin credential secret %q is not available", adminSecretName)
	}
	username, hasUser := secretValue(secret, "username")
	password, hasPassword := secretValue(secret, "password")
	if !hasUser || !hasPassword {
		return nil, fmt.Errorf("admin credential secret %q is missing username or password", adminSecretName)
	}

	useTLS := mlc.Spec.Tls != nil && mlc.Spec.Tls.EnableOnDefaultAppServers
	client := NewObjectStorageManagementClient(mlmanage.ClientOptions{
		Host:     bootstrapHost,
		Username: username,
		Password: password,
		UseTLS:   useTLS,
		// Matches the dynamic-host reconcile: operator-managed certificates are
		// not in the client trust store.
		InsecureSkipVerify: useTLS,
	})

	hosts, err := client.ListHostsStatus(cc.Ctx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap host is not reachable yet")
	}
	for _, host := range hosts {
		if isBootstrapHostStatus(host.Name, bootstrapHost) && host.Online {
			return client, nil
		}
	}
	return nil, fmt.Errorf("bootstrap host %q is not online yet", bootstrapHost)
}

func (cc *ClusterContext) bootstrapHostFQDN() (string, error) {
	mlc := cc.MarklogicCluster
	for _, group := range mlc.Spec.MarkLogicGroups {
		if group == nil || !group.IsBootstrap {
			continue
		}
		return fmt.Sprintf("%s-0.%s.%s.svc.%s", group.Name, group.Name, mlc.Namespace, mlc.Spec.ClusterDomain), nil
	}
	return "", fmt.Errorf("no bootstrap MarkLogic group is defined")
}
