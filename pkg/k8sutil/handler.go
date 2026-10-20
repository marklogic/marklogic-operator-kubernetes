// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (oc *OperatorContext) ReconsileMarklogicGroupHandler() (outcome reconcile.Result, reconcileErr error) {
	defer func() {
		unusedResult, unusedErr := oc.observeUnusedPrecreatedPVCs()
		progressResult, progressErr := oc.observeVolumeAttributesClasses(reconcileErr)
		for _, observation := range []reconcile.Result{unusedResult, progressResult} {
			if observation.RequeueAfter > 0 && (outcome.RequeueAfter == 0 || observation.RequeueAfter < outcome.RequeueAfter) {
				outcome.RequeueAfter = observation.RequeueAfter
			}
		}
		reconcileErr = errors.Join(reconcileErr, unusedErr, progressErr)
	}()
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

	vacResult, vacErr := oc.ReconcileVolumeAttributesClasses()
	if oc.MarklogicGroup.Spec.IsDynamic {
		if dynamicResult := oc.ReconcileDynamicGroupConfig(); dynamicResult.Completed() {
			result, err = dynamicResult.Output()
		}
	}

	if vacResult.RequeueAfter > 0 && (result.RequeueAfter == 0 || vacResult.RequeueAfter < result.RequeueAfter) {
		result.RequeueAfter = vacResult.RequeueAfter
	}
	return result, errors.Join(err, vacErr)
}

func (cc *ClusterContext) ReconsileMarklogicClusterHandler() (outcome reconcile.Result, reconcileErr error) {
	defer func() {
		progressResult, progressErr := cc.observeVolumeAttributesClasses()
		if progressResult.RequeueAfter > 0 && (outcome.RequeueAfter == 0 || progressResult.RequeueAfter < outcome.RequeueAfter) {
			outcome.RequeueAfter = progressResult.RequeueAfter
		}
		reconcileErr = errors.Join(reconcileErr, progressErr)
	}()
	if result := cc.ReconcileServiceAccount(); result.Completed() {
		return result.Output()
	}
	if result := cc.ReconcileSecret(); result.Completed() {
		return result.Output()
	}
	result, err := cc.ReconsileMarklogicCluster()
	if cc.MarklogicCluster.Spec.NetworkPolicy.Enabled {
		if result := cc.ReconcileNetworkPolicy(); result.Completed() {
			return result.Output()
		}
	}
	if cc.MarklogicCluster.Spec.HAProxy != nil && cc.MarklogicCluster.Spec.HAProxy.Enabled {
		if result := cc.ReconcileHAProxy(); result.Completed() {
			return result.Output()
		}
		if cc.MarklogicCluster.Spec.HAProxy.Ingress.Enabled {
			if result := cc.ReconcileIngress(); result.Completed() {
				return result.Output()
			}
		}
	}
	return result, err
}
