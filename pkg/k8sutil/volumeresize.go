// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"encoding/json"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// PVCResizeTimeoutMinutes is the timeout for each PVC resize operation
	PVCResizeTimeoutMinutes = 10
	// VolumeClaimTemplateName is the default name for the data volume claim template
	VolumeClaimTemplateName = "datadir"
)

// ReconcileVolumeResize handles the volume resize workflow for a MarklogicGroup
func (oc *OperatorContext) ReconcileVolumeResize() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	// Check if persistence is enabled
	if cr.Spec.Persistence == nil || !cr.Spec.Persistence.Enabled {
		logger.Info("Persistence not enabled, skipping volume resize check")
		return result.Continue()
	}

	// Get current StatefulSet
	currentSts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet doesn't exist yet, skip resize check
			return result.Continue()
		}
		logger.Error(err, "Failed to get StatefulSet for volume resize check")
		return result.Error(err)
	}

	// Check if resize is needed
	resizeNeeded, currentSize, targetSize, err := oc.checkVolumeResizeNeeded(currentSts, cr)
	if err != nil {
		logger.Error(err, "Failed to check if volume resize is needed")
		return result.Error(err)
	}

	if !resizeNeeded {
		// If a resize was previously completed, clean up the status
		if cr.Status.VolumeResizeStatus != nil &&
			cr.Status.VolumeResizeStatus.Phase == marklogicv1.VolumeResizePhaseCompleted {
			// Keep completed status for observability, but could clean up after a period
			return result.Continue()
		}
		return result.Continue()
	}

	logger.Info("Volume resize needed", "currentSize", currentSize, "targetSize", targetSize)

	// Initialize resize status if not present
	if cr.Status.VolumeResizeStatus == nil {
		return oc.initializeVolumeResizeStatus(currentSize, targetSize)
	}

	// Execute resize based on current phase
	return oc.executeVolumeResizePhase()
}

// checkVolumeResizeNeeded determines if volume resize is required
func (oc *OperatorContext) checkVolumeResizeNeeded(sts *appsv1.StatefulSet, cr *marklogicv1.MarklogicGroup) (bool, string, string, error) {
	logger := oc.ReqLogger

	// Get current PVC size from StatefulSet's volumeClaimTemplates
	var currentSize string
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if vct.Name == VolumeClaimTemplateName {
			if storage, ok := vct.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				currentSize = storage.String()
			}
			break
		}
	}

	if currentSize == "" {
		logger.Info("No datadir volume claim template found in StatefulSet")
		return false, "", "", nil
	}

	// Get target size from CR spec
	targetSize := cr.Spec.Persistence.Size
	if targetSize == "" {
		return false, currentSize, "", nil
	}

	// Parse and compare sizes
	currentQty, err := resource.ParseQuantity(currentSize)
	if err != nil {
		return false, currentSize, targetSize, fmt.Errorf("failed to parse current size: %w", err)
	}

	targetQty, err := resource.ParseQuantity(targetSize)
	if err != nil {
		return false, currentSize, targetSize, fmt.Errorf("failed to parse target size: %w", err)
	}

	// Check if target is larger than current
	if targetQty.Cmp(currentQty) <= 0 {
		// Target is same or smaller, no resize needed (PVC shrinking not supported)
		if targetQty.Cmp(currentQty) < 0 {
			logger.Info("Target size is smaller than current size, PVC shrinking not supported",
				"currentSize", currentSize, "targetSize", targetSize)
		}
		return false, currentSize, targetSize, nil
	}

	// Check if a resize is already in progress or completed
	if cr.Status.VolumeResizeStatus != nil {
		phase := cr.Status.VolumeResizeStatus.Phase
		if phase == marklogicv1.VolumeResizePhaseCompleted {
			// Check if this is a new resize request (different target size)
			if cr.Status.VolumeResizeStatus.TargetSize == targetSize {
				return false, currentSize, targetSize, nil
			}
		}
		// For any other phase, continue with resize process
		if phase != marklogicv1.VolumeResizePhaseNone && phase != marklogicv1.VolumeResizePhaseFailed {
			return true, currentSize, targetSize, nil
		}
	}

	return true, currentSize, targetSize, nil
}

