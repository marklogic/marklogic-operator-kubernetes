// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	objectStorageBootstrapRetry = 10 * time.Second
	objectStorageDefaultRetry   = 30 * time.Second
	objectStorageMaxMessage     = 512

	objectStorageAppliedMessage = "Credentials accepted by the MarkLogic Management API."
)

type objectStorageProvider string

const (
	providerAWS   objectStorageProvider = "aws"
	providerAzure objectStorageProvider = "azure"
)

var objectStorageProviders = []objectStorageProvider{providerAWS, providerAzure}

func (p objectStorageProvider) displayName() string {
	if p == providerAWS {
		return "AWS"
	}
	return "Azure"
}

// providerBinding is the declared provider/auth-mode/Secret-name triple.
type providerBinding struct {
	authType   string
	secretName string
}

func declaredObjectStorageProviders(cluster *marklogicv1.MarklogicCluster) map[objectStorageProvider]providerBinding {
	declared := map[objectStorageProvider]providerBinding{}
	cfg := cluster.Spec.ObjectStorage
	if cfg == nil {
		return declared
	}
	if cfg.AWS != nil {
		declared[providerAWS] = providerBinding{authType: authTypeOrDefault(cfg.AWS.AuthType), secretName: strings.TrimSpace(cfg.AWS.SecretName)}
	}
	if cfg.Azure != nil {
		declared[providerAzure] = providerBinding{authType: authTypeOrDefault(cfg.Azure.AuthType), secretName: strings.TrimSpace(cfg.Azure.SecretName)}
	}
	return declared
}

func authTypeOrDefault(authType marklogicv1.ObjectStorageAuthType) string {
	if authType == "" {
		return string(marklogicv1.ObjectStorageAuthSecret)
	}
	return string(authType)
}

// resolvedCredentials holds trimmed in-memory material; it is never persisted or logged.
type resolvedCredentials struct {
	fields [][2]string
	apply  func(ctx context.Context, client mlmanage.CredentialClient) error
}

func (resolvedCredentials) String() string   { return "resolvedCredentials{<redacted>}" }
func (resolvedCredentials) GoString() string { return "resolvedCredentials{<redacted>}" }

func trimmedSecretValue(data map[string][]byte, key string) string {
	return strings.TrimSpace(string(data[key]))
}

func resolveObjectStorageCredentials(provider objectStorageProvider, data map[string][]byte) (*resolvedCredentials, []string) {
	var missing []string
	require := func(key string) string {
		value := trimmedSecretValue(data, key)
		if value == "" {
			missing = append(missing, key)
		}
		return value
	}

	switch provider {
	case providerAWS:
		creds := mlmanage.AWSCredentials{
			AccessKey:    require("accessKey"),
			SecretKey:    require("secretKey"),
			SessionToken: trimmedSecretValue(data, "sessionToken"),
		}
		if len(missing) > 0 {
			return nil, missing
		}
		fields := [][2]string{{"accessKey", creds.AccessKey}, {"secretKey", creds.SecretKey}}
		if creds.SessionToken != "" {
			fields = append(fields, [2]string{"sessionToken", creds.SessionToken})
		}
		return &resolvedCredentials{fields: fields, apply: func(ctx context.Context, client mlmanage.CredentialClient) error {
			return client.ApplyAWSCredentials(ctx, creds)
		}}, nil
	default:
		creds := mlmanage.AzureCredentials{
			StorageAccount: require("storageAccount"),
			StorageKey:     require("storageKey"),
		}
		if len(missing) > 0 {
			return nil, missing
		}
		fields := [][2]string{{"storageAccount", creds.StorageAccount}, {"storageKey", creds.StorageKey}}
		return &resolvedCredentials{fields: fields, apply: func(ctx context.Context, client mlmanage.CredentialClient) error {
			return client.ApplyAzureCredentials(ctx, creds)
		}}, nil
	}
}

func writeFingerprintFrame(w io.Writer, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = w.Write(length[:])
	_, _ = io.WriteString(w, value)
}

