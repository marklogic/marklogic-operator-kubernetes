// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strconv"
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
	resizePausedMessage      = "Resize is paused by annotation marklogic.progress.com/resize-paused"
	dataDirPVCName           = "datadir"
	resizeRetryDelaySeconds  = 10
	resizeRetryMaxDelaySecs  = 300
	resizeRetryJitterPercent = 20
	resizeMaxRetries         = 15

	resizeMarkerSyncStarted             = "pr4.sync.started"
	resizeMarkerTemplateSynced          = "pr4.sync.template-synced"
	resizeMarkerRestartPlanPrepared     = "pr4.sync.restart-plan-prepared"
	resizeMarkerVerifyStarted           = "pr5.verify.started"
	resizeMarkerVerifyCompleted         = "pr5.verify.completed"
	resizeMarkerTemplateRecreateStarted = "pr5.1.sync.template-recreate-started"
	resizeMarkerTemplateDeleted         = "pr5.1.sync.template-deleted"
	resizeMarkerTemplateRecreated       = "pr5.1.sync.template-recreated"
)

type resizePVCDiscovery struct {
	expectedNames []string
	foundPVCs     map[string]*corev1.PersistentVolumeClaim
	missingPVCs   []string
	notBoundPVCs  []string
	minSize       *resource.Quantity
	minByTemplate map[string]*resource.Quantity
	targetByPVC   map[string]resource.Quantity
}

type templateResizeTargets map[string]resource.Quantity

