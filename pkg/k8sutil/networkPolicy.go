package k8sutil

import (
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
)

func generateNetworkPolicyDef(networkPolicyMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference, cr *databasev1alpha1.MarklogicCluster) *networkingv1.NetworkPolicy {
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

func (cc *ClusterContext) getNetworkPolicy(namespace string, networkPolicyName string) (*networkingv1.NetworkPolicy, error) {
	logger := cc.ReqLogger

	var networkPolicy *networkingv1.NetworkPolicy
	networkPolicy = &networkingv1.NetworkPolicy{}
	err := cc.Client.Get(cc.Ctx, types.NamespacedName{Name: networkPolicyName, Namespace: namespace}, networkPolicy)
	if err != nil {
		logger.Info("MarkLogic NetworkPolicy get action failed")
		return nil, err
	}
	logger.Info("MarkLogic NetworkPolicy get action is successful")
	return networkPolicy, nil
}

func generateNetworkPolicy(networkPolicyName string, cr *databasev1alpha1.MarklogicCluster) *networkingv1.NetworkPolicy {
	labels := getMarkLogicLabels(cr.GetObjectMeta().GetName())
	netObjectMeta := generateObjectMeta(networkPolicyName, cr.Namespace, labels, map[string]string{})
	networkPolicy := generateNetworkPolicyDef(netObjectMeta, marklogicClusterAsOwner(cr), cr)
	return networkPolicy
}

func (cc *ClusterContext) ReconcileNetworkPolicy() result.ReconcileResult {
	logger := cc.ReqLogger
	logger.Info("NetworkPolicy::Reconciling MarkLogic NetworkPolicy")
	client := cc.Client
	cr := cc.MarklogicCluster
	networkPolicyName := cr.ObjectMeta.Name
	currentNetworkPolicy, err := cc.getNetworkPolicy(cr.Namespace, networkPolicyName)
	networkPolicyDef := generateNetworkPolicy(networkPolicyName, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic NetworkPolicy not found, creating a new one")
			err = client.Create(cc.Ctx, networkPolicyDef)
			if err != nil {
				logger.Info("MarkLogic NetworkPolicy creation has failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic NetworkPolicy creation is successful")
		} else {
			logger.Error(err, "MarkLogic NetworkPolicy creation has failed")
			return result.Error(err)
		}
	} else {
		logger.Info("MarkLogic NetworkPolicy already exists")
		patchDiff, err := patch.DefaultPatchMaker.Calculate(currentNetworkPolicy, networkPolicyDef,
			patch.IgnoreStatusFields(),
			patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
			patch.IgnoreField("kind"))
		if err != nil {
			logger.Error(err, "Error calculating patch")
			return result.Error(err)
		}
		if !patchDiff.IsEmpty() {
			logger.Info("MarkLogic NetworkPolicy spec is different from the input NetworkPolicy spec, updating the NetworkPolicy")
			logger.Info(patchDiff.String())
			err := cc.Client.Update(cc.Ctx, networkPolicyDef)
			if err != nil {
				logger.Error(err, "Error updating NetworkPolicy")
				return result.Error(err)
			}
		} else {
			logger.Info("MarkLogic NetworkPolicy spec is the same as the input NetworkPolicy spec")

		}
		logger.Info("MarkLogic NetworkPolicy is updated")
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
