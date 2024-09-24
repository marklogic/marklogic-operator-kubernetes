package k8sutil

import (
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (oc *OperatorContext) ReconsileMarklogicGroupHandler() (reconcile.Result, error) {
	oc.ReqLogger.Info("handler::ReconsileMarklogicGroupHandler")

	if result := oc.ReconcileServices(); result.Completed() {
		return result.Output()
	}
	setOperatorInternalStatus(oc, "Created")

	if result := oc.ReconcileSecret(); result.Completed() {
		return result.Output()
	}

	if result := oc.ReconcileConfigMap(); result.Completed() {
		return result.Output()
	}

	if oc.MarklogicGroup.Spec.LogCollection.Enabled {
		if result := oc.ReconcileFluentBitConfigMap(); result.Completed() {
			return result.Output()
		}
	}

	if oc.MarklogicGroup.Spec.NetworkPolicy.Enabled {
		if result := oc.ReconcileNetworkPolicy(); result.Completed() {
			return result.Output()
		}
	}

	result, err := oc.ReconcileStatefulset()

	return result, err
}

func (cc *ClusterContext) ReconsileMarklogicClusterHandler() (reconcile.Result, error) {
	if cc.MarklogicCluster.Spec.HAProxy.Enabled {
		if result := cc.ReconcileHAProxy(); result.Completed() {
			return result.Output()
		}
	}
	result, err := cc.ReconsileMarklogicCluster()
	return result, err
}