// initializeVolumeResizeStatus initializes the resize status and starts the process
func (oc *OperatorContext) initializeVolumeResizeStatus(currentSize, targetSize string) result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	now := metav1.Now()
	cr.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{
		Phase:        marklogicv1.VolumeResizePhaseValidating,
		StartTime:    &now,
		TargetSize:   targetSize,
		OriginalSize: currentSize,
		Message:      "Starting resize validation",
	}

	if err := oc.updateStatus(); err != nil {
		logger.Error(err, "Failed to initialize volume resize status")
		return result.Error(err)
	}

	logger.Info("INFO: Starting resize validation")
	oc.Recorder.Event(cr, "Normal", "VolumeResizeStarted", fmt.Sprintf("Starting volume resize from %s to %s", currentSize, targetSize))

	return result.RequeueSoon(1)
}

// executeVolumeResizePhase executes the current phase of the resize operation
func (oc *OperatorContext) executeVolumeResizePhase() result.ReconcileResult {
	cr := oc.MarklogicGroup
	phase := cr.Status.VolumeResizeStatus.Phase

	switch phase {
	case marklogicv1.VolumeResizePhaseValidating:
		return oc.validateResizePrerequisites()
	case marklogicv1.VolumeResizePhaseResizingPVCs:
		return oc.resizePVCs()
	case marklogicv1.VolumeResizePhaseWaitingForPVCResize:
		return oc.waitForPVCResize()
	case marklogicv1.VolumeResizePhaseBackingUpStatefulSet:
		return oc.backupStatefulSet()
	case marklogicv1.VolumeResizePhaseDeletingStatefulSet:
		return oc.deleteStatefulSetWithOrphan()
	case marklogicv1.VolumeResizePhaseRecreatingStatefulSet:
		return oc.recreateStatefulSet()
	case marklogicv1.VolumeResizePhaseRestartingPods:
		return oc.restartPodsForFilesystemResize()
	case marklogicv1.VolumeResizePhaseVerifying:
		return oc.verifyPodHealth()
	case marklogicv1.VolumeResizePhaseCompleted, marklogicv1.VolumeResizePhaseFailed:
		return result.Continue()
	default:
		return result.Continue()
	}
}

// Step 1: validateResizePrerequisites validates all prerequisites for volume resize
func (oc *OperatorContext) validateResizePrerequisites() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	logger.Info("INFO: Starting resize validation")

	// Get StatefulSet
	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("StatefulSet not found: %v", err))
	}

	// Get StorageClass and verify allowVolumeExpansion
	var storageClassName string
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if vct.Name == VolumeClaimTemplateName && vct.Spec.StorageClassName != nil {
			storageClassName = *vct.Spec.StorageClassName
			break
		}
	}

	if storageClassName == "" && cr.Spec.Persistence.StorageClassName != "" {
		storageClassName = cr.Spec.Persistence.StorageClassName
	}

	if storageClassName != "" {
		storageClass := &storagev1.StorageClass{}
		if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: storageClassName}, storageClass); err != nil {
			return oc.setResizeFailure(fmt.Sprintf("Failed to get StorageClass %s: %v", storageClassName, err))
		}

		if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
			return oc.setResizeFailure(fmt.Sprintf("StorageClass %s does not allow volume expansion", storageClassName))
		}
		logger.Info("StorageClass allows volume expansion", "storageClass", storageClassName)
	}

	// Get PVCs for StatefulSet and verify they are Bound
	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err))
	}

	// Verify all PVCs are Bound
	for _, pvc := range pvcs {
		if pvc.Status.Phase != corev1.ClaimBound {
			return oc.setResizeFailure(fmt.Sprintf("PVC %s is not in Bound state: %s", pvc.Name, pvc.Status.Phase))
		}
	}

	// Verify new size > current size (already done in checkVolumeResizeNeeded, but verify again)
	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	originalQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.OriginalSize)
	if targetQty.Cmp(originalQty) <= 0 {
		return oc.setResizeFailure("Target size must be greater than current size")
	}

	// Update status to next phase
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseResizingPVCs
	cr.Status.VolumeResizeStatus.TotalPVCs = int32(len(pvcs))
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Validation passed, identified %d PVCs to resize", len(pvcs))

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info(fmt.Sprintf("INFO: Identified %d PVCs with current sizes", len(pvcs)))
	oc.Recorder.Event(cr, "Normal", "VolumeResizeValidated", cr.Status.VolumeResizeStatus.Message)

	return result.RequeueSoon(1)
}

