package k8sutil

import (
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (oc *OperatorContext) ReconsileHandler() (reconcile.Result, error) {
	oc.ReqLogger.Info("handler::ReconsileHandler")

	if result := oc.ReconcileSrvices(); result.Completed() {
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

	result, err := oc.ReconcileStatefulset()

	return result, err
}
