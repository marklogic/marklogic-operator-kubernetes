// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
		return oc.operatorUserPending()
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
		for _, host := range hosts {
			if isBootstrapHostStatus(host.Name, bootstrapHost) {
				if !host.Online {
					return result.RequeueSoon(5)
				}
				if err := oc.ensureOperatorIdentity(operatorPassword, bootstrapHost, useTLS); err != nil {
					return oc.operatorUserPending()
				}
				return oc.markOperatorCredentialActive(clusterName)
			}
		}
		return result.RequeueSoon(5)
	}
	if !isPermanentAuthError(err) {
		return oc.operatorUserPending()
	}

	adminSecretName := adminCredentialSecretNameForGroup(oc.MarklogicGroup)
	adminUsername, adminPassword, err := oc.readCredentialSecret(adminSecretName)
	if err != nil {
		return oc.operatorUserPending()
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
		return oc.operatorUserPending()
	}
	bootstrapOnline := false
	for _, host := range hosts {
		if isBootstrapHostStatus(host.Name, bootstrapHost) && host.Online {
			bootstrapOnline = true
			break
		}
	}
	if !bootstrapOnline {
		return result.RequeueSoon(5)
	}

	if err := oc.ensureOperatorIdentityWithClient(adminClient, operatorPassword); err != nil {
		return oc.operatorUserPending()
	}
	return result.RequeueSoon(1)
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
	if oc.MarklogicGroup.Status.CredentialSecretName == secretName {
		return result.Continue()
	}
	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	oc.MarklogicGroup.Status.CredentialSecretName = secretName
	if err := oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch); err != nil {
		return result.Error(err)
	}
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

func (oc *OperatorContext) operatorUserPending() result.ReconcileResult {
	oc.ReqLogger.Info("MarkLogic operator user reconciliation is pending")
	return result.RequeueSoon(5)
}