// Step 3: resizePVCs updates the PVC storage requests
func (oc *OperatorContext) resizePVCs() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err))
	}

	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err))
	}

	targetQty, err := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to parse target size: %v", err))
	}

	resizedCount := int32(0)
	for _, pvc := range pvcs {
		currentStorage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if currentStorage.Cmp(targetQty) >= 0 {
			resizedCount++
			continue
		}

		// Update PVC storage request
		pvcCopy := pvc.DeepCopy()
		pvcCopy.Spec.Resources.Requests[corev1.ResourceStorage] = targetQty

		if err := oc.Client.Update(oc.Ctx, pvcCopy); err != nil {
			return oc.setResizeFailure(fmt.Sprintf("Failed to resize PVC %s: %v", pvc.Name, err))
		}

		logger.Info(fmt.Sprintf("INFO: Initiated resize for PVC %s", pvc.Name))
		oc.Recorder.Event(cr, "Normal", "PVCResizeInitiated", fmt.Sprintf("Initiated resize for PVC %s", pvc.Name))
		resizedCount++
	}

	cr.Status.VolumeResizeStatus.PVCsResized = resizedCount
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseWaitingForPVCResize
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Initiated resize for %d PVCs, waiting for completion", resizedCount)

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(5)
}

// Step 4: waitForPVCResize polls PVC conditions for resize completion
func (oc *OperatorContext) waitForPVCResize() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err))
	}

	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err))
	}

	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)

	allResized := true
	filesystemResizePending := false
	warnings := []string{}

	for _, pvc := range pvcs {
		// Check PVC conditions
		for _, condition := range pvc.Status.Conditions {
			if condition.Type == corev1.PersistentVolumeClaimResizing && condition.Status == corev1.ConditionTrue {
				allResized = false
				logger.Info("PVC still resizing", "pvc", pvc.Name)
			}
			if condition.Type == corev1.PersistentVolumeClaimFileSystemResizePending && condition.Status == corev1.ConditionTrue {
				filesystemResizePending = true
				warning := fmt.Sprintf("WARNING: Filesystem resize pending for PVC %s, pod restart required", pvc.Name)
				warnings = append(warnings, warning)
				logger.Info(warning)
			}
		}

		// Check if capacity has been updated
		if pvc.Status.Capacity != nil {
			actualCapacity := pvc.Status.Capacity[corev1.ResourceStorage]
			if actualCapacity.Cmp(targetQty) < 0 {
				allResized = false
			}
		} else {
			allResized = false
		}
	}

	// Check timeout
	if cr.Status.VolumeResizeStatus.StartTime != nil {
		elapsed := time.Since(cr.Status.VolumeResizeStatus.StartTime.Time)
		timeoutDuration := time.Duration(PVCResizeTimeoutMinutes*int(cr.Status.VolumeResizeStatus.TotalPVCs)) * time.Minute
		if elapsed > timeoutDuration {
			return oc.setResizeFailure(fmt.Sprintf("Timeout waiting for PVC resize after %v", elapsed))
		}
	}

	if !allResized && !filesystemResizePending {
		cr.Status.VolumeResizeStatus.Message = "Waiting for PVC resize to complete"
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// Update status
	cr.Status.VolumeResizeStatus.FileSystemResizePending = filesystemResizePending
	cr.Status.VolumeResizeStatus.Warnings = warnings
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseBackingUpStatefulSet
	cr.Status.VolumeResizeStatus.Message = "PVC storage resize complete, backing up StatefulSet"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("INFO: PVC resize complete, proceeding to StatefulSet backup")
	return result.RequeueSoon(1)
}

