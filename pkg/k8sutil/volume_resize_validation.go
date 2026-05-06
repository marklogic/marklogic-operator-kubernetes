// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"strings"
	"time"

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
	resizeRetryDelaySeconds  = 15
	resizeMaxRetries         = 3
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

	active := cr.Status.VolumeResizeStatus
	if isResizeOperationActive(active) {
		return oc.reconcileActiveResizeOperation(active, targetSize, currentSts)
	}

	if pvcState.minSize == nil {
		// No live baseline yet, continue with the existing reconcile flow.
		return result.Continue()
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
			return oc.reconcileActiveResizeOperation(oc.MarklogicGroup.Status.VolumeResizeStatus, targetSize, currentSts)
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

	oc.initializePVCStatuses(resizeStatus, pvcState)
	oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseResizingPVCs, "", "Resize validation completed; submitting PVC resize requests")
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", resizeStatus.Message)
	if err := oc.patchResizeStatus(resizeStatus); err != nil {
		return result.Error(err)
	}
	return result.RequeueSoon(1)
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

func (oc *OperatorContext) reconcileActiveResizeOperation(active *marklogicv1.VolumeResizeStatus, targetSize resource.Quantity, currentSts *appsv1.StatefulSet) result.ReconcileResult {
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
		if updated {
			if err := oc.patchResizeStatus(active); err != nil {
				return result.Error(err)
			}
		}
		return result.Done()
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

	switch active.Phase {
	case marklogicv1.VolumeResizePhaseValidating:
		oc.transitionResizePhase(active, marklogicv1.VolumeResizePhaseResizingPVCs, "", "Submitting PVC resize requests")
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", active.Message)
		if err := oc.patchResizeStatus(active); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(1)
	case marklogicv1.VolumeResizePhaseResizingPVCs:
		return oc.processResizeSubmission(active)
	case marklogicv1.VolumeResizePhaseWaitingForPVCResize:
		return oc.processResizeWaiting(active)
	case marklogicv1.VolumeResizePhaseStalled:
		ready, requeueSecs := isRetryWindowOpen(active)
		if !ready {
			return result.RequeueSoon(requeueSecs)
		}
		active.NextRetryTime = nil
		oc.transitionResizePhase(active, marklogicv1.VolumeResizePhaseResizingPVCs, "", "Retrying PVC resize operation")
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", active.Message)
		if err := oc.patchResizeStatus(active); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(1)
	default:
		_ = currentSts
		return result.Done()
	}
}

func (oc *OperatorContext) processResizeSubmission(status *marklogicv1.VolumeResizeStatus) result.ReconcileResult {
	ready, requeueSecs := isRetryWindowOpen(status)
	if !ready {
		return result.RequeueSoon(requeueSecs)
	}

	targetSize, err := resource.ParseQuantity(status.TargetSize)
	if err != nil {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonInvalidResizeRequest, fmt.Sprintf("Invalid target size %q", status.TargetSize))
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	if len(status.PVCStatuses) == 0 {
		currentSts, stsErr := oc.GetStatefulSet(oc.MarklogicGroup.Namespace, oc.MarklogicGroup.Spec.Name)
		if stsErr != nil {
			return result.Error(stsErr)
		}
		pvcState, discoverErr := oc.discoverPrimaryPVCs(currentSts)
		if discoverErr != nil {
			return result.Error(discoverErr)
		}
		oc.initializePVCStatuses(status, pvcState)
	}

	indices := oc.getSubmissionIndices(status)
	for _, idx := range indices {
		entry := &status.PVCStatuses[idx]
		if isPVCCheckpointed(entry) {
			continue
		}

		pvc := &corev1.PersistentVolumeClaim{}
		if getErr := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: entry.Name}, pvc); getErr != nil {
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonResizeFailed, getErr.Error())
			continue
		}

		requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		observed := pvc.Status.Capacity[corev1.ResourceStorage]
		if observed.IsZero() {
			observed = requested
		}
		entry.RequestedSize = requested.String()
		entry.ObservedCapacity = observed.String()

		if requested.Cmp(targetSize) >= 0 {
			entry.State = marklogicv1.PVCResizeStateWaitingForCheckpoint
			entry.LastReason = ""
			entry.LastMessage = "Waiting for resize checkpoint"
			continue
		}

		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = targetSize
		if patchErr := oc.Client.Patch(oc.Ctx, pvc, patch); patchErr != nil {
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonResizeFailed, patchErr.Error())
			continue
		}

		entry.RequestedSize = targetSize.String()
		entry.State = marklogicv1.PVCResizeStateResizeSubmitted
		entry.LastReason = ""
		entry.LastMessage = "Resize request submitted"
	}

	oc.updateSequentialActivePVC(status)
	oc.recalculatePVCProgress(status)

	if len(status.FailedPVCs) > 0 {
		if oc.scheduleRetryOrFail(status, marklogicv1.VolumeResizeReasonPartialResizeFailure, "Failed to submit one or more PVC resize patches") {
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", status.Message)
			if patchErr := oc.patchResizeStatus(status); patchErr != nil {
				return result.Error(patchErr)
			}
			return result.RequeueSoon(resizeRetryDelaySeconds)
		}
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	oc.clearRetryState(status)
	oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseWaitingForPVCResize, "", "Waiting for PVC resize checkpoints")
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.RequeueSoon(3)
}

