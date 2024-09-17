package k8sutil

import (
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
)

func generateNetworkPolicyDef(networkPolicyMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference, cr *databasev1alpha1.MarklogicGroup) *networkingv1.NetworkPolicy {
	networkPolicySpec := networkingv1.NetworkPolicySpec{
		PolicyTypes: cr.Spec.NetworkPolicy.PolicyTypes,
		PodSelector: cr.Spec.NetworkPolicy.PodSelector,
		Ingress:     cr.Spec.NetworkPolicy.Ingress,
		Egress:      cr.Spec.NetworkPolicy.Egress,
	}
	networkPolicyDef := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NetworkPolicy",
			APIVersion: "v1",
		},
		ObjectMeta: networkPolicyMeta,
		Spec:       networkPolicySpec,
	}
	networkPolicyDef.SetOwnerReferences(append(networkPolicyDef.GetOwnerReferences(), ownerRef))
	return networkPolicyDef
}

func (oc *OperatorContext) getNetworkPolicy(namespace string, networkPolicyName string) (*networkingv1.NetworkPolicy, error) {
	logger := oc.ReqLogger

	var networkPolicy *networkingv1.NetworkPolicy
	err := oc.Client.Get(oc.Ctx, types.NamespacedName{Name: networkPolicyName, Namespace: namespace}, networkPolicy)
	if err != nil {
		logger.Info("MarkLogic NetworkPolicy get action failed")
		return nil, err
	}
	logger.Info("MarkLogic NetworkPolicy get action is successful")
	return networkPolicy, nil
}

func generateNetworkPolicy(networkPolicyName string, cr *databasev1alpha1.MarklogicGroup) *networkingv1.NetworkPolicy {
	labels := getMarkLogicLabels(cr.Spec.Name)
	netObjectMeta := generateObjectMeta(networkPolicyName, cr.Namespace, labels, map[string]string{})
	networkPolicy := generateNetworkPolicyDef(netObjectMeta, marklogicServerAsOwner(cr), cr)
	return networkPolicy
}

func (oc *OperatorContext) ReconcileNetworkPolicy() result.ReconcileResult {
	logger := oc.ReqLogger
	logger.Info("NetworkPolicy::Reconciling MarkLogic NetworkPolicy")
	client := oc.Client
	cr := oc.MarklogicGroup
	networkPolicy := &networkingv1.NetworkPolicy{}
	networkPolicyName := cr.Spec.Name + "-network-policy"
	networkPolicyNsName := types.NamespacedName{Name: networkPolicyName, Namespace: cr.Namespace}
	err := client.Get(oc.Ctx, networkPolicyNsName, networkPolicy)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic NetworkPolicy not found, creating a new one")
			networkPolicy = generateNetworkPolicy(networkPolicyName, cr)
			err = client.Create(oc.Ctx, networkPolicy)
			if err != nil {
				logger.Info("MarkLogic NetworkPolicy creation has failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic NetworkPolicy creation is successful")
		} else {
			logger.Error(err, "MarkLogic NetworkPolicy creation has failed")
			return result.Error(err)
		}
	}

	return result.Continue()
}

func (oc *OperatorContext) createNetworkPolicy(namespace string, networkPolicy *networkingv1.NetworkPolicy) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, networkPolicy)
	if err != nil {
		logger.Error(err, "MarkLogic NetworkPolicy creation has failed")
		return err
	}
	logger.Info("MarkLogic NetworkPolicy creation is successful")
	return nil
}
