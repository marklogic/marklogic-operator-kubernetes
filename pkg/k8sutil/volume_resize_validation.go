// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	resizePauseAnnotationKey = "marklogic.progress.com/resize-paused"
	dataDirPVCName           = "datadir"
)

type resizePVCDiscovery struct {
	expectedNames []string
	foundPVCs     map[string]*corev1.PersistentVolumeClaim
	missingPVCs   []string
	notBoundPVCs  []string
	minSize       *resource.Quantity
}

func (oc *OperatorContext) ReconcileVolumeResizeValidation() result.ReconcileResult {
	cr := oc.MarklogicGroup
	if cr == nil || cr.Spec.Persistence == nil || !cr.Spec.Persistence.Enabled {
		return result.Continue()
	}

	targetSize, err := resource.ParseQuantity(cr.Spec.Persistence.Size)
	if err != nil {
		return oc.failResizeValidation(marklogicv1.VolumeResizeReasonInvalidResizeRequest, fmt.Sprintf("Invalid persistence size %q", cr.Spec.Persistence.Size))
	}

	currentSts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet and PVCs are not ready yet, keep existing reconcile flow.
			return result.Continue()
		}
		return result.Error(err)
	}

	pvcState, err := oc.discoverPrimaryPVCs(currentSts)
	if err != nil {
		return result.Error(err)
	}
	if pvcState.minSize == nil {
		// No live baseline yet, continue with the existing reconcile flow.
		return result.Continue()
	}

	active := cr.Status.VolumeResizeStatus
	if isResizeOperationActive(active) {
		return oc.reconcileActiveResizeOperation(active, targetSize)
	}

	comparison := targetSize.Cmp(*pvcState.minSize)
	if comparison == 0 {
		return result.Continue()
	}

	resizeStatus := oc.newResizeStatus(pvcState, targetSize.String())
	claimed, err := oc.claimResizeStatusCAS(resizeStatus)
	if err != nil {
		return result.Error(err)
	}
	if !claimed {
		if isResizeOperationActive(oc.MarklogicGroup.Status.VolumeResizeStatus) {
			return oc.reconcileActiveResizeOperation(oc.MarklogicGroup.Status.VolumeResizeStatus, targetSize)
		}
		return result.Continue()
	}

	if comparison < 0 {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonShrinkNotSupported, fmt.Sprintf("Shrink is not supported: current=%s target=%s", pvcState.minSize.String(), targetSize.String()))
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeRequested", fmt.Sprintf("Resize requested from %s to %s", resizeStatus.CurrentSize, resizeStatus.TargetSize))

	if isResizePaused(cr) {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseStalled, marklogicv1.VolumeResizeReasonPaused, "Resize is paused by annotation marklogic.progress.com/resize-paused")
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if cr.Spec.UpdateStrategy != appsv1.OnDeleteStatefulSetStrategyType {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonInvalidResizeRequest, "Resize requires spec.updateStrategy=OnDelete")
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if len(pvcState.missingPVCs) > 0 {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseStalled, marklogicv1.VolumeResizeReasonPVCNotBound, fmt.Sprintf("Target PVCs not found: %s", strings.Join(pvcState.missingPVCs, ",")))
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if len(pvcState.notBoundPVCs) > 0 {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseStalled, marklogicv1.VolumeResizeReasonPVCNotBound, fmt.Sprintf("PVCs are not Bound: %s", strings.Join(pvcState.notBoundPVCs, ",")))
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	if err := oc.validateStorageClassExpansionAllowed(pvcState.foundPVCs); err != nil {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonStorageClassNotExpandable, err.Error())
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	resizeStatus.Message = "Resize validation completed"
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", resizeStatus.Message)
	if err := oc.patchResizeStatus(resizeStatus); err != nil {
		return result.Error(err)
	}
	return result.Done()
}

