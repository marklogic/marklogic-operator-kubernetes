package k8sutil

import (
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (cc *ClusterContext) ReconcileServiceAccount() result.ReconcileResult {
	logger := cc.ReqLogger
	cr := cc.MarklogicCluster
	namespace := cr.Namespace
	saName := cr.Spec.ServiceAccountName

	// Skip if no service account name is specified
	if saName == "" {
		logger.Info("No ServiceAccount name specified, skipping reconciliation")
		return result.Continue()
	}

	namespacedName := types.NamespacedName{Name: saName, Namespace: namespace}
	sa := &corev1.ServiceAccount{}
	logger.Info("Reconciling ServiceAccount", "namespace", namespacedName.Namespace, "name", namespacedName.Name)
	err := cc.Client.Get(cc.Ctx, namespacedName, sa)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("ServiceAccount not found, creating a new one", "namespace", namespacedName.Namespace, "name", namespacedName.Name)
			saDef := generateServiceAccountDef(namespacedName, cr)
			err = cc.Client.Create(cc.Ctx, saDef)
			if err != nil {
				logger.Error(err, "Failed to create service account", "namespace", namespacedName.Namespace, "name", namespacedName.Name)
				return result.Error(err)
			}
			logger.Info("ServiceAccount created successfully", "namespace", namespacedName.Namespace, "name", namespacedName.Name)
		} else {
			logger.Error(err, "Failed to get ServiceAccount", "namespace", namespacedName.Namespace, "name", namespacedName.Name)
			return result.Error(err)
		}
	} else {
		logger.Info("ServiceAccount already exists")
	}

	return result.Continue()
}

func generateServiceAccountDef(namespacedName types.NamespacedName, cr *marklogicv1.MarklogicCluster) *corev1.ServiceAccount {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespacedName.Name,
			Namespace: namespacedName.Namespace,
		},
	}

	// Add owner reference for garbage collection
	if cr != nil {
		ownerRef := marklogicClusterAsOwner(cr)
		AddOwnerRefToObject(serviceAccount, ownerRef)
	}

	return serviceAccount
}