func (oc *OperatorContext) processResizeWaiting(status *marklogicv1.VolumeResizeStatus) result.ReconcileResult {
	ready, requeueSecs := isRetryWindowOpen(status)
	if !ready {
		return result.RequeueSoon(requeueSecs)
	}

	targetSize, err := resource.ParseQuantity(status.TargetSize)
	if err != nil {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonInvalidResizeRequest, fmt.Sprintf("Invalid target size %q", status.TargetSize))
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	for i := range status.PVCStatuses {
		entry := &status.PVCStatuses[i]
		if isPVCCheckpointed(entry) {
			continue
		}

		pvc := &corev1.PersistentVolumeClaim{}
		if getErr := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: entry.Name}, pvc); getErr != nil {
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonResizeFailed, getErr.Error())
			continue
		}

		requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		observed := pvc.Status.Capacity[corev1.ResourceStorage]
		if observed.IsZero() {
			observed = requested
		}

		entry.RequestedSize = requested.String()
		entry.ObservedCapacity = observed.String()

		if requested.Cmp(targetSize) >= 0 && observed.Cmp(targetSize) >= 0 {
			if hasFileSystemResizePending(pvc) {
				entry.State = marklogicv1.PVCResizeStateCheckpointed
				entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOfflinePending
				entry.RestartRequired = true
				entry.LastReason = ""
				entry.LastMessage = "Offline checkpoint reached"
			} else {
				entry.State = marklogicv1.PVCResizeStateCheckpointed
				entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOnlineComplete
				entry.RestartRequired = false
				entry.LastReason = ""
				entry.LastMessage = "Online checkpoint reached"
			}
			continue
		}

		entry.State = marklogicv1.PVCResizeStateWaitingForCheckpoint
		entry.LastReason = ""
		entry.LastMessage = "Waiting for resize checkpoint"
	}

	oc.updateSequentialActivePVC(status)
	oc.recalculatePVCProgress(status)

	if len(status.FailedPVCs) > 0 {
		if oc.scheduleRetryOrFail(status, marklogicv1.VolumeResizeReasonPartialResizeFailure, "Failed while waiting for PVC resize checkpoint") {
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", status.Message)
			if patchErr := oc.patchResizeStatus(status); patchErr != nil {
				return result.Error(patchErr)
			}
			return result.RequeueSoon(resizeRetryDelaySeconds)
		}
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	oc.clearRetryState(status)
	if status.PVCsCheckpointed == status.TotalPVCs && status.TotalPVCs > 0 {
		status.ActivePVC = ""
		status.Message = "All PVCs reached resize checkpoint"
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	status.Message = "Waiting for PVC resize checkpoints"
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.RequeueSoon(5)
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

func (oc *OperatorContext) initializePVCStatuses(status *marklogicv1.VolumeResizeStatus, pvcState *resizePVCDiscovery) {
	if status.PVCStatuses == nil || len(status.PVCStatuses) == 0 {
		status.PVCStatuses = make([]marklogicv1.PVCResizeStatus, 0, len(pvcState.expectedNames))
		for _, name := range pvcState.expectedNames {
			status.PVCStatuses = append(status.PVCStatuses, marklogicv1.PVCResizeStatus{Name: name, State: marklogicv1.PVCResizeStatePending})
		}
	}
	status.TotalPVCs = int32(len(status.PVCStatuses))
	oc.updateSequentialActivePVC(status)
	oc.recalculatePVCProgress(status)
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

func isPVCCheckpointed(status *marklogicv1.PVCResizeStatus) bool {
	if status == nil {
		return false
	}
	return status.State == marklogicv1.PVCResizeStateCheckpointed || status.State == marklogicv1.PVCResizeStateRestarted
}

func hasFileSystemResizePending(pvc *corev1.PersistentVolumeClaim) bool {
	for _, cond := range pvc.Status.Conditions {
		if cond.Type == corev1.PersistentVolumeClaimFileSystemResizePending && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isRetryWindowOpen(status *marklogicv1.VolumeResizeStatus) (bool, int) {
	if status == nil || status.NextRetryTime == nil {
		return true, 0
	}
	d := time.Until(status.NextRetryTime.Time)
	if d <= 0 {
		return true, 0
	}
	secs := int(d.Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	return false, secs
}

func (oc *OperatorContext) getSubmissionIndices(status *marklogicv1.VolumeResizeStatus) []int {
	indices := []int{}
	if status.ResizeStrategy == marklogicv1.VolumeResizeStrategySequential {
		oc.updateSequentialActivePVC(status)
		for idx := range status.PVCStatuses {
			if status.PVCStatuses[idx].Name == status.ActivePVC && !isPVCCheckpointed(&status.PVCStatuses[idx]) {
				return []int{idx}
			}
		}
		return indices
	}

	for idx := range status.PVCStatuses {
		if !isPVCCheckpointed(&status.PVCStatuses[idx]) {
			indices = append(indices, idx)
		}
	}
	return indices
}

func (oc *OperatorContext) updateSequentialActivePVC(status *marklogicv1.VolumeResizeStatus) {
	if status == nil || status.ResizeStrategy != marklogicv1.VolumeResizeStrategySequential {
		return
	}
	for idx := range status.PVCStatuses {
		if status.PVCStatuses[idx].Name == status.ActivePVC && !isPVCCheckpointed(&status.PVCStatuses[idx]) {
			return
		}
	}
	for idx := range status.PVCStatuses {
		if !isPVCCheckpointed(&status.PVCStatuses[idx]) {
			status.ActivePVC = status.PVCStatuses[idx].Name
			return
		}
	}
	status.ActivePVC = ""
}

func (oc *OperatorContext) markPVCFailed(status *marklogicv1.VolumeResizeStatus, pvcName string, reason marklogicv1.VolumeResizeReason, message string) {
	for idx := range status.PVCStatuses {
		if status.PVCStatuses[idx].Name != pvcName {
			continue
		}
		status.PVCStatuses[idx].State = marklogicv1.PVCResizeStateFailed
		status.PVCStatuses[idx].LastReason = string(reason)
		status.PVCStatuses[idx].LastMessage = message
		status.PVCStatuses[idx].CheckpointType = ""
		status.PVCStatuses[idx].RestartRequired = false
		return
	}
	status.PVCStatuses = append(status.PVCStatuses, marklogicv1.PVCResizeStatus{
		Name:        pvcName,
		State:       marklogicv1.PVCResizeStateFailed,
		LastReason:  string(reason),
		LastMessage: message,
	})
	status.TotalPVCs = int32(len(status.PVCStatuses))
}

func (oc *OperatorContext) recalculatePVCProgress(status *marklogicv1.VolumeResizeStatus) {
	checkpointed := int32(0)
	failed := make([]marklogicv1.FailedPVCStatus, 0)
	for _, pvcStatus := range status.PVCStatuses {
		if isPVCCheckpointed(&pvcStatus) {
			checkpointed++
		}
		if pvcStatus.State == marklogicv1.PVCResizeStateFailed {
			failed = append(failed, marklogicv1.FailedPVCStatus{
				Name:    pvcStatus.Name,
				Reason:  pvcStatus.LastReason,
				Message: pvcStatus.LastMessage,
			})
		}
	}
	status.PVCsCheckpointed = checkpointed
	status.TotalPVCs = int32(len(status.PVCStatuses))
	status.FailedPVCs = failed
}

func (oc *OperatorContext) scheduleRetryOrFail(status *marklogicv1.VolumeResizeStatus, reason marklogicv1.VolumeResizeReason, message string) bool {
	status.RetryCount++
	if status.RetryCount > resizeMaxRetries {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonMaxRetriesExceeded, fmt.Sprintf("%s: max retries exceeded (%d)", message, resizeMaxRetries))
		status.NextRetryTime = nil
		return false
	}

	nextRetry := metav1.NewTime(time.Now().Add(time.Duration(resizeRetryDelaySeconds) * time.Second))
	status.NextRetryTime = &nextRetry
	oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseStalled, reason, fmt.Sprintf("%s: retry %d/%d at %s", message, status.RetryCount, resizeMaxRetries, nextRetry.Format(time.RFC3339)))
	return true
}

func (oc *OperatorContext) clearRetryState(status *marklogicv1.VolumeResizeStatus) {
	status.RetryCount = 0
	status.NextRetryTime = nil
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
