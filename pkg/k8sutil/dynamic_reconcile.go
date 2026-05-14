// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var NewDynamicManagementClient = func(opts mlmanage.ClientOptions) mlmanage.Client {
	return mlmanage.NewClient(opts)
}

const (
	dynamicPhasePending    = "pending"
	dynamicPhaseDegraded   = "degraded"
	dynamicPhaseFailed     = "failed"
	dynamicPhaseConfigured = "configured"

	dynamicReasonBootstrapNotReady   = "BootstrapNotReady"
	dynamicReasonInvalidConfig       = "InvalidConfiguration"
	dynamicReasonGroupConfigFailed   = "GroupConfigFailed"
	minimumSupportedMarkLogicVersion = 12
)

var statusCodeRegex = regexp.MustCompile(`status\s+(\d{3})`)

func (oc *OperatorContext) ReconcileDynamicGroupConfig() result.ReconcileResult {
	if !oc.MarklogicGroup.Spec.IsDynamic {
		return result.Continue()
	}

	if oc.MarklogicGroup.Status.Dynamic == nil {
		if err := oc.setDynamicStatus(dynamicPhasePending, "", "waiting for bootstrap readiness", false, false, false); err != nil {
			return result.Error(err)
		}
	}

	clusterName, err := oc.getOwningClusterName()
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, err.Error(), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	bootstrapHost := strings.TrimSpace(oc.MarklogicGroup.Spec.BootstrapHost)
	if bootstrapHost == "" {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, "bootstrap host is required for dynamic groups", false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	adminSecretName := strings.TrimSpace(oc.MarklogicGroup.Spec.SecretName)
	if adminSecretName == "" {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, "admin credential secret is missing", false, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	adminUser, adminPass, err := oc.readCredentialSecret(adminSecretName)
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, fmt.Sprintf("failed to read admin credentials: %v", err), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	useTLS := oc.MarklogicGroup.Spec.Tls != nil && oc.MarklogicGroup.Spec.Tls.EnableOnDefaultAppServers
	adminClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host:               bootstrapHost,
		Username:           adminUser,
		Password:           adminPass,
		UseTLS:             useTLS,
		InsecureSkipVerify: useTLS,
	})

	hosts, err := adminClient.ListHostsStatus(oc.Ctx)
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, fmt.Sprintf("bootstrap readiness check failed: %v", err), false, false, false); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	version := ""
	for _, host := range hosts {
		if !host.Online {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonBootstrapNotReady, "bootstrap hosts are not yet online", false, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if version == "" && host.Version != "" {
			version = host.Version
		}
	}

	if !isSupportedMarkLogicVersion(version) {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, fmt.Sprintf("unsupported bootstrap MarkLogic version: %s", version), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	manageSecret, err := oc.ensureDynamicCredentialSecret(clusterName)
	if err != nil {
		if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to reconcile dynamic credential secret: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}
	manageUser := string(manageSecret.Data["username"])
	managePass := string(manageSecret.Data["password"])

	if err := adminClient.EnsureManageAdminUser(oc.Ctx, manageUser, managePass); err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("manage-admin user reconcile is pending: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("manage-admin user reconcile failed: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	groupClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host:               bootstrapHost,
		Username:           manageUser,
		Password:           managePass,
		UseTLS:             useTLS,
		InsecureSkipVerify: useTLS,
	})

	groupName := resolvedMarkLogicGroupName(oc.MarklogicGroup)
	groupInfo, err := groupClient.GetGroup(oc.Ctx, groupName)
	if err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to inspect MarkLogic group: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to inspect MarkLogic group: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if groupInfo.Exists && groupInfo.ForestCount > 0 {
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonInvalidConfig, "dynamic group is mapped to a MarkLogic group that already has forests", true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if !groupInfo.Exists {
		if err := groupClient.CreateGroup(oc.Ctx, groupName); err != nil {
			if isTransientManagementError(err) {
				if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to create MarkLogic group: %v", err), true, false, false); err != nil {
					return result.Error(err)
				}
				return result.RequeueSoon(5)
			}
			if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to create MarkLogic group: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.Done()
		}
	}

	if err := groupClient.EnableDynamicHosts(oc.Ctx, groupName); err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable allow-dynamic-hosts: %v", err), true, false, false); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable allow-dynamic-hosts: %v", err), true, false, false); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if err := groupClient.EnableAdminAPITokenAuthentication(oc.Ctx, groupName); err != nil {
		if isTransientManagementError(err) {
			if err := oc.setDynamicStatus(dynamicPhaseDegraded, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable API-token-authentication: %v", err), true, false, true); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if err := oc.setDynamicStatus(dynamicPhaseFailed, dynamicReasonGroupConfigFailed, fmt.Sprintf("failed to enable API-token-authentication: %v", err), true, false, true); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if err := oc.setDynamicStatus(dynamicPhaseConfigured, "", "dynamic group configured for token-based host management", true, true, true); err != nil {
		return result.Error(err)
	}

	return result.Continue()
}

func (oc *OperatorContext) setDynamicStatus(phase, reason, message string, bootstrapReady, configured, dynamicHostsEnabled bool) error {
	current := oc.MarklogicGroup.Status.Dynamic
	next := &marklogicv1.DynamicGroupStatus{
		Phase:               phase,
		Reason:              reason,
		Message:             message,
		BootstrapReady:      bootstrapReady,
		Configured:          configured,
		DynamicHostsEnabled: dynamicHostsEnabled,
	}
	if current != nil {
		next.Hosts = current.Hosts
	}
	if current != nil && current.Phase == next.Phase && current.Reason == next.Reason && current.Message == next.Message && current.BootstrapReady == next.BootstrapReady && current.Configured == next.Configured && current.DynamicHostsEnabled == next.DynamicHostsEnabled {
		return nil
	}

	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	oc.MarklogicGroup.Status.Dynamic = next
	return oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch)
}