func (oc *OperatorContext) failResizeValidation(reason marklogicv1.VolumeResizeReason, message string) result.ReconcileResult {
	resizeStatus := oc.MarklogicGroup.Status.VolumeResizeStatus
	if resizeStatus == nil {
		now := metav1.Now()
		resizeStatus = &marklogicv1.VolumeResizeStatus{
			OperationID:        "resize-" + generateRandomAlphaNumeric(10),
			ObservedGeneration: oc.MarklogicGroup.Generation,
			FirstStartedTime:   &now,
			LastTransitionTime: &now,
			CurrentSize:        "",
			TargetSize:         oc.MarklogicGroup.Spec.Persistence.Size,
			ResizeStrategy:     resolveResizeStrategy(oc.MarklogicGroup.Spec.Persistence),
			Phase:              marklogicv1.VolumeResizePhaseValidating,
		}
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseFailed, reason, message)
		claimed, err := oc.claimResizeStatusCAS(resizeStatus)
		if err != nil {
			return result.Error(err)
		}
		if !claimed {
			return result.Continue()
		}
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", message)
		return result.Done()
	}
	oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseFailed, reason, message)
	oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", message)
	if err := oc.patchResizeStatus(resizeStatus); err != nil {
		return result.Error(err)
	}
	return result.Done()
}

func (oc *OperatorContext) reconcileActiveResizeOperation(active *marklogicv1.VolumeResizeStatus, targetSize resource.Quantity) result.ReconcileResult {
	if active == nil {
		return result.Done()
	}

	updated := false
	if isResizePaused(oc.MarklogicGroup) {
		message := "Resize is paused by annotation marklogic.progress.com/resize-paused"
		if active.Phase != marklogicv1.VolumeResizePhaseStalled || active.Reason != marklogicv1.VolumeResizeReasonPaused || active.Message != message {
			oc.transitionResizePhase(active, marklogicv1.VolumeResizePhaseStalled, marklogicv1.VolumeResizeReasonPaused, message)
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", message)
			updated = true
		}
	}

	currentTarget, err := resource.ParseQuantity(active.TargetSize)
	if err == nil && targetSize.Cmp(currentTarget) > 0 {
		newDeferredTarget := targetSize.String()
		if active.DeferredTargetSize != newDeferredTarget || active.DeferredObservedGeneration != oc.MarklogicGroup.Generation {
			active.DeferredTargetSize = newDeferredTarget
			active.DeferredObservedGeneration = oc.MarklogicGroup.Generation
			oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", fmt.Sprintf("Deferred newer resize target %s while operation %s is active", newDeferredTarget, active.OperationID))
			updated = true
		}
	}

	if updated {
		if err := oc.patchResizeStatus(active); err != nil {
			return result.Error(err)
		}
	}

	return result.Done()
}

func (oc *OperatorContext) newResizeStatus(pvcState *resizePVCDiscovery, targetSize string) *marklogicv1.VolumeResizeStatus {
	now := metav1.Now()
	return &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-" + generateRandomAlphaNumeric(10),
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseValidating,
		Message:            "Validating resize request",
		CurrentSize:        pvcState.minSize.String(),
		TargetSize:         targetSize,
		ResizeStrategy:     resolveResizeStrategy(oc.MarklogicGroup.Spec.Persistence),
		TotalPVCs:          int32(len(pvcState.expectedNames)),
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
	}
}

func (oc *OperatorContext) transitionResizePhase(status *marklogicv1.VolumeResizeStatus, phase marklogicv1.VolumeResizePhase, reason marklogicv1.VolumeResizeReason, message string) {
	now := metav1.Now()
	if status.Phase != phase {
		status.LastTransitionTime = &now
	}
	status.Phase = phase
	status.Reason = reason
	status.Message = message
	if status.LastTransitionTime == nil {
		status.LastTransitionTime = &now
	}
	if phase == marklogicv1.VolumeResizePhaseFailed || phase == marklogicv1.VolumeResizePhaseCompleted {
		status.CompletionTime = &now
	}
}

func (oc *OperatorContext) patchResizeStatus(status *marklogicv1.VolumeResizeStatus) error {
	latest := &marklogicv1.MarklogicGroup{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: oc.MarklogicGroup.Name, Namespace: oc.MarklogicGroup.Namespace}, latest); err != nil {
		return err
	}

	patchClient := client.MergeFrom(latest.DeepCopy())
	latest.Status.VolumeResizeStatus = status
	if err := oc.Client.Status().Patch(oc.Ctx, latest, patchClient); err != nil {
		return err
	}

	oc.MarklogicGroup.Status.VolumeResizeStatus = status
	return nil
}

