// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"crypto/rand"
	"math/big"
	"reflect"
	"strings"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	operatorCredentialSecretSuffix = "-operator"
	operatorUsername               = "marklogic-kubernetes-operator"
	operatorPasswordLength         = 32
)

func (cc *ClusterContext) ReconcileSecret() result.ReconcileResult {
	logger := cc.ReqLogger
	client := cc.Client
	mlc := cc.MarklogicCluster

	if mlc.Spec.Auth != nil && mlc.Spec.Auth.SecretName != nil && *mlc.Spec.Auth.SecretName != "" {
		logger.Info("MarkLogic Secret is provided, skipping the creation")
	} else {
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
			} else {
				logger.Error(err, "MarkLogic admin Secret creation is failed")
				return result.Error(err)
			}
		}
	}

	if operatorSecretResult := cc.reconcileOperatorCredentialSecret(); operatorSecretResult.Completed() {
		return operatorSecretResult
	}

	return result.Continue()
}

func operatorCredentialSecretName(clusterName string) string {
	return clusterName + operatorCredentialSecretSuffix
}

func (cc *ClusterContext) reconcileOperatorCredentialSecret() result.ReconcileResult {
	mlc := cc.MarklogicCluster
	if mlc.Spec.Auth != nil && mlc.Spec.Auth.OperatorSecretName != nil && strings.TrimSpace(*mlc.Spec.Auth.OperatorSecretName) != "" {
		return result.Continue()
	}

	secretName := operatorCredentialSecretName(mlc.Name)
	secretKey := types.NamespacedName{Name: secretName, Namespace: mlc.Namespace}
	secret := &corev1.Secret{}
	err := cc.Client.Get(cc.Ctx, secretKey, secret)
	if errors.IsNotFound(err) {
		password, passwordErr := generateOperatorPassword()
		if passwordErr != nil {
			cc.ReqLogger.Error(passwordErr, "Operator credential password generation failed")
			return result.Error(passwordErr)
		}
		labels := cc.GetClusterLabels(mlc.Name)
		annotations := cc.GetClusterAnnotations()
		objectMeta := generateObjectMeta(secretName, mlc.Namespace, labels, annotations)
		secret = generateSecretDef(objectMeta, marklogicClusterAsOwner(mlc), map[string][]byte{
			"username": []byte(operatorUsername),
			"password": []byte(password),
		})
		if err := cc.Client.Create(cc.Ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
			cc.ReqLogger.Error(err, "Operator credential Secret creation failed")
			return result.Error(err)
		}
		if err := cc.Client.Get(cc.Ctx, secretKey, secret); err != nil {
			return result.Error(err)
		}
	} else if err != nil {
		cc.ReqLogger.Error(err, "Operator credential Secret lookup failed")
		return result.Error(err)
	}

	changed := false
	if string(secret.Data["username"]) != operatorUsername {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["username"] = []byte(operatorUsername)
		changed = true
	}
	if len(secret.Data["password"]) == 0 {
		password, passwordErr := generateOperatorPassword()
		if passwordErr != nil {
			cc.ReqLogger.Error(passwordErr, "Operator credential password generation failed")
			return result.Error(passwordErr)
		}
		secret.Data["password"] = []byte(password)
		changed = true
	}

	ownerRef := marklogicClusterAsOwner(mlc)
	ownerRefs := make([]metav1.OwnerReference, 0, len(secret.OwnerReferences)+1)
	for _, existing := range secret.OwnerReferences {
		if existing.Kind != "MarklogicCluster" {
			ownerRefs = append(ownerRefs, existing)
		}
	}
	ownerRefs = append(ownerRefs, ownerRef)
	if !reflect.DeepEqual(secret.OwnerReferences, ownerRefs) {
		secret.OwnerReferences = ownerRefs
		changed = true
	}
	if changed {
		if err := cc.Client.Update(cc.Ctx, secret); err != nil {
			cc.ReqLogger.Error(err, "Operator credential Secret update failed")
			return result.Error(err)
		}
	}
	return result.Continue()
}

func generateOperatorPassword() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	password := make([]byte, operatorPasswordLength)
	for index := range password {
		value, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		password[index] = charset[value.Int64()]
	}
	return string(password), nil
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
