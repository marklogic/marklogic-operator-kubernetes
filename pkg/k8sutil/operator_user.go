// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const operatorCredentialsReadyCondition = "OperatorCredentialsReady"

func (oc *OperatorContext) ReconcileOperatorUser() result.ReconcileResult {
	clusterName, err := oc.getOwningClusterName()
	if err != nil {
		return result.Continue()
	}

	bootstrapHost := strings.TrimSpace(oc.MarklogicGroup.Spec.BootstrapHost)
	if bootstrapHost == "" {
		name := strings.TrimSpace(oc.MarklogicGroup.Spec.Name)
		domain := strings.TrimSpace(oc.MarklogicGroup.Spec.ClusterDomain)
		if domain == "" {
			domain = "cluster.local"
		}
		bootstrapHost = fmt.Sprintf("%s-0.%s.%s.svc.%s", name, name, oc.MarklogicGroup.Namespace, domain)
	}

	operatorPassword, err := oc.readOperatorCredentialSecret(clusterName)
	if err != nil {
		secretName := operatorCredentialSecretNameForGroup(oc.MarklogicGroup)
		return oc.operatorUserPending("OperatorSecretUnavailable", fmt.Sprintf(
			"Cannot read operator credential Secret %q in namespace %q: %v. Verify it exists and contains a non-empty password key.",
			secretName, oc.MarklogicGroup.Namespace, err,
		))
	}

	useTLS := oc.MarklogicGroup.Spec.Tls != nil && oc.MarklogicGroup.Spec.Tls.EnableOnDefaultAppServers
	operatorClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host:               bootstrapHost,
		Username:           operatorUsername,
		Password:           operatorPassword,
		UseTLS:             useTLS,
		InsecureSkipVerify: useTLS,
	})
	hosts, err := operatorClient.ListHostsStatus(oc.Ctx)
	if err == nil {
		bootstrapHostFound := false
		for _, host := range hosts {
			if isBootstrapHostStatus(host.Name, bootstrapHost) {
				bootstrapHostFound = true
				if !host.Online {
					return oc.operatorUserPending("BootstrapHostNotReady", fmt.Sprintf(
						"Bootstrap host %q is not online yet; check the MarkLogic pod and server startup status.", bootstrapHost,
					))
				}
				if err := oc.ensureOperatorIdentity(operatorPassword, bootstrapHost, useTLS); err != nil {
					return oc.operatorUserPending("OperatorIdentityReconciliationFailed", fmt.Sprintf(
						"Could not reconcile the operator role and user through the bootstrap admin credentials: %v. Verify the admin Secret and MarkLogic Management API access.", err,
					))
				}
				return oc.markOperatorCredentialActive(clusterName)
			}
		}
		if !bootstrapHostFound {
			return oc.operatorUserPending("BootstrapHostNotFound", fmt.Sprintf(
				"Bootstrap host %q was not returned by the MarkLogic Management API; verify the bootstrap host configuration and cluster membership.", bootstrapHost,
			))
		}
	}
	if !isPermanentAuthError(err) {
		return oc.operatorUserPending("OperatorManagementAPIFailed", fmt.Sprintf(
			"Could not validate the operator user against bootstrap host %q: %v. Check host readiness and Management API connectivity.", bootstrapHost, err,
		))
	}

	adminSecretName := adminCredentialSecretNameForGroup(oc.MarklogicGroup)
	adminUsername, adminPassword, err := oc.readCredentialSecret(adminSecretName)
	if err != nil {
		return oc.operatorUserPending("BootstrapAdminSecretUnavailable", fmt.Sprintf(
			"Operator authentication failed and bootstrap admin Secret %q cannot be read: %v. Verify the Secret exists and contains username and password keys.", adminSecretName, err,
		))
	}
	adminClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host:               bootstrapHost,
		Username:           adminUsername,
		Password:           adminPassword,
		UseTLS:             useTLS,
		InsecureSkipVerify: useTLS,
	})
	hosts, err = adminClient.ListHostsStatus(oc.Ctx)
	if err != nil {
		return oc.operatorUserPending("BootstrapAdminAccessFailed", fmt.Sprintf(
			"Operator authentication failed and the bootstrap admin credentials could not access the Management API on %q: %v. Verify the admin credentials and host availability.", bootstrapHost, err,
		))
	}
	bootstrapOnline := false
	for _, host := range hosts {
		if isBootstrapHostStatus(host.Name, bootstrapHost) && host.Online {
			bootstrapOnline = true
			break
		}
	}
	if !bootstrapOnline {
		return oc.operatorUserPending("BootstrapHostNotReady", fmt.Sprintf(
			"Bootstrap host %q is not online according to the bootstrap admin credentials; check the MarkLogic pod and server startup status.", bootstrapHost,
		))
	}

	if err := oc.ensureOperatorIdentityWithClient(adminClient, operatorPassword); err != nil {
		return oc.operatorUserPending("OperatorIdentityReconciliationFailed", fmt.Sprintf(
			"Could not create or repair the operator role and user using bootstrap admin credentials: %v. Verify the admin Secret and MarkLogic Management API access.", err,
		))
	}
	return oc.operatorUserPending("OperatorIdentityReconciled", "The bootstrap admin credentials repaired the operator role and user; waiting for the next reconciliation to verify operator authentication.")
}