// Step 5: backupStatefulSet stores the current StatefulSet spec in CR status
func (oc *OperatorContext) backupStatefulSet() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet for backup: %v", err))
	}

	// Serialize StatefulSet spec to JSON for backup
	stsSpecJSON, err := json.Marshal(sts.Spec)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to serialize StatefulSet spec: %v", err))
	}

	cr.Status.VolumeResizeStatus.StatefulSetBackup = string(stsSpecJSON)
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseDeletingStatefulSet
	cr.Status.VolumeResizeStatus.Message = "StatefulSet spec backed up, deleting with orphan policy"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("INFO: Backed up StatefulSet spec")
	oc.Recorder.Event(cr, "Normal", "StatefulSetBackedUp", "StatefulSet spec backed up for recovery")

	return result.RequeueSoon(1)
}

// Step 6: deleteStatefulSetWithOrphan deletes the StatefulSet with orphan propagation policy
func (oc *OperatorContext) deleteStatefulSetWithOrphan() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet already deleted, proceed to recreate
			cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRecreatingStatefulSet
			cr.Status.VolumeResizeStatus.Message = "StatefulSet deleted, recreating with updated template"
			if err := oc.updateStatus(); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(1)
		}
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err))
	}

	// Delete StatefulSet with orphan propagation policy (keeps pods running)
	orphanPolicy := metav1.DeletePropagationOrphan
	deleteOptions := client.DeleteOptions{
		PropagationPolicy: &orphanPolicy,
	}

	if err := oc.Client.Delete(oc.Ctx, sts, &deleteOptions); err != nil {
		if apierrors.IsNotFound(err) {
			cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRecreatingStatefulSet
			cr.Status.VolumeResizeStatus.Message = "StatefulSet deleted, recreating with updated template"
			if err := oc.updateStatus(); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(1)
		}
		return oc.setResizeFailure(fmt.Sprintf("CRITICAL: Failed to delete StatefulSet: %v", err))
	}

	logger.Info("INFO: Deleted StatefulSet with orphan policy")
	oc.Recorder.Event(cr, "Normal", "StatefulSetDeleted", "StatefulSet deleted with orphan policy, pods remain running")

	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRecreatingStatefulSet
	cr.Status.VolumeResizeStatus.Message = "StatefulSet deleted, recreating with updated template"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(2)
}