func (oc *OperatorContext) ReconcileVolumeResizeValidation() result.ReconcileResult {
	cr := oc.MarklogicGroup
	if cr == nil {
		return result.Continue()
	}

	targets, err := resolveResizeTargetsFromSpec(cr)
	if err != nil {
		return oc.failResizeValidation(marklogicv1.VolumeResizeReasonInvalidResizeRequest, err.Error())
	}
	if len(targets) == 0 {
		return result.Continue()
	}
	primaryTarget := primaryResizeTarget(targets)

	currentSts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet and PVCs are not ready yet, keep existing reconcile flow.
			return result.Continue()
		}
		return result.Error(err)
	}

	pvcState, err := oc.discoverPrimaryPVCs(currentSts, targets)
	if err != nil {
		return result.Error(err)
	}

	active := cr.Status.VolumeResizeStatus
	if isResizeOperationActive(active) {
		return oc.reconcileActiveResizeOperation(active, primaryTarget, currentSts)
	}

	if pvcState.minSize == nil {
		// No live baseline yet, continue with the existing reconcile flow.
		return result.Continue()
	}

	comparison, cmpMessage := compareTargetsWithCurrent(pvcState, targets)
	if comparison == 0 {
		return result.Continue()
	}

	if shouldIgnoreTerminalResizeRestart(cr.Status.VolumeResizeStatus, primaryTarget.String(), cr.Generation) {
		return result.Continue()
	}

	resizeStatus := oc.newResizeStatus(pvcState, primaryTarget.String())
	claimed, err := oc.claimResizeStatusCAS(resizeStatus)
	if err != nil {
		return result.Error(err)
	}
	if !claimed {
		if isResizeOperationActive(oc.MarklogicGroup.Status.VolumeResizeStatus) {
			return oc.reconcileActiveResizeOperation(oc.MarklogicGroup.Status.VolumeResizeStatus, primaryTarget, currentSts)
		}
		return result.Continue()
	}

	if comparison < 0 {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonShrinkNotSupported, cmpMessage)
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}

	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeRequested", fmt.Sprintf("Resize requested from %s to %s", resizeStatus.CurrentSize, resizeStatus.TargetSize))

	if isResizePaused(cr) {
		if setResizePausedStatus(resizeStatus, resizePausedMessage) {
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", resizeStatus.Message)
		}
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
		return result.RequeueSoon(resizeRetryDelaySeconds)
	}

	if len(pvcState.notBoundPVCs) > 0 {
		oc.transitionResizePhase(resizeStatus, marklogicv1.VolumeResizePhaseStalled, marklogicv1.VolumeResizeReasonPVCNotBound, fmt.Sprintf("PVCs are not Bound: %s", strings.Join(pvcState.notBoundPVCs, ",")))
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", resizeStatus.Message)
		if err := oc.patchResizeStatus(resizeStatus); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(resizeRetryDelaySeconds)
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
		targetSize := ""
		if oc.MarklogicGroup.Spec.Persistence != nil {
			targetSize = oc.MarklogicGroup.Spec.Persistence.Size
		}
		now := metav1.Now()
		resizeStatus = &marklogicv1.VolumeResizeStatus{
			OperationID:        "resize-" + generateRandomAlphaNumeric(10),
			ObservedGeneration: oc.MarklogicGroup.Generation,
			FirstStartedTime:   &now,
			LastTransitionTime: &now,
			CurrentSize:        "",
			TargetSize:         targetSize,
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
		if setResizePausedStatus(active, resizePausedMessage) {
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", resizePausedMessage)
			updated = true
		}
		if updated {
			if err := oc.patchResizeStatus(active); err != nil {
				return result.Error(err)
			}
		}
		return result.Done()
	}

	if clearResizePausedStatus(active, resizePausedMessage) {
		updated = true
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
	case marklogicv1.VolumeResizePhaseSynchronizingStatefulSet:
		return oc.processStatefulSetSynchronization(active, currentSts)
	case marklogicv1.VolumeResizePhaseRestartingPods:
		return oc.processPodRestarts(active, currentSts)
	case marklogicv1.VolumeResizePhaseWaitingForPodsReady:
		return oc.processPodsReadyWait(active, currentSts)
	case marklogicv1.VolumeResizePhaseVerifyingResizeOutcome:
		return oc.processResizeVerification(active, currentSts)
	case marklogicv1.VolumeResizePhaseStalled:
		ready, requeueSecs := isRetryWindowOpen(active)
		if !ready {
			return result.RequeueSoon(requeueSecs)
		}
		active.NextRetryTime = nil
		retryPhase := marklogicv1.VolumeResizePhaseResizingPVCs
		retryMessage := "Retrying PVC resize operation"
		if active.Reason == marklogicv1.VolumeResizeReasonStatefulSetSyncFailed || active.Reason == marklogicv1.VolumeResizeReasonTemplateUpdateInterrupted {
			retryPhase = marklogicv1.VolumeResizePhaseSynchronizingStatefulSet
			retryMessage = "Retrying StatefulSet synchronization"
		}
		if active.Reason == marklogicv1.VolumeResizeReasonPodRecoveryFailed {
			retryPhase = marklogicv1.VolumeResizePhaseWaitingForPodsReady
			retryMessage = "Retrying pod recovery and readiness checks"
		}
		if active.Reason == marklogicv1.VolumeResizeReasonMarkLogicHealthCheckFailed {
			retryPhase = marklogicv1.VolumeResizePhaseVerifyingResizeOutcome
			retryMessage = "Retrying resize outcome verification"
		}
		oc.transitionResizePhase(active, retryPhase, "", retryMessage)
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

	if len(status.PVCStatuses) == 0 {
		currentSts, stsErr := oc.GetStatefulSet(oc.MarklogicGroup.Namespace, oc.MarklogicGroup.Spec.Name)
		if stsErr != nil {
			return result.Error(stsErr)
		}
		templateTargets, targetErr := desiredTemplateTargetsFromStatus(status, currentSts.Name)
		if targetErr != nil {
			return result.Error(targetErr)
		}
		pvcState, discoverErr := oc.discoverPrimaryPVCs(currentSts, templateTargets)
		if discoverErr != nil {
			return result.Error(discoverErr)
		}
		oc.initializePVCStatuses(status, pvcState)
	}

	indices := oc.getSubmissionIndices(status)
	for _, idx := range indices {
		entry := &status.PVCStatuses[idx]
		if isPVCCheckpointed(entry) {
			oc.ReqLogger.V(1).Info("processResizeSubmission: PVC already checkpointed, skipping", "name", entry.Name)
			continue
		}

		pvc := &corev1.PersistentVolumeClaim{}
		if getErr := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: entry.Name}, pvc); getErr != nil {
			oc.ReqLogger.V(1).Info("processResizeSubmission: failed to fetch PVC", "name", entry.Name, "error", getErr.Error())
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonResizeFailed, getErr.Error())
			continue
		}

		requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		observed := pvc.Status.Capacity[corev1.ResourceStorage]
		if observed.IsZero() {
			observed = requested
		}
		entryTarget, targetErr := resizeTargetForPVCEntry(status, entry)
		if targetErr != nil {
			oc.ReqLogger.Info("DEBUG: processResizeSubmission - Failed to resolve target", "name", entry.Name, "error", targetErr.Error())
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonInvalidResizeRequest, targetErr.Error())
			continue
		}
		entry.RequestedSize = entryTarget.String()
		entry.ObservedCapacity = observed.String()

		oc.ReqLogger.Info("DEBUG: processResizeSubmission - PVC size check", "name", entry.Name, "requested", requested.String(), "target", entryTarget.String(), "observed", observed.String())

		if requested.Cmp(entryTarget) >= 0 {
			entry.State = marklogicv1.PVCResizeStateWaitingForCheckpoint
			entry.LastReason = ""
			entry.LastMessage = "Waiting for resize checkpoint"
			oc.ReqLogger.Info("DEBUG: processResizeSubmission - Request already at target, waiting for checkpoint", "name", entry.Name)
			continue
		}

		oc.ReqLogger.Info("DEBUG: processResizeSubmission - Submitting PVC resize patch", "name", entry.Name, "newSize", entryTarget.String())
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = entryTarget
		if patchErr := oc.Client.Patch(oc.Ctx, pvc, patch); patchErr != nil {
			oc.ReqLogger.Info("DEBUG: processResizeSubmission - Patch failed", "name", entry.Name, "error", patchErr.Error())
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonResizeFailed, patchErr.Error())
			continue
		}

		entry.RequestedSize = entryTarget.String()
		entry.State = marklogicv1.PVCResizeStateResizeSubmitted
		entry.LastReason = ""
		entry.LastMessage = "Resize request submitted"
		oc.ReqLogger.Info("DEBUG: processResizeSubmission - Resize patch submitted successfully", "name", entry.Name)
	}

	oc.updateSequentialActivePVC(status)
	oc.recalculatePVCProgress(status)

	if len(status.FailedPVCs) > 0 {
		if retryScheduled, retryDelaySecs := oc.scheduleRetryOrFail(status, marklogicv1.VolumeResizeReasonPartialResizeFailure, "Failed to submit one or more PVC resize patches"); retryScheduled {
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", status.Message)
			if patchErr := oc.patchResizeStatus(status); patchErr != nil {
				return result.Error(patchErr)
			}
			return result.RequeueSoon(retryDelaySecs)
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

		entryTarget, targetErr := resizeTargetForPVCEntry(status, entry)
		if targetErr != nil {
			oc.markPVCFailed(status, entry.Name, marklogicv1.VolumeResizeReasonInvalidResizeRequest, targetErr.Error())
			continue
		}
		entry.RequestedSize = entryTarget.String()
		entry.ObservedCapacity = observed.String()

		if requested.Cmp(entryTarget) >= 0 {
			if hasFileSystemResizePending(pvc) {
				entry.State = marklogicv1.PVCResizeStateCheckpointed
				entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOfflinePending
				entry.RestartRequired = true
				entry.LastReason = ""
				entry.LastMessage = "Offline checkpoint reached"
				continue
			}
			if observed.Cmp(entryTarget) >= 0 {
				entry.State = marklogicv1.PVCResizeStateCheckpointed
				entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOnlineComplete
				entry.RestartRequired = false
				entry.LastReason = ""
				entry.LastMessage = "Online checkpoint reached"
				continue
			}
		}

		entry.State = marklogicv1.PVCResizeStateWaitingForCheckpoint
		entry.LastReason = ""
		entry.LastMessage = "Waiting for resize checkpoint"
	}

	oc.updateSequentialActivePVC(status)
	oc.recalculatePVCProgress(status)

	if len(status.FailedPVCs) > 0 {
		if retryScheduled, retryDelaySecs := oc.scheduleRetryOrFail(status, marklogicv1.VolumeResizeReasonPartialResizeFailure, "Failed while waiting for PVC resize checkpoint"); retryScheduled {
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", status.Message)
			if patchErr := oc.patchResizeStatus(status); patchErr != nil {
				return result.Error(patchErr)
			}
			return result.RequeueSoon(retryDelaySecs)
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
		addResizeMarker(status, resizeMarkerSyncStarted)
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseSynchronizingStatefulSet, "", "All PVC checkpoints reached; synchronizing StatefulSet template")
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(1)
	}

	if status.ResizeStrategy == marklogicv1.VolumeResizeStrategySequential && status.PVCsCheckpointed < status.TotalPVCs {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseResizingPVCs, "", "Sequential strategy advancing to next PVC resize request")
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(1)
	}

	status.Message = "Waiting for PVC resize checkpoints"
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.RequeueSoon(5)
}

