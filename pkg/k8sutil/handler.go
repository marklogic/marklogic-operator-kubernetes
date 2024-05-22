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
	result, err := oc.ReconcileStatefulset()

	return result, err
}
