// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (oc *OperatorContext) ReconsileMarklogicGroupHandler() (reconcile.Result, error) {
	oc.ReqLogger.Info("handler::ReconsileMarklogicGroupHandler")

	if oc.MarklogicGroup.Spec.IsDynamic && oc.MarklogicGroup.DeletionTimestamp != nil {
		// During dynamic-group teardown, skip create/update reconcilers so
		// finalizer cleanup can complete even when the namespace is terminating.
		if dynamicResult := oc.ReconcileDynamicGroupConfig(); dynamicResult.Completed() {
			return dynamicResult.Output()
		}
		return reconcile.Result{Requeue: true}, nil
	}

	if result := oc.ReconcileServices(); result.Completed() {
		return result.Output()
	}
	err := setOperatorInternalStatus(oc, "Created")
	if err != nil {
		oc.ReqLogger.Error(err, "Failed to set operator internal status")
	}

	if result := oc.ReconcileConfigMap(); result.Completed() {
		return result.Output()
	}

	if oc.MarklogicGroup.Spec.LogCollection.Enabled {
		if result := oc.ReconcileFluentBitConfigMap(); result.Completed() {
			return result.Output()
		}
	}

	if result := oc.ReconcileVolumeResizeValidation(); result.Completed() {
		return result.Output()
	}

	result, err := oc.ReconcileStatefulset()
	if err != nil {
		return result, err
	}

	if oc.MarklogicGroup.Spec.IsDynamic {
		if dynamicResult := oc.ReconcileDynamicGroupConfig(); dynamicResult.Completed() {
			return dynamicResult.Output()
		}
	}

	return result, err
}

func (cc *ClusterContext) ReconsileMarklogicClusterHandler() (reconcile.Result, error) {
	outcome := &reconcileOutcome{}
	cc.reconcileClusterCore(outcome)
	// Object storage is independent of the core steps: it always runs once the cluster context exists.
	outcome.add(cc.ReconcileObjectStorage())
	return outcome.output()
}

// reconcileClusterCore keeps the dependencies between core steps but records every outcome
// instead of returning, so later independent work still runs.
func (cc *ClusterContext) reconcileClusterCore(outcome *reconcileOutcome) {
	if step := cc.ReconcileServiceAccount(); step.Completed() {
		outcome.addStep(step)
		return
	}
	if step := cc.ReconcileSecret(); step.Completed() {
		outcome.addStep(step)
		return
	}
	outcome.add(cc.ReconsileMarklogicCluster())
	if cc.MarklogicCluster.Spec.NetworkPolicy.Enabled {
		if step := cc.ReconcileNetworkPolicy(); step.Completed() {
			outcome.addStep(step)
			return
		}
	}
	if cc.MarklogicCluster.Spec.HAProxy != nil && cc.MarklogicCluster.Spec.HAProxy.Enabled {
		if step := cc.ReconcileHAProxy(); step.Completed() {
			outcome.addStep(step)
			return
		}
		if cc.MarklogicCluster.Spec.HAProxy.Ingress.Enabled {
			outcome.addStep(cc.ReconcileIngress())
		}
	}
}
