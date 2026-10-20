// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"strings"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	credentialsActive := oc.MarklogicGroup.Status.CredentialSecretName != ""
	statefulSetExists := false
	var statefulSet *appsv1.StatefulSet
	if credentialsActive {
		statefulSet = &appsv1.StatefulSet{}
		err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: oc.MarklogicGroup.Spec.Name}, statefulSet)
		if err == nil {
			statefulSetExists = true
		} else if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
	}

	var pendingOperatorUserResult result.ReconcileResult
	if credentialsActive && statefulSetExists {
		if operatorUserResult := oc.ReconcileOperatorUser(); operatorUserResult.Completed() {
			if !oc.activeOperatorCredentialSecretMissing() {
				if err := oc.reconcileBootstrapStatefulSetRecovery(statefulSet); err != nil {
					return reconcile.Result{}, err
				}
				return operatorUserResult.Output()
			}
			pendingOperatorUserResult = operatorUserResult
		}
	}

	statefulSetResult, err := oc.ReconcileStatefulset()
	if err != nil {
		return statefulSetResult, err
	}
	if pendingOperatorUserResult != nil {
		return pendingOperatorUserResult.Output()
	}
	if !credentialsActive || !statefulSetExists {
		if operatorUserResult := oc.ReconcileOperatorUser(); operatorUserResult.Completed() {
			return operatorUserResult.Output()
		}
	}
	rotated, rotationErr := oc.rotateCredentialPodsIfNeeded()
	if rotationErr != nil {
		return reconcile.Result{}, rotationErr
	}
	if rotated && oc.MarklogicGroup.Spec.IsDynamic {
		if cleanupResult := oc.reconcileDynamicCredentialRotationCleanup(); cleanupResult.Completed() {
			return cleanupResult.Output()
		}
	}

	if oc.MarklogicGroup.Spec.IsDynamic {
		if dynamicResult := oc.ReconcileDynamicGroupConfig(); dynamicResult.Completed() {
			return dynamicResult.Output()
		}
	}
	if rotated {
		return result.RequeueSoon(5).Output()
	}

	return statefulSetResult, err
}

func (oc *OperatorContext) activeOperatorCredentialSecretMissing() bool {
	secretName := oc.MarklogicGroup.Status.CredentialSecretName
	if secretName == "" {
		secretName = operatorCredentialSecretNameForGroup(oc.MarklogicGroup)
	}
	secret := &corev1.Secret{}
	err := oc.Client.Get(oc.Ctx, client.ObjectKey{
		Namespace: oc.MarklogicGroup.Namespace,
		Name:      secretName,
	}, secret)
	return apierrors.IsNotFound(err)
}

func (oc *OperatorContext) reconcileBootstrapStatefulSetRecovery(statefulSet *appsv1.StatefulSet) error {
	if statefulSet == nil || strings.TrimSpace(oc.MarklogicGroup.Spec.BootstrapHost) != "" || oc.MarklogicGroup.Spec.Replicas == nil || *oc.MarklogicGroup.Spec.Replicas < 1 || statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 0 {
		return nil
	}
	patch := client.MergeFrom(statefulSet.DeepCopy())
	recoveryReplicas := int32(1)
	statefulSet.Spec.Replicas = &recoveryReplicas
	return oc.Client.Patch(oc.Ctx, statefulSet, patch)
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