// Step 7: recreateStatefulSet recreates the StatefulSet with updated volumeClaimTemplate
func (oc *OperatorContext) recreateStatefulSet() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	// Check if StatefulSet already exists
	_, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err == nil {
		// StatefulSet exists, check if it has the correct size
		logger.Info("StatefulSet already exists, proceeding to pod restart phase")
		if cr.Status.VolumeResizeStatus.FileSystemResizePending {
			cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRestartingPods
			cr.Status.VolumeResizeStatus.Message = "StatefulSet recreated, restarting pods for filesystem resize"
			cr.Status.VolumeResizeStatus.LastResizedPodIndex = -1
		} else {
			cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifying
			cr.Status.VolumeResizeStatus.Message = "StatefulSet recreated, verifying pod health"
		}
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(1)
	}

	if !apierrors.IsNotFound(err) {
		return oc.setResizeFailure(fmt.Sprintf("Failed to check StatefulSet existence: %v", err))
	}

	// Generate new StatefulSet with updated volumeClaimTemplate
	groupLabels := cr.Labels
	if groupLabels == nil {
		groupLabels = getSelectorLabels(cr.Spec.Name)
	}
	groupLabels["app.kubernetes.io/instance"] = cr.Spec.Name
	groupAnnotations := cr.GetAnnotations()
	delete(groupAnnotations, "banzaicloud.com/last-applied")

	objectMeta := generateObjectMeta(cr.Spec.Name, cr.Namespace, groupLabels, groupAnnotations)
	containerParams := generateContainerParams(cr)
	statefulSetParams := generateStatefulSetsParams(cr)

	// Generate new StatefulSet definition
	statefulSetDef := generateStatefulSetsDef(objectMeta, statefulSetParams, marklogicServerAsOwner(cr), containerParams)

	// Create the StatefulSet
	if err := oc.Client.Create(oc.Ctx, statefulSetDef); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logger.Info("StatefulSet already exists during recreation")
		} else {
			return oc.setResizeFailure(fmt.Sprintf("CRITICAL: Failed to recreate StatefulSet: %v", err))
		}
	}

	logger.Info("INFO: Recreated StatefulSet with new template")
	oc.Recorder.Event(cr, "Normal", "StatefulSetRecreated", "StatefulSet recreated with updated volume claim template")

	if cr.Status.VolumeResizeStatus.FileSystemResizePending {
		cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRestartingPods
		cr.Status.VolumeResizeStatus.Message = "StatefulSet recreated, restarting pods for filesystem resize"
		cr.Status.VolumeResizeStatus.LastResizedPodIndex = -1
	} else {
		cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifying
		cr.Status.VolumeResizeStatus.Message = "StatefulSet recreated, verifying pod health"
	}

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(5)
}

// Step 8: restartPodsForFilesystemResize deletes pods sequentially if filesystem resize is pending
func (oc *OperatorContext) restartPodsForFilesystemResize() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	replicas := int32(1)
	if cr.Spec.Replicas != nil {
		replicas = *cr.Spec.Replicas
	}

	lastIndex := cr.Status.VolumeResizeStatus.LastResizedPodIndex
	nextIndex := lastIndex + 1

	if nextIndex >= replicas {
		// All pods restarted
		cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifying
		cr.Status.VolumeResizeStatus.Message = "All pods restarted, verifying health"
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	podName := fmt.Sprintf("%s-%d", cr.Spec.Name, nextIndex)

	// Get the pod
	pod := &corev1.Pod{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: cr.Namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			// Pod doesn't exist, skip to next
			cr.Status.VolumeResizeStatus.LastResizedPodIndex = nextIndex
			if err := oc.updateStatus(); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(2)
		}
		return oc.setResizeFailure(fmt.Sprintf("Failed to get pod %s: %v", podName, err))
	}

	// Check if pod is ready before proceeding
	podReady := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			podReady = true
			break
		}
	}

	if !podReady && pod.DeletionTimestamp == nil {
		// Pod is not ready yet, wait
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for pod %s to be ready before restart", podName)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// Check if we've already deleted this pod and it's being recreated
	if pod.DeletionTimestamp != nil {
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for pod %s to be deleted", podName)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	// Delete the pod
	if err := oc.Client.Delete(oc.Ctx, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return oc.setResizeFailure(fmt.Sprintf("Failed to delete pod %s: %v", podName, err))
		}
	}

	logger.Info(fmt.Sprintf("INFO: Restarting pod %s for filesystem resize", podName))
	oc.Recorder.Event(cr, "Normal", "PodRestarted", fmt.Sprintf("Restarting pod %s for filesystem resize", podName))

	cr.Status.VolumeResizeStatus.LastResizedPodIndex = nextIndex
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Restarted pod %s, waiting for ready state", podName)

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(15)
}

