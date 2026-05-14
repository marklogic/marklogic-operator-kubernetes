// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const dynamicCredentialSecretSuffix = "-manage-admin"

func (cc *ClusterContext) ReconcileSecret() result.ReconcileResult {
	logger := cc.ReqLogger
	client := cc.Client
	mlc := cc.MarklogicCluster

	if mlc.Spec.Auth != nil && mlc.Spec.Auth.SecretName != nil && *mlc.Spec.Auth.SecretName != "" {
		logger.Info("MarkLogic Secret is provided, skipping the creation")
		return result.Continue()
	}

	logger.Info("Reconciling MarkLogic Secret")
	labels := cc.GetClusterLabels(mlc.ObjectMeta.Name)
	annotations := cc.GetClusterAnnotations()
	secretName := mlc.ObjectMeta.Name + "-admin"
	objectMeta := generateObjectMeta(secretName, mlc.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	secret := &corev1.Secret{}
	err := client.Get(cc.Ctx, nsName, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic admin Secret is not found, creating a new one")
			secretData := cc.generateSecretData()
			secretDef := generateSecretDef(objectMeta, marklogicClusterAsOwner(mlc), secretData)
			err = cc.createSecret(secretDef)
			if err != nil {
				logger.Info("MarkLogic admin Secret creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic admin Secret creation is successful")
			// result.Continue()
		} else {
			logger.Error(err, "MarkLogic admin Secret creation is failed")
			return result.Error(err)
		}
	}

	if hasDynamicGroups(mlc.Spec.MarkLogicGroups) {
		if dynamicSecretResult := cc.reconcileDynamicCredentialSecret(mlc.ObjectMeta.Name); dynamicSecretResult.Completed() {
			return dynamicSecretResult
		}
	}

	return result.Continue()
}

func hasDynamicGroups(groups []*marklogicv1.MarklogicGroups) bool {
	for _, group := range groups {
		if group != nil && group.IsDynamic {
			return true
		}
	}
	return false
}

func dynamicCredentialSecretName(clusterName string) string {
	return clusterName + dynamicCredentialSecretSuffix
}

func manageAdminUsername(clusterName string) string {
	return clusterName + "-manage-admin"
}

func generateDynamicSecretData(clusterName string) map[string][]byte {
	return map[string][]byte{
		"username": []byte(manageAdminUsername(clusterName)),
		"password": []byte(generateRandomAlphaNumeric(16)),
	}
}

func (cc *ClusterContext) reconcileDynamicCredentialSecret(clusterName string) result.ReconcileResult {
	logger := cc.ReqLogger
	client := cc.Client

	secretName := dynamicCredentialSecretName(clusterName)
	labels := cc.GetClusterLabels(clusterName)
	annotations := cc.GetClusterAnnotations()
	objectMeta := generateObjectMeta(secretName, cc.MarklogicCluster.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}

	secret := &corev1.Secret{}
	err := client.Get(cc.Ctx, nsName, secret)
	if err == nil {
		return result.Continue()
	}
	if !errors.IsNotFound(err) {
		logger.Error(err, "MarkLogic manage-admin Secret reconcile failed")
		return result.Error(err)
	}

	logger.Info("MarkLogic manage-admin Secret is not found, creating a new one")
	secretData := generateDynamicSecretData(clusterName)
	secretDef := generateSecretDef(objectMeta, marklogicClusterAsOwner(cc.MarklogicCluster), secretData)
	if err := cc.createSecret(secretDef); err != nil {
		logger.Error(err, "MarkLogic manage-admin Secret creation failed")
		return result.Error(err)
	}

	logger.Info("MarkLogic manage-admin Secret creation is successful")
	return result.Continue()
}

func (cc *ClusterContext) generateSecretData() map[string][]byte {
	// logger := oc.ReqLogger
	spec := cc.MarklogicCluster.Spec
	secretData := map[string][]byte{}
	if spec.Auth != nil && spec.Auth.AdminUsername != nil {
		secretData["username"] = []byte(*spec.Auth.AdminUsername)
	} else {
		secretData["username"] = []byte(generateRandomAlphaNumeric(5))
	}

	if spec.Auth != nil && spec.Auth.AdminPassword != nil {
		secretData["password"] = []byte(*spec.Auth.AdminPassword)
	} else {
		secretData["password"] = []byte(generateRandomAlphaNumeric(10))
	}

	if spec.Auth != nil && spec.Auth.WalletPassword != nil {
		secretData["wallet-password"] = []byte(*spec.Auth.WalletPassword)
	} else {
		secretData["wallet-password"] = []byte(generateRandomAlphaNumeric(10))
	}

	return secretData
}

func generateSecretDef(secretMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference, secretData map[string][]byte) *corev1.Secret {
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: secretMeta,
		Type:       corev1.SecretTypeOpaque,
		Data:       secretData,
	}
	secret.SetOwnerReferences(append(secret.GetOwnerReferences(), ownerRef))
	return secret
}

func (oc *ClusterContext) createSecret(secret *corev1.Secret) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, secret)
	if err != nil {
		logger.Error(err, "MarkLogic admin secret creation is failed")
		return err
	}
	logger.Info("MarkLogic script admin secret is successful")
	return nil
}
