package k8sutil

import (
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (cc *ClusterContext) ReconcileSecret() result.ReconcileResult {
	logger := cc.ReqLogger
	client := cc.Client
	mlc := cc.MarklogicCluster

	if mlc.Spec.Auth != nil && mlc.Spec.Auth.SecretName != nil && *mlc.Spec.Auth.SecretName != "" {
		logger.Info("MarkLogic Secret is provided, skipping the creation")
		return result.Continue()
	}

	logger.Info("Reconciling MarkLogic Secret")
	labels := getCommonLabels(mlc.ObjectMeta.Name)
	annotations := getCommonAnnotations()
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