// Step 9 & 10: verifyPodHealth confirms all pods are healthy and updates CR status
func (oc *OperatorContext) verifyPodHealth() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err))
	}

	// Check if all replicas are ready
	expectedReplicas := int32(1)
	if cr.Spec.Replicas != nil {
		expectedReplicas = *cr.Spec.Replicas
	}

	if sts.Status.ReadyReplicas != expectedReplicas {
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for all pods to be ready: %d/%d", sts.Status.ReadyReplicas, expectedReplicas)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// Verify PVCs are mounted with new size
	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get PVCs: %v", err))
	}

	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	allCorrectSize := true
	for _, pvc := range pvcs {
		if pvc.Status.Capacity != nil {
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]
			if capacity.Cmp(targetQty) < 0 {
				allCorrectSize = false
				logger.Info("PVC not yet at target size", "pvc", pvc.Name, "current", capacity.String(), "target", targetQty.String())
			}
		}
	}

	if !allCorrectSize {
		cr.Status.VolumeResizeStatus.Message = "Waiting for PVC capacities to update"
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// Calculate duration
	var duration string
	if cr.Status.VolumeResizeStatus.StartTime != nil {
		duration = time.Since(cr.Status.VolumeResizeStatus.StartTime.Time).Round(time.Second).String()
	}

	// Update status to completed
	now := metav1.Now()
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseCompleted
	cr.Status.VolumeResizeStatus.CompletionTime = &now
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Volume resize completed successfully in %s", duration)
	cr.Status.VolumeResizeStatus.StatefulSetBackup = "" // Clear backup

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("SUCCESS: All pods healthy with resized volumes")
	logger.Info("SUCCESS: Resize completed", "duration", duration)
	oc.Recorder.Event(cr, "Normal", "VolumeResizeCompleted", fmt.Sprintf("Volume resize completed successfully in %s", duration))

	return result.Continue()
}

// Helper functions

// getPVCsForStatefulSet returns all PVCs associated with the StatefulSet
func (oc *OperatorContext) getPVCsForStatefulSet(sts *appsv1.StatefulSet) ([]corev1.PersistentVolumeClaim, error) {
	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}

	var pvcs []corev1.PersistentVolumeClaim
	for i := int32(0); i < replicas; i++ {
		pvcName := fmt.Sprintf("%s-%s-%d", VolumeClaimTemplateName, sts.Name, i)
		pvc := &corev1.PersistentVolumeClaim{}
		if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: sts.Namespace, Name: pvcName}, pvc); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			// PVC not found for this ordinal, might not exist yet
			continue
		}
		pvcs = append(pvcs, *pvc)
	}

	return pvcs, nil
}

// setResizeFailure sets the resize status to failed and returns an error result
func (oc *OperatorContext) setResizeFailure(message string) result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	logger.Error(nil, "Volume resize failed", "message", message)

	if cr.Status.VolumeResizeStatus == nil {
		cr.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{}
	}

	now := metav1.Now()
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseFailed
	cr.Status.VolumeResizeStatus.CompletionTime = &now
	cr.Status.VolumeResizeStatus.Message = message

	if err := oc.updateStatus(); err != nil {
		logger.Error(err, "Failed to update status after resize failure")
	}

	oc.Recorder.Event(cr, "Warning", "VolumeResizeFailed", message)

	return result.Done()
}

// updateStatus updates the MarklogicGroup status - call this AFTER setting up the patch
// Usage: patch := client.MergeFrom(cr.DeepCopy()); modify cr.Status...; oc.Client.Status().Patch(ctx, cr, patch)
func (oc *OperatorContext) updateVolumeResizeStatus(updateFn func()) error {
	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	updateFn()
	return oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch)
}

// updateStatus updates the MarklogicGroup status using Status().Update()
// This is simpler and works when the object has already been modified
func (oc *OperatorContext) updateStatus() error {
	return oc.Client.Status().Update(oc.Ctx, oc.MarklogicGroup)
}

// ReconcileVolumeResizeForCluster handles volume resize for MarklogicCluster (delegates to groups)
func (cc *ClusterContext) ReconcileVolumeResizeForCluster() (reconcile.Result, error) {
	// Volume resize for cluster is handled at the group level
	// This method is a placeholder for cluster-level coordination if needed
	return reconcile.Result{}, nil
}