func (oc *OperatorContext) ensureOperatorIdentity(operatorPassword, bootstrapHost string, useTLS bool) error {
	adminUsername, adminPassword, err := oc.readCredentialSecret(adminCredentialSecretNameForGroup(oc.MarklogicGroup))
	if err != nil {
		return err
	}
	adminClient := NewDynamicManagementClient(mlmanage.ClientOptions{
		Host: bootstrapHost, Username: adminUsername, Password: adminPassword,
		UseTLS: useTLS, InsecureSkipVerify: useTLS,
	})
	return oc.ensureOperatorIdentityWithClient(adminClient, operatorPassword)
}

func (oc *OperatorContext) ensureOperatorIdentityWithClient(adminClient mlmanage.Client, operatorPassword string) error {
	if err := adminClient.EnsureOperatorRole(oc.Ctx); err != nil {
		return err
	}
	return adminClient.EnsureOperatorUser(oc.Ctx, operatorUsername, operatorPassword)
}

func adminCredentialSecretNameForGroup(group *marklogicv1.MarklogicGroup) string {
	if secretName := strings.TrimSpace(group.Spec.SecretName); secretName != "" {
		return secretName
	}
	return group.Name + "-admin"
}

func (oc *OperatorContext) markOperatorCredentialActive(clusterName string) result.ReconcileResult {
	secretName := operatorCredentialSecretNameForGroup(oc.MarklogicGroup)
	group := oc.MarklogicGroup
	previousSecretName := group.Status.CredentialSecretName
	patch := client.MergeFrom(group.DeepCopy())
	group.Status.CredentialSecretName = secretName
	changed := apiMeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               operatorCredentialsReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "OperatorCredentialsActive",
		Message:            fmt.Sprintf("Operator user %q authenticated successfully; Secret %q is active for Management API operations.", operatorUsername, secretName),
		ObservedGeneration: group.Generation,
	})
	if !changed && previousSecretName == secretName {
		return result.Continue()
	}
	if err := oc.Client.Status().Patch(oc.Ctx, group, patch); err != nil {
		return result.Error(err)
	}
	oc.recordOperatorCredentialsEvent(metav1.ConditionTrue, "OperatorCredentialsActive", "Operator credentials are active and verified.")
	return result.RequeueSoon(1)
}

func (oc *OperatorContext) readOperatorCredentialSecret(clusterName string) (string, error) {
	secretName := operatorCredentialSecretName(clusterName)
	if auth := oc.MarklogicGroup.Spec.Auth; auth != nil && auth.OperatorSecretName != nil && strings.TrimSpace(*auth.OperatorSecretName) != "" {
		secretName = strings.TrimSpace(*auth.OperatorSecretName)
	}
	secret := &corev1.Secret{}
	if err := oc.Client.Get(oc.Ctx, types.NamespacedName{Name: secretName, Namespace: oc.MarklogicGroup.Namespace}, secret); err != nil {
		return "", err
	}
	password := string(secret.Data["password"])
	if password == "" {
		return "", fmt.Errorf("operator credential Secret is missing password")
	}
	return password, nil
}

func operatorCredentialSecretNameForGroup(group *marklogicv1.MarklogicGroup) string {
	if auth := group.Spec.Auth; auth != nil && auth.OperatorSecretName != nil && strings.TrimSpace(*auth.OperatorSecretName) != "" {
		return strings.TrimSpace(*auth.OperatorSecretName)
	}
	for _, ownerRef := range group.OwnerReferences {
		if ownerRef.Kind == "MarklogicCluster" {
			return operatorCredentialSecretName(ownerRef.Name)
		}
	}
	if strings.HasSuffix(group.Spec.SecretName, "-admin") {
		return strings.TrimSuffix(group.Spec.SecretName, "-admin") + operatorCredentialSecretSuffix
	}
	return operatorCredentialSecretName(group.Name)
}

func (oc *OperatorContext) operatorUserPending(reason, message string) result.ReconcileResult {
	if err := oc.updateOperatorCredentialsCondition(metav1.ConditionFalse, reason, message); err != nil {
		return result.Error(err)
	}
	oc.ReqLogger.Info("MarkLogic operator user reconciliation is pending", "reason", reason, "message", message)
	return result.RequeueSoon(5)
}

func (oc *OperatorContext) updateOperatorCredentialsCondition(status metav1.ConditionStatus, reason, message string) error {
	group := oc.MarklogicGroup
	var previous *metav1.Condition
	if current := apiMeta.FindStatusCondition(group.Status.Conditions, operatorCredentialsReadyCondition); current != nil {
		previousCondition := *current
		previous = &previousCondition
	}
	patch := client.MergeFrom(group.DeepCopy())
	changed := apiMeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               operatorCredentialsReadyCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
	if !changed {
		return nil
	}
	if err := oc.Client.Status().Patch(oc.Ctx, group, patch); err != nil {
		return err
	}
	if previous == nil || previous.Status != status || previous.Reason != reason {
		oc.recordOperatorCredentialsEvent(status, reason, message)
	}
	return nil
}

func (oc *OperatorContext) recordOperatorCredentialsEvent(status metav1.ConditionStatus, reason, message string) {
	if oc.Recorder == nil {
		return
	}
	eventType := corev1.EventTypeWarning
	if status == metav1.ConditionTrue || reason == "OperatorIdentityReconciled" {
		eventType = corev1.EventTypeNormal
	}
	oc.Recorder.Event(oc.MarklogicGroup, eventType, reason, message)
}