func (oc *OperatorContext) processStatefulSetSynchronization(status *marklogicv1.VolumeResizeStatus, currentSts *appsv1.StatefulSet) result.ReconcileResult {
	templateTargets, err := desiredTemplateTargetsFromStatus(status, currentSts.Name)
	if err != nil {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonInvalidResizeRequest, err.Error())
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	addResizeMarker(status, resizeMarkerSyncStarted)
	if !hasResizeMarker(status, resizeMarkerTemplateSynced) {
		synced, syncErr := oc.syncStatefulSetPVCTemplates(status, currentSts, templateTargets)
		if syncErr != nil {
			return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonStatefulSetSyncFailed, "Failed to synchronize StatefulSet template", syncErr)
		}
		if synced {
			oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", "StatefulSet template synchronization is progressing")
		}
		if !hasResizeMarker(status, resizeMarkerTemplateSynced) {
			status.Message = "Synchronizing StatefulSet template with immutable-safe recreate flow"
			if patchErr := oc.patchResizeStatus(status); patchErr != nil {
				return result.Error(patchErr)
			}
			return result.RequeueSoon(1)
		}
		status.Message = "StatefulSet template synchronized; preparing restart plan"
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(1)
	}

	offlineCandidates := getOfflineRestartCandidates(status, currentSts.Name)
	for idx := range status.PVCStatuses {
		entry := &status.PVCStatuses[idx]
		if containsName(offlineCandidates, entry.Name) {
			if entry.State != marklogicv1.PVCResizeStateRestarted {
				entry.State = marklogicv1.PVCResizeStateRestartPending
			}
			if entry.PodName == "" {
				entry.PodName = derivePodNameFromPVC(currentSts.Name, entry.Name)
			}
			entry.LastReason = ""
			entry.LastMessage = "Pending controlled pod restart"
			continue
		}
		if entry.State == marklogicv1.PVCResizeStateRestartPending {
			entry.State = marklogicv1.PVCResizeStateCheckpointed
			entry.LastReason = ""
			entry.LastMessage = "No restart required"
		}
	}
	addResizeMarker(status, resizeMarkerRestartPlanPrepared)
	if len(offlineCandidates) > 0 {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseRestartingPods, "", "Restart plan prepared; restarting offline-pending pods")
	} else {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseWaitingForPodsReady, "", "No offline restarts required; waiting for pod readiness")
	}
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.RequeueSoon(1)
}

func (oc *OperatorContext) processPodRestarts(status *marklogicv1.VolumeResizeStatus, currentSts *appsv1.StatefulSet) result.ReconcileResult {
	next := oc.nextRestartCandidate(status, currentSts.Name)
	if next == nil {
		status.ActivePVC = ""
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseWaitingForPodsReady, "", "Waiting for restarted pods to become ready")
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(2)
	}

	if next.PodName == "" {
		next.PodName = derivePodNameFromPVC(currentSts.Name, next.Name)
	}
	if next.PodName == "" {
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonPodRecoveryFailed, "Failed to derive pod name for restart", fmt.Errorf("pvc %s has no resolvable pod", next.Name))
	}

	pod := &corev1.Pod{}
	getErr := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: next.PodName}, pod)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonPodRecoveryFailed, "Failed to fetch pod for restart", getErr)
	}
	if getErr == nil {
		if deleteErr := oc.Client.Delete(oc.Ctx, pod); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonPodRecoveryFailed, "Failed to delete pod for restart", deleteErr)
		}
	}

	status.ActivePVC = next.Name
	next.State = marklogicv1.PVCResizeStateRestartPending
	next.LastReason = ""
	next.LastMessage = fmt.Sprintf("Restart initiated for pod %s", next.PodName)
	oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseWaitingForPodsReady, "", fmt.Sprintf("Waiting for pod %s to become ready", next.PodName))
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.RequeueSoon(2)
}

func (oc *OperatorContext) processPodsReadyWait(status *marklogicv1.VolumeResizeStatus, currentSts *appsv1.StatefulSet) result.ReconcileResult {
	if status.ActivePVC != "" {
		activeEntry := oc.findPVCStatusByName(status, status.ActivePVC)
		if activeEntry != nil {
			if activeEntry.PodName == "" {
				activeEntry.PodName = derivePodNameFromPVC(currentSts.Name, activeEntry.Name)
			}
			if activeEntry.PodName != "" {
				ready, readyErr := oc.isPodReady(activeEntry.PodName)
				if readyErr != nil {
					return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonPodRecoveryFailed, "Failed while waiting for pod readiness", readyErr)
				}
				if !ready {
					status.Message = fmt.Sprintf("Waiting for pod %s to become ready", activeEntry.PodName)
					if patchErr := oc.patchResizeStatus(status); patchErr != nil {
						return result.Error(patchErr)
					}
					return result.RequeueSoon(3)
				}

				for i := range status.PVCStatuses {
					entry := &status.PVCStatuses[i]
					if entry.PodName != activeEntry.PodName {
						continue
					}
					if entry.State != marklogicv1.PVCResizeStateRestartPending {
						continue
					}
					entry.State = marklogicv1.PVCResizeStateRestarted
					entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOfflineComplete
					entry.RestartRequired = false
					entry.LastReason = ""
					entry.LastMessage = "Pod restart completed"
				}
				status.ActivePVC = ""
			}
		}
	}

	next := oc.nextRestartCandidate(status, currentSts.Name)
	if next != nil {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseRestartingPods, "", "Continuing controlled pod restarts")
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(1)
	}

	allReady, readyErr := oc.areStatefulSetPodsReady(currentSts)
	if readyErr != nil {
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonPodRecoveryFailed, "Failed to verify StatefulSet pod readiness", readyErr)
	}
	if !allReady {
		status.Message = "Waiting for StatefulSet pods to be ready"
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(5)
	}

	oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseVerifyingResizeOutcome, "", "StatefulSet synchronized and pods ready; awaiting verification")
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.RequeueSoon(1)
}

