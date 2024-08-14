package k8sutil

import (
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (oc *OperatorContext) ReconcileSecret() result.ReconcileResult {
	logger := oc.ReqLogger
	client := oc.Client
	cr := oc.MarklogicGroup

	logger.Info("Reconciling MarkLogic Secret")
	labels := getMarkLogicLabels(cr.Spec.Name)
	annotations := map[string]string{}
	secretName := cr.Spec.Name + "-admin"
	objectMeta := generateObjectMeta(secretName, cr.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	secret := &corev1.Secret{}
	err := client.Get(oc.Ctx, nsName, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic admin Secret is not found, creating a new one")
			secretData := oc.generateSecretData()
			secretDef := generateSecretDef(objectMeta, marklogicServerAsOwner(cr), secretData)
			err = oc.createSecret(secretDef)
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

func (oc *OperatorContext) generateSecretData() map[string][]byte {
	// logger := oc.ReqLogger
	spec := oc.MarklogicGroup.Spec
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

func (oc *OperatorContext) createSecret(secret *corev1.Secret) error {
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