func (oc *OperatorContext) getOwningClusterName() (string, error) {
	for _, ownerRef := range oc.MarklogicGroup.OwnerReferences {
		if ownerRef.Kind == "MarklogicCluster" {
			return ownerRef.Name, nil
		}
	}
	if strings.HasSuffix(oc.MarklogicGroup.Spec.SecretName, "-admin") {
		return strings.TrimSuffix(oc.MarklogicGroup.Spec.SecretName, "-admin"), nil
	}
	return "", fmt.Errorf("unable to resolve owning MarklogicCluster for dynamic group")
}

func (oc *OperatorContext) ensureDynamicCredentialSecret(clusterName string) (*corev1.Secret, error) {
	secretName := dynamicCredentialSecretName(clusterName)
	nsName := types.NamespacedName{Name: secretName, Namespace: oc.MarklogicGroup.Namespace}
	secret := &corev1.Secret{}
	if err := oc.Client.Get(oc.Ctx, nsName, secret); err == nil {
		return secret, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	labels := getSelectorLabels(clusterName)
	annotations := oc.GetOperatorAnnotations()
	secretMeta := generateObjectMeta(secretName, oc.MarklogicGroup.Namespace, labels, annotations)
	secretData := generateDynamicSecretData(clusterName)
	secretDef := generateSecretDef(secretMeta, marklogicServerAsOwner(oc.MarklogicGroup), secretData)

	if err := oc.Client.Create(oc.Ctx, secretDef); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}

	if err := oc.Client.Get(oc.Ctx, nsName, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (oc *OperatorContext) readCredentialSecret(secretName string) (string, string, error) {
	secret := &corev1.Secret{}
	nsName := types.NamespacedName{Name: secretName, Namespace: oc.MarklogicGroup.Namespace}
	if err := oc.Client.Get(oc.Ctx, nsName, secret); err != nil {
		return "", "", err
	}
	username, hasUser := secret.Data["username"]
	password, hasPass := secret.Data["password"]
	if !hasUser || !hasPass {
		return "", "", fmt.Errorf("secret %s missing username/password", secretName)
	}
	return string(username), string(password), nil
}

func resolvedMarkLogicGroupName(group *marklogicv1.MarklogicGroup) string {
	if group.Spec.GroupConfig != nil && strings.TrimSpace(group.Spec.GroupConfig.Name) != "" {
		return group.Spec.GroupConfig.Name
	}
	if strings.TrimSpace(group.Spec.Name) != "" {
		return group.Spec.Name
	}
	return group.Name
}

func isSupportedMarkLogicVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	parts := strings.SplitN(version, ".", 2)
	majorPart := parts[0]
	majorPart = strings.TrimLeftFunc(majorPart, func(r rune) bool { return r < '0' || r > '9' })
	if majorPart == "" {
		return false
	}
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return false
	}
	return major >= minimumSupportedMarkLogicVersion
}

func isTransientManagementError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "timeout") || strings.Contains(message, "connection refused") || strings.Contains(message, "no such host") || strings.Contains(message, "temporary") {
		return true
	}
	statusMatch := statusCodeRegex.FindStringSubmatch(message)
	if len(statusMatch) != 2 {
		return false
	}
	statusCode, convErr := strconv.Atoi(statusMatch[1])
	if convErr != nil {
		return false
	}
	return statusCode == 429 || statusCode >= 500
}