// objectStorageFingerprint is an opaque HMAC-SHA256 keyed by the cluster UID over a length-framed encoding.
func objectStorageFingerprint(clusterUID types.UID, provider objectStorageProvider, fields [][2]string) string {
	mac := hmac.New(sha256.New, []byte(clusterUID))
	writeFingerprintFrame(mac, string(provider))
	for _, field := range fields {
		writeFingerprintFrame(mac, field[0])
		writeFingerprintFrame(mac, field[1])
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func providerEntry(status *marklogicv1.ObjectStorageStatus, provider objectStorageProvider) *marklogicv1.ObjectStorageProviderStatus {
	if status == nil {
		return nil
	}
	if provider == providerAWS {
		return status.AWS
	}
	return status.Azure
}

func setProviderEntry(status *marklogicv1.ObjectStorageStatus, provider objectStorageProvider, entry *marklogicv1.ObjectStorageProviderStatus) {
	if provider == providerAWS {
		status.AWS = entry
		return
	}
	status.Azure = entry
}

func isDetachEligible(entry *marklogicv1.ObjectStorageProviderStatus) bool {
	return entry != nil && entry.DetachEligible != nil && *entry.DetachEligible
}

func hasCompleteSuccess(entry *marklogicv1.ObjectStorageProviderStatus) bool {
	if entry == nil || entry.AuthType == "" || entry.AppliedFingerprint == "" || entry.LastAppliedTime == nil || entry.AppliedSecret == nil {
		return false
	}
	secret := entry.AppliedSecret
	return secret.Name != "" && secret.UID != "" && secret.ResourceVersion != ""
}

func matchesBinding(entry *marklogicv1.ObjectStorageProviderStatus, binding providerBinding) bool {
	return entry != nil && entry.AuthType == binding.authType && entry.AppliedSecret != nil && entry.AppliedSecret.Name == binding.secretName
}

type eligibilityAction int

const (
	eligibilityPreserve eligibilityAction = iota
	eligibilityClear
	eligibilityGrant
)

type appliedRecord struct {
	authType    string
	fingerprint string
	secret      marklogicv1.ObjectStorageSecretRef
	time        metav1.Time
}

// providerResult is the final outcome for one provider in one reconcile.
type providerResult struct {
	phase       marklogicv1.ObjectStoragePhase
	reason      marklogicv1.ObjectStorageReason
	message     string
	observed    marklogicv1.ObjectStorageSecretRef
	eligibility eligibilityAction
	applied     *appliedRecord
	retryAfter  time.Duration
}

// pendingPut is a required credential application awaiting bootstrap prerequisites.
type pendingPut struct {
	creds       *resolvedCredentials
	fingerprint string
	observed    marklogicv1.ObjectStorageSecretRef
}

type providerEvaluation struct {
	result *providerResult
	put    *pendingPut
}

func truncateObjectStorageMessage(message string) string {
	if len(message) <= objectStorageMaxMessage {
		return message
	}
	return message[:objectStorageMaxMessage]
}

func detachedMessage(secretName string) string {
	return truncateObjectStorageMessage("The Secret is absent after credentials were applied. " +
		"The credentials stored in MarkLogic remain in effect and are not revoked, " +
		"rotation resumes when the referenced Secret is recreated, " +
		"cloud validity is not verified, and temporary credentials can expire while detached. Secret: " + secretName)
}

// retainedObserved keeps previously observed identity only for the same Secret name.
func retainedObserved(snapshot *marklogicv1.ObjectStorageProviderStatus, binding providerBinding) marklogicv1.ObjectStorageSecretRef {
	ref := marklogicv1.ObjectStorageSecretRef{Name: binding.secretName}
	if snapshot != nil && snapshot.ObservedSecret != nil && snapshot.ObservedSecret.Name == binding.secretName {
		ref.UID = snapshot.ObservedSecret.UID
		ref.ResourceVersion = snapshot.ObservedSecret.ResourceVersion
	}
	return ref
}

// evaluateProvider resolves one provider locally, before any bootstrap access.
func evaluateProvider(provider objectStorageProvider, binding providerBinding, snapshot *marklogicv1.ObjectStorageProviderStatus, secret *corev1.Secret, readErr error, clusterUID types.UID) providerEvaluation {
	if readErr != nil {
		observed := retainedObserved(snapshot, binding)
		if apierrors.IsNotFound(readErr) {
			if isDetachEligible(snapshot) && hasCompleteSuccess(snapshot) && matchesBinding(snapshot, binding) {
				return providerEvaluation{result: &providerResult{
					phase:       marklogicv1.ObjectStoragePhaseDetached,
					reason:      marklogicv1.ObjectStorageReasonSecretDeletedAfterApply,
					message:     detachedMessage(binding.secretName),
					observed:    observed,
					eligibility: eligibilityPreserve,
				}}
			}
			return providerEvaluation{result: &providerResult{
				phase:       marklogicv1.ObjectStoragePhaseFailed,
				reason:      marklogicv1.ObjectStorageReasonSecretNotFound,
				message:     truncateObjectStorageMessage(fmt.Sprintf("Secret %q was not found in the cluster namespace.", binding.secretName)),
				observed:    observed,
				eligibility: eligibilityClear,
				retryAfter:  objectStorageDefaultRetry,
			}}
		}
		eligibility := eligibilityPreserve
		if !matchesBinding(snapshot, binding) {
			eligibility = eligibilityClear
		}
		return providerEvaluation{result: &providerResult{
			phase:       marklogicv1.ObjectStoragePhaseFailed,
			reason:      marklogicv1.ObjectStorageReasonSecretReadFailed,
			message:     truncateObjectStorageMessage(fmt.Sprintf("Secret %q could not be read from the Kubernetes API.", binding.secretName)),
			observed:    observed,
			eligibility: eligibility,
			retryAfter:  objectStorageDefaultRetry,
		}}
	}

	observed := marklogicv1.ObjectStorageSecretRef{Name: binding.secretName, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion}
	creds, missing := resolveObjectStorageCredentials(provider, secret.Data)
	if len(missing) > 0 {
		return providerEvaluation{result: &providerResult{
			phase:       marklogicv1.ObjectStoragePhaseFailed,
			reason:      marklogicv1.ObjectStorageReasonSecretKeyMissing,
			message:     truncateObjectStorageMessage(fmt.Sprintf("Secret %q is missing required non-empty key(s): %s.", binding.secretName, strings.Join(missing, ", "))),
			observed:    observed,
			eligibility: eligibilityClear,
			retryAfter:  objectStorageDefaultRetry,
		}}
	}

	fingerprint := objectStorageFingerprint(clusterUID, provider, creds.fields)
	if snapshot != nil && snapshot.Phase == marklogicv1.ObjectStoragePhaseApplied &&
		isDetachEligible(snapshot) && hasCompleteSuccess(snapshot) && matchesBinding(snapshot, binding) &&
		snapshot.AppliedSecret.UID == observed.UID && snapshot.AppliedFingerprint == fingerprint {
		return providerEvaluation{result: &providerResult{
			phase:       marklogicv1.ObjectStoragePhaseApplied,
			message:     objectStorageAppliedMessage,
			observed:    observed,
			eligibility: eligibilityPreserve,
		}}
	}
	return providerEvaluation{put: &pendingPut{creds: creds, fingerprint: fingerprint, observed: observed}}
}

func bootstrapPendingResult(put *pendingPut, message string) *providerResult {
	return &providerResult{
		phase:       marklogicv1.ObjectStoragePhasePending,
		reason:      marklogicv1.ObjectStorageReasonBootstrapNotReady,
		message:     truncateObjectStorageMessage(message),
		observed:    put.observed,
		eligibility: eligibilityClear,
		retryAfter:  objectStorageBootstrapRetry,
	}
}

// bootstrapProbeMessage describes a failed readiness prerequisite without raw error text.
func bootstrapProbeMessage(err error) string {
	var prerequisite *bootstrapPrerequisiteError
	if errors.As(err, &prerequisite) {
		return "Waiting for bootstrap host: " + prerequisite.message
	}
	var credErr *mlmanage.CredentialError
	if errors.As(err, &credErr) {
		switch {
		case credErr.IsTransport():
			return "Waiting for bootstrap host: the Management API is unreachable."
		case credErr.StatusCode == 401:
			return "Waiting for bootstrap host: Management API authentication failed (HTTP 401)."
		default:
			return "Waiting for bootstrap host: the Management API is not ready (HTTP " + strconv.Itoa(credErr.StatusCode) + ")."
		}
	}
	return "Waiting for bootstrap host: the Management API is not ready."
}

type bootstrapPrerequisiteError struct{ message string }

func (e *bootstrapPrerequisiteError) Error() string { return e.message }

// putOutcome maps the application attempt to a provider result.
func putOutcome(binding providerBinding, put *pendingPut, applyErr error, now metav1.Time) *providerResult {
	if applyErr == nil {
		return &providerResult{
			phase:       marklogicv1.ObjectStoragePhaseApplied,
			message:     objectStorageAppliedMessage,
			observed:    put.observed,
			eligibility: eligibilityGrant,
			applied: &appliedRecord{
				authType:    binding.authType,
				fingerprint: put.fingerprint,
				secret:      put.observed,
				time:        now,
			},
		}
	}

	result := &providerResult{
		phase:       marklogicv1.ObjectStoragePhaseFailed,
		observed:    put.observed,
		eligibility: eligibilityClear,
	}
	operation := "PUT " + mlmanage.CredentialsPropertiesPath
	var credErr *mlmanage.CredentialError
	if !errors.As(applyErr, &credErr) || credErr.IsTransport() {
		result.reason = marklogicv1.ObjectStorageReasonManagementAPIUnreachable
		result.message = "The Management API request failed without an HTTP response."
		result.retryAfter = objectStorageDefaultRetry
		return result
	}

	result.message = fmt.Sprintf("%s returned HTTP %d.", operation, credErr.StatusCode)
	switch {
	case credErr.StatusCode == 400:
		result.reason = marklogicv1.ObjectStorageReasonInvalidPayload
	case credErr.StatusCode == 401:
		result.reason = marklogicv1.ObjectStorageReasonAuthenticationFailed
		result.retryAfter = objectStorageDefaultRetry
	case credErr.StatusCode == 403:
		result.reason = marklogicv1.ObjectStorageReasonInsufficientPrivilege
	case credErr.StatusCode >= 500:
		result.reason = marklogicv1.ObjectStorageReasonManagementAPIUnreachable
		result.retryAfter = objectStorageDefaultRetry
	default:
		result.reason = marklogicv1.ObjectStorageReasonManagementAPIError
		result.retryAfter = objectStorageDefaultRetry
	}
	return result
}

// applyProviderResult builds the next persisted entry, retaining success history.
func applyProviderResult(current *marklogicv1.ObjectStorageProviderStatus, result providerResult, generation int64) *marklogicv1.ObjectStorageProviderStatus {
	next := current.DeepCopy()
	if next == nil {
		next = &marklogicv1.ObjectStorageProviderStatus{}
	}
	next.Phase = result.phase
	next.Reason = result.reason
	next.Message = result.message
	next.ObservedGeneration = generation
	observed := result.observed
	next.ObservedSecret = &observed

	switch result.eligibility {
	case eligibilityClear:
		value := false
		next.DetachEligible = &value
	case eligibilityGrant:
		value := true
		next.DetachEligible = &value
	}

	if result.applied != nil {
		appliedAt := result.applied.time
		secret := result.applied.secret
		next.AuthType = result.applied.authType
		next.AppliedFingerprint = result.applied.fingerprint
		next.LastAppliedTime = &appliedAt
		next.AppliedSecret = &secret
	}
	return next
}

// failureKey is the persisted comparison key used to deduplicate failure events.
type failureKey struct {
	generation      int64
	secretName      string
	secretUID       string
	resourceVersion string
	phase           marklogicv1.ObjectStoragePhase
	reason          marklogicv1.ObjectStorageReason
}

func failureKeyOfEntry(entry *marklogicv1.ObjectStorageProviderStatus) failureKey {
	key := failureKey{generation: entry.ObservedGeneration, phase: entry.Phase, reason: entry.Reason}
	if entry.ObservedSecret != nil {
		key.secretName = entry.ObservedSecret.Name
		key.secretUID = entry.ObservedSecret.UID
		key.resourceVersion = entry.ObservedSecret.ResourceVersion
	}
	return key
}

func failureKeyOfResult(generation int64, result providerResult) failureKey {
	return failureKey{
		generation:      generation,
		secretName:      result.observed.Name,
		secretUID:       result.observed.UID,
		resourceVersion: result.observed.ResourceVersion,
		phase:           result.phase,
		reason:          result.reason,
	}
}