func (oc *OperatorContext) claimResizeStatusCAS(status *marklogicv1.VolumeResizeStatus) (bool, error) {
	expectedResourceVersion := oc.MarklogicGroup.ResourceVersion
	latest := &marklogicv1.MarklogicGroup{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: oc.MarklogicGroup.Name, Namespace: oc.MarklogicGroup.Namespace}, latest); err != nil {
		return false, err
	}
	if expectedResourceVersion == "" {
		expectedResourceVersion = latest.ResourceVersion
	}

	if isResizeOperationActive(latest.Status.VolumeResizeStatus) {
		oc.MarklogicGroup = latest
		return false, nil
	}

	claim := latest.DeepCopy()
	claim.ResourceVersion = expectedResourceVersion
	claim.Status.VolumeResizeStatus = status
	if err := oc.Client.Status().Update(oc.Ctx, claim); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}

	oc.MarklogicGroup.ResourceVersion = claim.ResourceVersion
	oc.MarklogicGroup.Status.VolumeResizeStatus = status
	return true, nil
}

func (oc *OperatorContext) emitResizeEvent(eventType, reason, message string) {
	if oc.Recorder == nil {
		return
	}
	oc.Recorder.Event(oc.MarklogicGroup, eventType, reason, message)
}

func isResizePaused(group *marklogicv1.MarklogicGroup) bool {
	if group == nil {
		return false
	}
	if group.Annotations == nil {
		return false
	}
	value, ok := group.Annotations[resizePauseAnnotationKey]
	if !ok {
		return false
	}
	return strings.EqualFold(value, "true")
}

func isResizeOperationActive(status *marklogicv1.VolumeResizeStatus) bool {
	if status == nil {
		return false
	}
	return status.Phase != marklogicv1.VolumeResizePhaseCompleted && status.Phase != marklogicv1.VolumeResizePhaseFailed
}

func resolveResizeStrategy(persistence *marklogicv1.Persistence) marklogicv1.VolumeResizeStrategy {
	if persistence != nil && persistence.ResizeStrategy == marklogicv1.VolumeResizeStrategySequential {
		return marklogicv1.VolumeResizeStrategySequential
	}
	return marklogicv1.VolumeResizeStrategyParallel
}

func (oc *OperatorContext) discoverPrimaryPVCs(sts *appsv1.StatefulSet) (*resizePVCDiscovery, error) {
	state := &resizePVCDiscovery{
		expectedNames: []string{},
		foundPVCs:     map[string]*corev1.PersistentVolumeClaim{},
		missingPVCs:   []string{},
		notBoundPVCs:  []string{},
	}

	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}

	for i := int32(0); i < replicas; i++ {
		name := fmt.Sprintf("%s-%s-%d", dataDirPVCName, sts.Name, i)
		state.expectedNames = append(state.expectedNames, name)
		pvc := &corev1.PersistentVolumeClaim{}
		err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: sts.Namespace, Name: name}, pvc)
		if err != nil {
			if apierrors.IsNotFound(err) {
				state.missingPVCs = append(state.missingPVCs, name)
				continue
			}
			return nil, err
		}
		state.foundPVCs[name] = pvc
		if pvc.Status.Phase != corev1.ClaimBound {
			state.notBoundPVCs = append(state.notBoundPVCs, name)
		}

		capacity := pvc.Status.Capacity[corev1.ResourceStorage]
		if capacity.IsZero() {
			capacity = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		}
		if capacity.IsZero() {
			continue
		}
		capCopy := capacity.DeepCopy()
		if state.minSize == nil || capCopy.Cmp(*state.minSize) < 0 {
			state.minSize = &capCopy
		}
	}

	return state, nil
}

func (oc *OperatorContext) validateStorageClassExpansionAllowed(foundPVCs map[string]*corev1.PersistentVolumeClaim) error {
	seen := map[string]struct{}{}
	for pvcName, pvc := range foundPVCs {
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
			return fmt.Errorf("PVC %s has no storageClassName", pvcName)
		}
		scName := *pvc.Spec.StorageClassName
		if _, ok := seen[scName]; ok {
			continue
		}
		seen[scName] = struct{}{}

		sc := &storagev1.StorageClass{}
		if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: scName}, sc); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("StorageClass %s not found", scName)
			}
			return err
		}
		if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
			return fmt.Errorf("StorageClass %s does not allow volume expansion", scName)
		}
	}
	return nil
}