func (oc *OperatorContext) processResizeVerification(status *marklogicv1.VolumeResizeStatus, currentSts *appsv1.StatefulSet) result.ReconcileResult {
	ready, requeueSecs := isRetryWindowOpen(status)
	if !ready {
		return result.RequeueSoon(requeueSecs)
	}

	templateTargets, err := desiredTemplateTargetsFromStatus(status, currentSts.Name)
	if err != nil {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonInvalidResizeRequest, err.Error())
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.Done()
	}

	if !hasResizeMarker(status, resizeMarkerVerifyStarted) {
		addResizeMarker(status, resizeMarkerVerifyStarted)
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", "Starting resize outcome verification")
	}

	templatesBelowTarget := make([]string, 0)
	for templateName, templateTarget := range templateTargets {
		templateRequest, hasTemplate := getStatefulSetTemplateRequest(currentSts, templateName)
		if !hasTemplate {
			oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonTemplateUpdateInterrupted, fmt.Sprintf("StatefulSet template %s is missing during verification", templateName))
			oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
			if patchErr := oc.patchResizeStatus(status); patchErr != nil {
				return result.Error(patchErr)
			}
			return result.Done()
		}
		if templateRequest.Cmp(templateTarget) < 0 {
			templatesBelowTarget = append(templatesBelowTarget, fmt.Sprintf("%s=%s", templateName, templateRequest.String()))
		}
	}
	if len(templatesBelowTarget) > 0 {
		sort.Strings(templatesBelowTarget)
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonStatefulSetSyncFailed, fmt.Sprintf("StatefulSet template requests are below target: %s", strings.Join(templatesBelowTarget, ",")), fmt.Errorf("template request below target"))
	}

	notFinalPVCs := make([]string, 0)
	needsRestartAgain := make([]string, 0)
	for i := range status.PVCStatuses {
		entry := &status.PVCStatuses[i]
		pvc := &corev1.PersistentVolumeClaim{}
		if getErr := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: entry.Name}, pvc); getErr != nil {
			return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonMarkLogicHealthCheckFailed, "Failed to fetch PVC during verification", getErr)
		}
		oc.ReqLogger.Info("DEBUG: processResizeVerification - PVC state", "name", entry.Name, "state", entry.State, "checkpointType", entry.CheckpointType, "restartRequired", entry.RestartRequired, "fileSystemResizePending", hasFileSystemResizePending(pvc))

		requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		observed := pvc.Status.Capacity[corev1.ResourceStorage]
		if observed.IsZero() {
			observed = requested
		}

		entryTarget, targetErr := resizeTargetForPVCEntry(status, entry)
		if targetErr != nil {
			return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonInvalidResizeRequest, "Invalid PVC target during verification", targetErr)
		}
		entry.RequestedSize = entryTarget.String()
		entry.ObservedCapacity = observed.String()

		if requested.Cmp(entryTarget) < 0 {
			notFinalPVCs = append(notFinalPVCs, entry.Name)
			continue
		}

		if hasFileSystemResizePending(pvc) {
			notFinalPVCs = append(notFinalPVCs, entry.Name)
			entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOfflinePending
			entry.RestartRequired = true
			entry.LastReason = ""
			entry.LastMessage = "Filesystem resize still pending"
			// If this PVC already has a checkpoint (offline or online), it means pod was restarted
			// but filesystem resize is still pending - needs another restart
			if isPVCCheckpointed(entry) {
				needsRestartAgain = append(needsRestartAgain, entry.Name)
				entry.State = marklogicv1.PVCResizeStateRestartPending
				if entry.PodName == "" {
					entry.PodName = derivePodNameFromPVC(currentSts.Name, entry.Name)
				}
				entry.LastMessage = "Filesystem resize still pending after restart; scheduling another restart"
			}
			continue
		}

		if observed.Cmp(entryTarget) < 0 {
			notFinalPVCs = append(notFinalPVCs, entry.Name)
			continue
		}

		if entry.State == marklogicv1.PVCResizeStateRestartPending || entry.RestartRequired {
			notFinalPVCs = append(notFinalPVCs, entry.Name)
			entry.LastReason = ""
			entry.LastMessage = "Awaiting restart completion"
			continue
		}

		if entry.State == marklogicv1.PVCResizeStateRestarted || entry.CheckpointType == marklogicv1.PVCResizeCheckpointTypeOfflinePending {
			entry.State = marklogicv1.PVCResizeStateRestarted
			entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOfflineComplete
		} else {
			entry.State = marklogicv1.PVCResizeStateCheckpointed
			entry.CheckpointType = marklogicv1.PVCResizeCheckpointTypeOnlineComplete
		}
		entry.RestartRequired = false
		entry.LastReason = ""
		entry.LastMessage = "Verification checks passed"
	}

	oc.recalculatePVCProgress(status)
	if len(needsRestartAgain) > 0 {
		sort.Strings(needsRestartAgain)
		oc.ReqLogger.Info("DEBUG: processResizeVerification - PVCs still pending after restart, triggering another restart", "pvcs", needsRestartAgain)
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseRestartingPods, "", fmt.Sprintf("Filesystem resize still pending after restart for PVCs: %s; scheduling another restart", strings.Join(needsRestartAgain, ",")))
		oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(5)
	}
	if len(notFinalPVCs) > 0 {
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonMarkLogicHealthCheckFailed, fmt.Sprintf("Verification pending for PVCs: %s", strings.Join(notFinalPVCs, ",")), fmt.Errorf("final pvc state not satisfied"))
	}

	allReady, readyErr := oc.areStatefulSetPodsReady(currentSts)
	if readyErr != nil {
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonMarkLogicHealthCheckFailed, "Failed to verify pod readiness during verification", readyErr)
	}
	if !allReady {
		return oc.scheduleSyncRetryOrFail(status, marklogicv1.VolumeResizeReasonMarkLogicHealthCheckFailed, "StatefulSet pods are not yet ready during verification", fmt.Errorf("pods not ready"))
	}

	oc.clearRetryState(status)
	status.ActivePVC = ""
	addResizeMarker(status, resizeMarkerVerifyCompleted)
	oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseCompleted, "", "Resize operation completed successfully")
	oc.emitResizeEvent(corev1.EventTypeNormal, "VolumeResizeProgressing", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
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

func (oc *OperatorContext) initializePVCStatuses(status *marklogicv1.VolumeResizeStatus, pvcState *resizePVCDiscovery) {
	if len(status.PVCStatuses) == 0 {
		oc.ReqLogger.Info("DEBUG: initializePVCStatuses - initializing from discovery", "expectedNames", pvcState.expectedNames, "count", len(pvcState.expectedNames))
		status.PVCStatuses = make([]marklogicv1.PVCResizeStatus, 0, len(pvcState.expectedNames))
		for _, name := range pvcState.expectedNames {
			entry := marklogicv1.PVCResizeStatus{Name: name, State: marklogicv1.PVCResizeStatePending}
			if target, ok := pvcState.targetByPVC[name]; ok {
				entry.RequestedSize = target.String()
				oc.ReqLogger.Info("DEBUG: initializePVCStatuses - initialized PVC with target", "name", name, "target", target.String())
			} else {
				oc.ReqLogger.Info("DEBUG: initializePVCStatuses - initialized PVC without target", "name", name)
			}
			status.PVCStatuses = append(status.PVCStatuses, entry)
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
	latest := &marklogicv1.MarklogicGroup{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: oc.MarklogicGroup.Name, Namespace: oc.MarklogicGroup.Namespace}, latest); err != nil {
		return false, err
	}
	expectedResourceVersion := latest.ResourceVersion

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

func setResizePausedStatus(status *marklogicv1.VolumeResizeStatus, message string) bool {
	if status == nil {
		return false
	}

	changed := false
	if status.Reason != marklogicv1.VolumeResizeReasonPaused {
		status.Reason = marklogicv1.VolumeResizeReasonPaused
		changed = true
	}
	if status.Message != message {
		status.Message = message
		changed = true
	}
	if status.LastTransitionTime == nil {
		now := metav1.Now()
		status.LastTransitionTime = &now
		changed = true
	}

	return changed
}

func clearResizePausedStatus(status *marklogicv1.VolumeResizeStatus, pausedMessage string) bool {
	if status == nil || status.Reason != marklogicv1.VolumeResizeReasonPaused {
		return false
	}

	status.Reason = ""
	if status.Message == pausedMessage {
		status.Message = ""
	}
	return true
}

func shouldIgnoreTerminalResizeRestart(status *marklogicv1.VolumeResizeStatus, specTargetSize string, generation int64) bool {
	if status == nil || isResizeOperationActive(status) {
		return false
	}

	if status.ObservedGeneration != generation {
		return false
	}

	if !resizeTargetsEqual(status.TargetSize, specTargetSize) {
		return false
	}

	if status.DeferredObservedGeneration == generation && resizeTargetsEqual(status.DeferredTargetSize, specTargetSize) {
		return false
	}

	return true
}

func resizeTargetsEqual(left, right string) bool {
	leftQty, leftErr := resource.ParseQuantity(left)
	rightQty, rightErr := resource.ParseQuantity(right)
	if leftErr == nil && rightErr == nil {
		return leftQty.Cmp(rightQty) == 0
	}
	return left == right
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

func (oc *OperatorContext) scheduleRetryOrFail(status *marklogicv1.VolumeResizeStatus, reason marklogicv1.VolumeResizeReason, message string) (bool, int) {
	status.RetryCount++
	if status.RetryCount > resizeMaxRetries {
		oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseFailed, marklogicv1.VolumeResizeReasonMaxRetriesExceeded, fmt.Sprintf("%s: max retries exceeded (%d)", message, resizeMaxRetries))
		status.NextRetryTime = nil
		return false, 0
	}

	retryDelaySecs := computeResizeRetryDelaySeconds(status.RetryCount)
	nextRetry := metav1.NewTime(time.Now().Add(time.Duration(retryDelaySecs) * time.Second))
	status.NextRetryTime = &nextRetry
	oc.transitionResizePhase(status, marklogicv1.VolumeResizePhaseStalled, reason, fmt.Sprintf("%s: retry %d/%d at %s", message, status.RetryCount, resizeMaxRetries, nextRetry.Format(time.RFC3339)))
	return true, retryDelaySecs
}

func computeResizeRetryDelaySeconds(retryCount int32) int {
	baseDelaySecs := computeResizeRetryBaseDelaySeconds(retryCount)
	return jitteredResizeRetryDelaySeconds(baseDelaySecs, resizeRetryJitterIntn)
}

func resizeRetryJitterIntn(n int) int {
	if n <= 0 {
		return 0
	}
	randomValue, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(randomValue.Int64())
}

func computeResizeRetryBaseDelaySeconds(retryCount int32) int {
	if retryCount <= 1 {
		return resizeRetryDelaySeconds
	}

	delaySecs := resizeRetryDelaySeconds
	for attempt := int32(1); attempt < retryCount; attempt++ {
		delaySecs *= 2
		if delaySecs >= resizeRetryMaxDelaySecs {
			return resizeRetryMaxDelaySecs
		}
	}

	return delaySecs
}

func jitteredResizeRetryDelaySeconds(baseDelaySecs int, intnFn func(int) int) int {
	if baseDelaySecs <= 0 {
		return resizeRetryDelaySeconds
	}

	// Jitter avoids synchronized retries when multiple controllers fail at the same time.
	jitterRange := (baseDelaySecs * resizeRetryJitterPercent) / 100
	if jitterRange < 1 || intnFn == nil {
		if baseDelaySecs > resizeRetryMaxDelaySecs {
			return resizeRetryMaxDelaySecs
		}
		return baseDelaySecs
	}

	minDelay := baseDelaySecs - jitterRange
	if minDelay < 1 {
		minDelay = 1
	}

	maxDelay := baseDelaySecs + jitterRange
	if maxDelay > resizeRetryMaxDelaySecs {
		maxDelay = resizeRetryMaxDelaySecs
	}

	rangeSize := (maxDelay - minDelay) + 1
	if rangeSize <= 1 {
		return minDelay
	}

	return minDelay + intnFn(rangeSize)
}

func (oc *OperatorContext) clearRetryState(status *marklogicv1.VolumeResizeStatus) {
	status.RetryCount = 0
	status.NextRetryTime = nil
}

func (oc *OperatorContext) scheduleSyncRetryOrFail(status *marklogicv1.VolumeResizeStatus, reason marklogicv1.VolumeResizeReason, message string, err error) result.ReconcileResult {
	if retryScheduled, retryDelaySecs := oc.scheduleRetryOrFail(status, reason, fmt.Sprintf("%s: %v", message, err)); retryScheduled {
		oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeStalled", status.Message)
		if patchErr := oc.patchResizeStatus(status); patchErr != nil {
			return result.Error(patchErr)
		}
		return result.RequeueSoon(retryDelaySecs)
	}
	oc.emitResizeEvent(corev1.EventTypeWarning, "VolumeResizeFailed", status.Message)
	if patchErr := oc.patchResizeStatus(status); patchErr != nil {
		return result.Error(patchErr)
	}
	return result.Done()
}

func (oc *OperatorContext) syncStatefulSetPVCTemplates(status *marklogicv1.VolumeResizeStatus, currentSts *appsv1.StatefulSet, templateTargets templateResizeTargets) (bool, error) {
	if currentSts == nil {
		return false, fmt.Errorf("statefulset is nil")
	}

	templatesMissingFromStatefulSet := make([]string, 0)
	templatesBelowTarget := make([]string, 0)
	for templateName, target := range templateTargets {
		current, hasTemplate := getStatefulSetTemplateRequest(currentSts, templateName)
		if !hasTemplate {
			templatesMissingFromStatefulSet = append(templatesMissingFromStatefulSet, templateName)
			continue
		}
		if current.Cmp(target) < 0 {
			templatesBelowTarget = append(templatesBelowTarget, templateName)
		}
	}

	if len(templatesMissingFromStatefulSet) > 0 {
		sort.Strings(templatesMissingFromStatefulSet)
		oc.ReqLogger.Info("DEBUG: syncStatefulSetPVCTemplates - templates missing from StatefulSet", "statefulSet", currentSts.Name, "missing", templatesMissingFromStatefulSet, "available", getTemplateNamesFromSTS(currentSts))
		return false, fmt.Errorf("statefulset %s is missing volumeClaimTemplates: %s", currentSts.Name, strings.Join(templatesMissingFromStatefulSet, ","))
	}

	if len(templatesBelowTarget) == 0 {
		addResizeMarker(status, resizeMarkerTemplateRecreated)
		addResizeMarker(status, resizeMarkerTemplateSynced)
		return false, nil
	}

	sort.Strings(templatesBelowTarget)
	oc.ReqLogger.Info("DEBUG: syncStatefulSetPVCTemplates - templates below target, recreating StatefulSet", "statefulSet", currentSts.Name, "belowTarget", templatesBelowTarget, "allTemplates", getTemplateNamesFromSTS(currentSts))
	_ = templatesBelowTarget

	addResizeMarker(status, resizeMarkerTemplateRecreateStarted)
	if hasResizeMarker(status, resizeMarkerTemplateDeleted) {
		return true, nil
	}

	deletePolicy := metav1.DeletePropagationOrphan
	err := oc.Client.Delete(oc.Ctx, currentSts, &client.DeleteOptions{PropagationPolicy: &deletePolicy})
	if err != nil {
		if apierrors.IsNotFound(err) {
			addResizeMarker(status, resizeMarkerTemplateDeleted)
			return true, nil
		}
		return false, err
	}
	addResizeMarker(status, resizeMarkerTemplateDeleted)
	return true, nil
}

func getStatefulSetTemplateRequest(currentSts *appsv1.StatefulSet, templateName string) (resource.Quantity, bool) {
	if currentSts == nil {
		return resource.Quantity{}, false
	}
	for i := range currentSts.Spec.VolumeClaimTemplates {
		template := currentSts.Spec.VolumeClaimTemplates[i]
		if template.Name != templateName {
			continue
		}
		if template.Spec.Resources.Requests == nil {
			return resource.Quantity{}, false
		}
		request, ok := template.Spec.Resources.Requests[corev1.ResourceStorage]
		if !ok {
			return resource.Quantity{}, false
		}
		return request, true
	}
	return resource.Quantity{}, false
}

func getTemplateNamesFromSTS(sts *appsv1.StatefulSet) []string {
	if sts == nil {
		return nil
	}
	names := make([]string, 0, len(sts.Spec.VolumeClaimTemplates))
	for _, t := range sts.Spec.VolumeClaimTemplates {
		names = append(names, t.Name)
	}
	return names
}

func getTemplateNameFromPVCName(statefulSetName, pvcName string) (string, bool) {
	ordinal := parseOrdinalFromName(pvcName)
	if ordinal < 0 {
		return "", false
	}
	withoutOrdinalSuffix := strings.TrimSuffix(pvcName, fmt.Sprintf("-%d", ordinal))
	statefulSetSuffix := fmt.Sprintf("-%s", statefulSetName)
	if !strings.HasSuffix(withoutOrdinalSuffix, statefulSetSuffix) {
		return "", false
	}
	templateName := strings.TrimSuffix(withoutOrdinalSuffix, statefulSetSuffix)
	if templateName == "" {
		return "", false
	}
	return templateName, true
}

func getOfflineRestartCandidates(status *marklogicv1.VolumeResizeStatus, statefulSetName string) []string {
	candidates := make([]string, 0)
	for idx := range status.PVCStatuses {
		entry := &status.PVCStatuses[idx]
		if entry.CheckpointType != marklogicv1.PVCResizeCheckpointTypeOfflinePending {
			continue
		}
		entry.PodName = derivePodNameFromPVC(statefulSetName, entry.Name)
		candidates = append(candidates, entry.Name)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return parseOrdinalFromName(candidates[i]) > parseOrdinalFromName(candidates[j])
	})
	return candidates
}

func (oc *OperatorContext) nextRestartCandidate(status *marklogicv1.VolumeResizeStatus, statefulSetName string) *marklogicv1.PVCResizeStatus {
	indices := make([]int, 0)
	for idx := range status.PVCStatuses {
		entry := &status.PVCStatuses[idx]
		if entry.State != marklogicv1.PVCResizeStateRestartPending {
			continue
		}
		if entry.PodName == "" {
			entry.PodName = derivePodNameFromPVC(statefulSetName, entry.Name)
		}
		indices = append(indices, idx)
	}
	if len(indices) == 0 {
		return nil
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return parseOrdinalFromName(status.PVCStatuses[indices[i]].Name) > parseOrdinalFromName(status.PVCStatuses[indices[j]].Name)
	})
	return &status.PVCStatuses[indices[0]]
}

func (oc *OperatorContext) findPVCStatusByName(status *marklogicv1.VolumeResizeStatus, pvcName string) *marklogicv1.PVCResizeStatus {
	for idx := range status.PVCStatuses {
		if status.PVCStatuses[idx].Name == pvcName {
			return &status.PVCStatuses[idx]
		}
	}
	return nil
}

func (oc *OperatorContext) isPodReady(podName string) (bool, error) {
	pod := &corev1.Pod{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return hasPodReadyCondition(pod), nil
}

func (oc *OperatorContext) areStatefulSetPodsReady(currentSts *appsv1.StatefulSet) (bool, error) {
	if currentSts == nil {
		return false, fmt.Errorf("statefulset is nil")
	}

	podList := &corev1.PodList{}
	if err := oc.Client.List(oc.Ctx, podList, client.InNamespace(currentSts.Namespace), client.MatchingLabels(map[string]string{
		"app.kubernetes.io/name":     "marklogic",
		"app.kubernetes.io/instance": currentSts.Name,
	})); err != nil {
		return false, err
	}

	replicas := int32(1)
	if currentSts.Spec.Replicas != nil {
		replicas = *currentSts.Spec.Replicas
	}
	if int32(len(podList.Items)) < replicas {
		return false, nil
	}
	for idx := range podList.Items {
		if !hasPodReadyCondition(&podList.Items[idx]) {
			return false, nil
		}
	}
	return true, nil
}

func hasPodReadyCondition(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func parseOrdinalFromName(name string) int {
	lastDash := strings.LastIndex(name, "-")
	if lastDash == -1 || lastDash == len(name)-1 {
		return -1
	}
	ord, err := strconv.Atoi(name[lastDash+1:])
	if err != nil {
		return -1
	}
	return ord
}

func derivePodNameFromPVC(statefulSetName, pvcName string) string {
	ordinal := parseOrdinalFromName(pvcName)
	if ordinal < 0 {
		return ""
	}
	return fmt.Sprintf("%s-%d", statefulSetName, ordinal)
}

func hasResizeMarker(status *marklogicv1.VolumeResizeStatus, marker string) bool {
	if status == nil || marker == "" {
		return false
	}
	normalizeResizeMarkers(status)
	for _, existing := range status.Markers {
		if existing == marker {
			return true
		}
	}
	return false
}

func addResizeMarker(status *marklogicv1.VolumeResizeStatus, marker string) {
	if status == nil || marker == "" {
		return
	}
	normalizeResizeMarkers(status)
	if hasResizeMarker(status, marker) {
		return
	}
	status.Markers = append(status.Markers, marker)
}

func normalizeResizeMarkers(status *marklogicv1.VolumeResizeStatus) {
	if status == nil || len(status.Warnings) == 0 {
		return
	}

	cleanWarnings := make([]string, 0, len(status.Warnings))
	for _, warning := range status.Warnings {
		if isResizeMarker(warning) {
			if !containsName(status.Markers, warning) {
				status.Markers = append(status.Markers, warning)
			}
			continue
		}
		cleanWarnings = append(cleanWarnings, warning)
	}
	status.Warnings = cleanWarnings
}

func isResizeMarker(value string) bool {
	switch value {
	case resizeMarkerSyncStarted,
		resizeMarkerTemplateSynced,
		resizeMarkerRestartPlanPrepared,
		resizeMarkerVerifyStarted,
		resizeMarkerVerifyCompleted,
		resizeMarkerTemplateRecreateStarted,
		resizeMarkerTemplateDeleted,
		resizeMarkerTemplateRecreated:
		return true
	default:
		return false
	}
}

func containsName(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func resolveResizeStrategy(persistence *marklogicv1.Persistence) marklogicv1.VolumeResizeStrategy {
	if persistence != nil && persistence.ResizeStrategy == marklogicv1.VolumeResizeStrategySequential {
		return marklogicv1.VolumeResizeStrategySequential
	}
	return marklogicv1.VolumeResizeStrategyParallel
}

func (oc *OperatorContext) discoverPrimaryPVCs(sts *appsv1.StatefulSet, targets templateResizeTargets) (*resizePVCDiscovery, error) {
	state := &resizePVCDiscovery{
		expectedNames: []string{},
		foundPVCs:     map[string]*corev1.PersistentVolumeClaim{},
		missingPVCs:   []string{},
		notBoundPVCs:  []string{},
		minByTemplate: map[string]*resource.Quantity{},
		targetByPVC:   map[string]resource.Quantity{},
	}

	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}

	templateNames := make([]string, 0, len(targets))
	for templateName := range targets {
		templateNames = append(templateNames, templateName)
	}
	if len(templateNames) == 0 {
		templateNames = getResizableTemplateNames(sts)
	}
	sort.Strings(templateNames)
	oc.ReqLogger.Info("DEBUG: discoverPrimaryPVCs - discovering PVCs", "templateNames", templateNames, "replicas", replicas, "stsName", sts.Name)
	for _, templateName := range templateNames {
		templateTarget, hasTarget := targets[templateName]
		if !hasTarget {
			continue
		}
		for i := int32(0); i < replicas; i++ {
			name := fmt.Sprintf("%s-%s-%d", templateName, sts.Name, i)
			state.expectedNames = append(state.expectedNames, name)
			state.targetByPVC[name] = templateTarget
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
			if existing, ok := state.minByTemplate[templateName]; !ok || existing == nil || capCopy.Cmp(*existing) < 0 {
				state.minByTemplate[templateName] = &capCopy
			}
		}
	}

	templateStateMap := make(map[string]string)
	for k, v := range state.minByTemplate {
		if v != nil {
			templateStateMap[k] = v.String()
		}
	}
	oc.ReqLogger.Info("DEBUG: discoverPrimaryPVCs - discovery complete", "found", len(state.foundPVCs), "expected", len(state.expectedNames), "missing", state.missingPVCs, "minByTemplate", templateStateMap)
	return state, nil
}

func getResizableTemplateNames(sts *appsv1.StatefulSet) []string {
	if sts == nil {
		return []string{dataDirPVCName}
	}
	templateNames := make([]string, 0, len(sts.Spec.VolumeClaimTemplates))
	for i := range sts.Spec.VolumeClaimTemplates {
		template := sts.Spec.VolumeClaimTemplates[i]
		if template.Name == "" {
			continue
		}
		if template.Spec.Resources.Requests == nil {
			continue
		}
		if _, hasStorageRequest := template.Spec.Resources.Requests[corev1.ResourceStorage]; !hasStorageRequest {
			continue
		}
		templateNames = append(templateNames, template.Name)
	}
	if len(templateNames) == 0 {
		templateNames = append(templateNames, dataDirPVCName)
	}
	return templateNames
}

func resolveResizeTargetsFromSpec(group *marklogicv1.MarklogicGroup) (templateResizeTargets, error) {
	targets := templateResizeTargets{}
	if group == nil {
		return targets, nil
	}
	if group.Spec.Persistence != nil && group.Spec.Persistence.Enabled {
		size, err := resource.ParseQuantity(group.Spec.Persistence.Size)
		if err != nil {
			return nil, fmt.Errorf("invalid persistence size %q", group.Spec.Persistence.Size)
		}
		targets[dataDirPVCName] = size
	}
	if group.Spec.AdditionalVolumeClaimTemplates != nil {
		for _, tmpl := range *group.Spec.AdditionalVolumeClaimTemplates {
			if tmpl.Name == "" || tmpl.Spec.Resources.Requests == nil {
				continue
			}
			size, ok := tmpl.Spec.Resources.Requests[corev1.ResourceStorage]
			if !ok || size.IsZero() {
				continue
			}
			targets[tmpl.Name] = size
		}
	}
	targetMap := make(map[string]string)
	for k, v := range targets {
		targetMap[k] = v.String()
	}
	_ = targetMap
	return targets, nil
}

func primaryResizeTarget(targets templateResizeTargets) resource.Quantity {
	if target, ok := targets[dataDirPVCName]; ok {
		return target
	}
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return resource.Quantity{}
	}
	return targets[names[0]]
}

func compareTargetsWithCurrent(state *resizePVCDiscovery, targets templateResizeTargets) (int, string) {
	hasIncrease := false
	for templateName, target := range targets {
		current, ok := state.minByTemplate[templateName]
		if !ok || current == nil {
			hasIncrease = true
			continue
		}
		cmp := target.Cmp(*current)
		if cmp < 0 {
			return -1, fmt.Sprintf("Shrink is not supported for %s: current=%s target=%s", templateName, current.String(), target.String())
		}
		if cmp > 0 {
			hasIncrease = true
		}
	}
	if hasIncrease {
		return 1, ""
	}
	return 0, ""
}

func resizeTargetForPVCEntry(status *marklogicv1.VolumeResizeStatus, entry *marklogicv1.PVCResizeStatus) (resource.Quantity, error) {
	if entry != nil && entry.RequestedSize != "" {
		if target, err := resource.ParseQuantity(entry.RequestedSize); err == nil {
			return target, nil
		}
	}
	if status != nil && status.TargetSize != "" {
		if target, err := resource.ParseQuantity(status.TargetSize); err == nil {
			return target, nil
		}
	}
	name := ""
	if entry != nil {
		name = entry.Name
	}
	return resource.Quantity{}, fmt.Errorf("unable to resolve resize target for pvc %s", name)
}

func desiredTemplateTargetsFromStatus(status *marklogicv1.VolumeResizeStatus, statefulSetName string) (templateResizeTargets, error) {
	targets := templateResizeTargets{}
	if status == nil {
		return targets, fmt.Errorf("volume resize status is nil")
	}
	for i := range status.PVCStatuses {
		entry := &status.PVCStatuses[i]
		templateName, ok := getTemplateNameFromPVCName(statefulSetName, entry.Name)
		if !ok {
			continue
		}
		target, err := resizeTargetForPVCEntry(status, entry)
		if err != nil {
			return nil, err
		}
		if existing, ok := targets[templateName]; !ok || target.Cmp(existing) > 0 {
			targets[templateName] = target
		}
	}
	if len(targets) == 0 {
		target, err := resource.ParseQuantity(status.TargetSize)
		if err != nil {
			return nil, fmt.Errorf("invalid target size %q", status.TargetSize)
		}
		targets[dataDirPVCName] = target
	}
	return targets, nil
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
			if apierrors.IsForbidden(err) {
				return fmt.Errorf("StorageClass validation requires cluster-scoped access to storageclasses (get/list/watch): %w", err)
			}
			return err
		}
		if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
			return fmt.Errorf("StorageClass %s does not allow volume expansion", scName)
		}
	}
	return nil
}
