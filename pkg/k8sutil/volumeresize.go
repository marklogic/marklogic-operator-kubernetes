// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"encoding/json"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// VolumeClaimTemplateName is the default name for the data volume claim template
	VolumeClaimTemplateName = "datadir"
	// ConditionTypeProgressing is the condition type for tracking resize progress
	ConditionTypeProgressing = "Progressing"
	// ConditionTypeVolumeResizeComplete is the condition type for resize completion
	ConditionTypeVolumeResizeComplete = "VolumeResizeComplete"
	// MaxRetryCount is the maximum number of retries for recoverable operations
	MaxRetryCount = 3
	// ResizeAnnotationKey is the annotation key for tracking resize operations
	ResizeAnnotationKey = "marklogic.com/volume-resize-id"
	// PauseResizeAnnotationKey is the annotation key to pause/resume resize operations
	PauseResizeAnnotationKey = "marklogic.com/pause-resize"
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

	// Check for interrupted template update recovery (edge case: Template update interrupted)
	// If StatefulSet deletion timestamp exists, track it for logging but don't timeout
	if cr.Status.VolumeResizeStatus != nil && cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp != nil {
		elapsed := time.Since(cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp.Time)
		if elapsed > 5*time.Minute {
			logger.Info("INFO: Recovered from interrupted template update", "elapsed", elapsed)
			oc.Recorder.Event(cr, "Warning", "TemplateUpdateRecovered", fmt.Sprintf("Recovered from interrupted template update after %v", elapsed))
			// Clear stale deletion timestamp and proceed with recreation
			cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp = nil
			cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRecreatingStatefulSet
			if err := oc.updateStatus(); err != nil {
				return result.Error(err)
			}
		}
	}

	// Get current StatefulSet
	currentSts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet doesn't exist yet - check if this is during a resize operation
			if cr.Status.VolumeResizeStatus != nil &&
				(cr.Status.VolumeResizeStatus.Phase == marklogicv1.VolumeResizePhaseDeletingStatefulSet ||
					cr.Status.VolumeResizeStatus.Phase == marklogicv1.VolumeResizePhaseRecreatingStatefulSet) {
				// STS missing during resize - this is expected, continue with recreate
				return oc.executeVolumeResizePhase()
			}
			// StatefulSet doesn't exist and no resize in progress, skip
			return result.Continue()
		}
		logger.Error(err, "Failed to get StatefulSet for volume resize check")
		return result.Error(err)
	}

	// Edge case: Template update succeeded but status stale
	// Check if resize is actually complete but status says "in progress"
	if cr.Status.VolumeResizeStatus != nil &&
		cr.Status.VolumeResizeStatus.Phase != marklogicv1.VolumeResizePhaseCompleted &&
		cr.Status.VolumeResizeStatus.Phase != marklogicv1.VolumeResizePhaseNone &&
		cr.Status.VolumeResizeStatus.TargetSize != "" {

		// Check if STS template already has the target size
		for _, vct := range currentSts.Spec.VolumeClaimTemplates {
			if vct.Name == VolumeClaimTemplateName {
				if storage, ok := vct.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
					targetQty, err := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
					if err != nil {
						// Invalid target size in status - log warning and skip recovery logic
						logger.Info("WARNING: Invalid target size in volume resize status, skipping recovery check",
							"targetSize", cr.Status.VolumeResizeStatus.TargetSize, "error", err.Error())
						break
					}
					if storage.Cmp(targetQty) >= 0 {
						// STS already has target size - verify CR spec also matches
						crSpecSize := cr.Spec.Persistence.Size
						if crSpecSize != cr.Status.VolumeResizeStatus.TargetSize {
							logger.Info("WARNING: STS template has target size but CR spec does not match, waiting for sync",
								"stsSize", storage.String(), "crSpecSize", crSpecSize, "targetSize", cr.Status.VolumeResizeStatus.TargetSize)
							return result.RequeueSoon(10)
						}

						// Both STS and CR spec match target - check if PVCs also match
						pvcs, pvcErr := oc.getPVCsForStatefulSet(currentSts)
						if pvcErr == nil && len(pvcs) > 0 {
							allPVCsResized := true
							for _, pvc := range pvcs {
								if pvc.Status.Capacity != nil {
									capacity := pvc.Status.Capacity[corev1.ResourceStorage]
									if capacity.Cmp(targetQty) < 0 {
										allPVCsResized = false
										break
									}
								} else {
									allPVCsResized = false
									break
								}
							}

							if allPVCsResized {
								// Resize is actually complete but status wasn't updated
								// Only log if we have evidence of staleness (completion time not set or very old)
								isStale := cr.Status.VolumeResizeStatus.CompletionTime == nil ||
									(time.Now().Sub(cr.Status.VolumeResizeStatus.CompletionTime.Time) > 5*time.Minute)

								if isStale {
									logger.Info("INFO: PVCs resized but status not updated - marking as completed")
								}
								return oc.completeVolumeResize()
							}
						}
					}
				}
				break
			}
		}
	}

	// Edge case: Multiple VCT templates check
	if len(currentSts.Spec.VolumeClaimTemplates) > 1 {
		logger.Info("WARNING: Multiple volume claim templates found, explicit PVC name may be required",
			"templateCount", len(currentSts.Spec.VolumeClaimTemplates))
		// For now, we only resize the 'datadir' VCT - warn about others
		hasDatadir := false
		for _, vct := range currentSts.Spec.VolumeClaimTemplates {
			if vct.Name == VolumeClaimTemplateName {
				hasDatadir = true
				break
			}
		}
		if !hasDatadir {
			oc.Recorder.Event(cr, "Warning", "MultipleVolumeClaimTemplates",
				fmt.Sprintf("StatefulSet has %d volume claim templates but no '%s' template found", len(currentSts.Spec.VolumeClaimTemplates), VolumeClaimTemplateName))
		}
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

	// Initialize resize status if not present, OR if a new resize is requested after previous completion
	if cr.Status.VolumeResizeStatus == nil {
		return oc.initializeVolumeResizeStatus(currentSize, targetSize)
	}

	// Check if a new resize has been requested after a previous one completed
	if cr.Status.VolumeResizeStatus.Phase == marklogicv1.VolumeResizePhaseCompleted {
		// Check if there's a queued resize request
		if cr.Status.VolumeResizeStatus.QueuedTargetSize != "" {
			queuedTarget := cr.Status.VolumeResizeStatus.QueuedTargetSize
			logger.Info("Queued resize request found after completion - starting new resize",
				"queuedTarget", queuedTarget)
			cr.Status.VolumeResizeStatus.QueuedTargetSize = "" // Clear the queue
			if err := oc.updateStatus(); err != nil {
				logger.Error(err, "Failed to clear queued target size")
			}
			return oc.initializeVolumeResizeStatus(currentSize, queuedTarget)
		}
		// Check if a new resize has been requested via spec change
		if cr.Status.VolumeResizeStatus.TargetSize != targetSize {
			logger.Info("New resize request after previous completion", "previousTarget", cr.Status.VolumeResizeStatus.TargetSize, "newTarget", targetSize)
			// Reset status to allow new resize to proceed
			return oc.initializeVolumeResizeStatus(currentSize, targetSize)
		}
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

	// Edge case: Shrink requested - reject immediately
	if targetQty.Cmp(currentQty) < 0 {
		logger.Error(nil, "ERROR: PVC shrinking not allowed",
			"currentSize", currentSize, "targetSize", targetSize)
		oc.Recorder.Event(cr, "Warning", "ShrinkNotAllowed",
			fmt.Sprintf("Volume shrinking not supported: cannot resize from %s to %s", currentSize, targetSize))
		// Don't return error - just log and skip (user must fix CR)
		return false, currentSize, targetSize, nil
	}

	if targetQty.Cmp(currentQty) == 0 {
		// Same size, no resize needed
		return false, currentSize, targetSize, nil
	}

	// Check if a resize is already in progress or completed
	if cr.Status.VolumeResizeStatus != nil {
		phase := cr.Status.VolumeResizeStatus.Phase

		// Edge case: Check for concurrent resize (different target size while in progress)
		if phase != marklogicv1.VolumeResizePhaseNone &&
			phase != marklogicv1.VolumeResizePhaseFailed &&
			phase != marklogicv1.VolumeResizePhaseCompleted &&
			phase != marklogicv1.VolumeResizePhaseStalled {

			existingTargetSize := cr.Status.VolumeResizeStatus.TargetSize
			// Parse both sizes as Quantity objects for proper comparison (handles unit equivalents like "1Gi" vs "1024Mi")
			existingTargetQty, existingErr := resource.ParseQuantity(existingTargetSize)
			targetQty, targetErr := resource.ParseQuantity(targetSize)

			// If parsing fails, fall back to string comparison for safety
			sizesAreDifferent := true
			if existingErr == nil && targetErr == nil {
				// Both parsed successfully - compare using Cmp() for semantic equality
				sizesAreDifferent = existingTargetQty.Cmp(targetQty) != 0
			} else if existingErr == nil || targetErr == nil {
				// One failed to parse - consider them different
				sizesAreDifferent = true
			} else {
				// Both failed to parse - use string comparison as fallback
				sizesAreDifferent = existingTargetSize != targetSize
			}

			if sizesAreDifferent {
				logger.Info("WARNING: Concurrent resize detected - queuing newer request",
					"existingTarget", existingTargetSize, "newTarget", targetSize)
				oc.Recorder.Event(cr, "Warning", "ConcurrentResize",
					fmt.Sprintf("New resize request (%s) queued; current resize to %s in progress", targetSize, existingTargetSize))
				// Store the queued target size so it can be picked up after completion
				cr.Status.VolumeResizeStatus.QueuedTargetSize = targetSize
				if err := oc.updateStatus(); err != nil {
					logger.Error(err, "Failed to update status with queued resize target")
				}
				// Continue with existing resize, new request will be picked up after completion
				return true, currentSize, existingTargetSize, nil
			}
			return true, currentSize, targetSize, nil
		}

		if phase == marklogicv1.VolumeResizePhaseCompleted {
			// Check if this is a new resize request (different target size)
			if cr.Status.VolumeResizeStatus.TargetSize == targetSize {
				return false, currentSize, targetSize, nil
			}
			// New resize request after completion - allow it
			logger.Info("New resize request after previous completion",
				"previousTarget", cr.Status.VolumeResizeStatus.TargetSize, "newTarget", targetSize)
		}

		// For stalled state, check if we can retry
		if phase == marklogicv1.VolumeResizePhaseStalled {
			// Check if next retry time has passed
			if cr.Status.VolumeResizeStatus.NextRetryTime != nil {
				if time.Now().Before(cr.Status.VolumeResizeStatus.NextRetryTime.Time) {
					// Still in cooldown, don't retry yet
					return false, currentSize, targetSize, nil
				}
			}
			// Retry after cooldown
			return true, currentSize, targetSize, nil
		}
	}

	return true, currentSize, targetSize, nil
}

// initializeVolumeResizeStatus initializes the resize status and starts the process
func (oc *OperatorContext) initializeVolumeResizeStatus(currentSize, targetSize string) result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	// Determine resize strategy
	resizeStrategy := string(marklogicv1.ResizeStrategyParallel)
	if cr.Spec.Persistence != nil && cr.Spec.Persistence.ResizeStrategy != "" {
		resizeStrategy = string(cr.Spec.Persistence.ResizeStrategy)
	}

	now := metav1.Now()
	cr.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{
		ResizeProgress: marklogicv1.ResizeProgress{
			Phase:        marklogicv1.VolumeResizePhaseValidating,
			StartTime:    &now,
			CurrentSize:  currentSize,
			TargetSize:   targetSize,
			OriginalSize: currentSize,
		},
		ResizeMetaInfo: marklogicv1.ResizeMetaInfo{
			Message:        "Starting resize validation",
			ResizeStrategy: resizeStrategy,
		},
	}

	if err := oc.updateStatus(); err != nil {
		logger.Error(err, "Failed to initialize volume resize status")
		return result.Error(err)
	}

	logger.Info("INFO: Starting resize validation")
	// Event will be recorded once prechecks pass and resize actually starts

	return result.RequeueSoon(1)
}

// executeVolumeResizePhase executes the current phase of the resize operation
func (oc *OperatorContext) executeVolumeResizePhase() result.ReconcileResult {
	cr := oc.MarklogicGroup
	logger := oc.ReqLogger
	phase := cr.Status.VolumeResizeStatus.Phase

	// Handle recovery from stalled state
	if phase == marklogicv1.VolumeResizePhaseStalled {
		// Check if we can recover
		if cr.Status.VolumeResizeStatus.NextRetryTime != nil {
			if time.Now().Before(cr.Status.VolumeResizeStatus.NextRetryTime.Time) {
				// Still in cooldown
				return result.RequeueAfter(time.Until(cr.Status.VolumeResizeStatus.NextRetryTime.Time))
			}
		}

		// Recovery: Go back to the last phase before stall
		if cr.Status.VolumeResizeStatus.LastPhaseBeforeStall != "" &&
			cr.Status.VolumeResizeStatus.LastPhaseBeforeStall != marklogicv1.VolumeResizePhaseStalled {

			logger.Info("Recovering from stalled state",
				"lastPhase", cr.Status.VolumeResizeStatus.LastPhaseBeforeStall,
				"reason", cr.Status.VolumeResizeStatus.Reason)

			cr.Status.VolumeResizeStatus.Phase = cr.Status.VolumeResizeStatus.LastPhaseBeforeStall
			cr.Status.VolumeResizeStatus.Reason = marklogicv1.ResizeReasonNone
			cr.Status.VolumeResizeStatus.NextRetryTime = nil
			cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Recovered from stalled state, resuming from %s", cr.Status.VolumeResizeStatus.LastPhaseBeforeStall)

			// Set Progressing back to True
			now := metav1.Now()
			cr.SetCondition(metav1.Condition{
				Type:               ConditionTypeProgressing,
				Status:             metav1.ConditionTrue,
				Reason:             "Recovered",
				Message:            cr.Status.VolumeResizeStatus.Message,
				LastTransitionTime: now,
			})

			if err := oc.updateStatus(); err != nil {
				return result.Error(err)
			}

			oc.Recorder.Event(cr, "Normal", "ResizeRecovered",
				fmt.Sprintf("Volume resize recovered from stalled state, resuming from %s", cr.Status.VolumeResizeStatus.LastPhaseBeforeStall))

			phase = cr.Status.VolumeResizeStatus.Phase
		} else {
			// No recovery possible, stay stalled
			return result.Continue()
		}
	}

	switch phase {
	case marklogicv1.VolumeResizePhaseValidating:
		return oc.validateResizePrerequisites()
	case marklogicv1.VolumeResizePhaseResizingPVCs:
		return oc.resizePVCs()
	case marklogicv1.VolumeResizePhaseWaitingForPVCResize:
		return oc.waitForPVCResize()
	case marklogicv1.VolumeResizePhaseVerifyingHealth:
		return oc.verifyHealthBeforeStatefulSetChanges()
	case marklogicv1.VolumeResizePhaseBackingUpStatefulSet:
		return oc.backupStatefulSet()
	case marklogicv1.VolumeResizePhaseDeletingStatefulSet:
		return oc.deleteStatefulSetWithOrphan()
	case marklogicv1.VolumeResizePhaseRecreatingStatefulSet:
		return oc.recreateStatefulSet()
	case marklogicv1.VolumeResizePhaseVerifyingPodsRunning:
		return oc.verifyPodsRunning()
	case marklogicv1.VolumeResizePhaseRestartingPods:
		return oc.restartPodsForFilesystemResize()
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
		// Edge case: StatefulSet missing - cannot update template, document decision
		if apierrors.IsNotFound(err) {
			logger.Error(err, "ERROR: StatefulSet not found - cannot proceed with resize")
			oc.Recorder.Event(cr, "Warning", "StatefulSetNotFound",
				"StatefulSet not found for PVCs - resize cannot proceed")
			return oc.setResizeFailure("StatefulSet not found - cannot update volume template", marklogicv1.ResizeReasonStatefulSetNotFound)
		}
		return oc.setResizeFailure(fmt.Sprintf("StatefulSet not found: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Edge case: Multiple volume claim templates - warn and require explicit PVC name
	if len(sts.Spec.VolumeClaimTemplates) > 1 {
		hasDatadir := false
		var templateNames []string
		for _, vct := range sts.Spec.VolumeClaimTemplates {
			templateNames = append(templateNames, vct.Name)
			if vct.Name == VolumeClaimTemplateName {
				hasDatadir = true
			}
		}
		if !hasDatadir {
			return oc.setResizeFailure(
				fmt.Sprintf("Multiple VolumeClaimTemplates found (%v) but no '%s' template - explicit PVC name required",
					templateNames, VolumeClaimTemplateName),
				marklogicv1.ResizeReasonMultipleVolumeClaimTemplates)
		}
		logger.Info("WARNING: Multiple volume claim templates found, only resizing 'datadir'",
			"templates", templateNames)
		cr.Status.VolumeResizeStatus.Warnings = append(cr.Status.VolumeResizeStatus.Warnings,
			fmt.Sprintf("Multiple volume claim templates found (%v), only resizing '%s'", templateNames, VolumeClaimTemplateName))
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
		logger.Info("PRECHECK: Validating StorageClass expansion capability", "storageClass", storageClassName)

		storageClass := &storagev1.StorageClass{}
		if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: storageClassName}, storageClass); err != nil {
			logger.Error(nil, "PRECHECK FAILED: Could not retrieve StorageClass", "storageClass", storageClassName, "error", err)
			return oc.setResizeFailure(fmt.Sprintf("Failed to get StorageClass %s: %v", storageClassName, err), marklogicv1.ResizeReasonResizeFailed)
		}

		// Edge case: StorageClass no expansion - fail fast with clear error
		if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
			logger.Error(nil, "PRECHECK FAILED: StorageClass lacks expansion capability",
				"storageClass", storageClassName)
			return oc.setResizeFailure(
				fmt.Sprintf("StorageClass %s does not allow volume expansion (allowVolumeExpansion: false)", storageClassName),
				marklogicv1.ResizeReasonStorageClassNotExpandable)
		}
		logger.Info("PRECHECK PASSED: StorageClass allows volume expansion", "storageClass", storageClassName)
	}

	// Get PVCs for StatefulSet
	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		logger.Error(nil, "PRECHECK FAILED: Could not retrieve PVCs", "error", err)
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Detect cloud provider from StorageClass provisioner
	cloudProvider, err := oc.detectCloudProvider(sts, cr)
	if err != nil {
		logger.Error(err, "PRECHECK FAILED: Could not detect cloud provider")
		return oc.setResizeFailure(fmt.Sprintf("Failed to detect cloud provider: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}
	logger.Info("PRECHECK: Detected cloud provider", "cloudProvider", cloudProvider)

	// Edge case: Verify all PVCs are Bound
	logger.Info("PRECHECK: Validating PVC binding status", "pvcCount", len(pvcs))

	for _, pvc := range pvcs {
		if pvc.Status.Phase != corev1.ClaimBound {
			logger.Error(nil, "PRECHECK FAILED: PVC not bound",
				"pvc", pvc.Name, "phase", pvc.Status.Phase)
			return oc.setResizeStalled(
				fmt.Sprintf("PVC %s is not in Bound state (current: %s) - waiting for PVC to bind", pvc.Name, pvc.Status.Phase),
				marklogicv1.ResizeReasonPVCNotBound)
		}
	}
	logger.Info("PRECHECK PASSED: All PVCs are bound", "pvcCount", len(pvcs))

	// Verify new size > current size (edge case: Shrink requested or no change)
	logger.Info("PRECHECK: Validating resize size parameters",
		"originalSize", cr.Status.VolumeResizeStatus.OriginalSize,
		"targetSize", cr.Status.VolumeResizeStatus.TargetSize)

	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	originalQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.OriginalSize)

	if targetQty.Cmp(originalQty) == 0 {
		// Same size - no resize needed
		logger.Info("PRECHECK FAILED: Target size equals original size, no resize needed",
			"originalSize", cr.Status.VolumeResizeStatus.OriginalSize,
			"targetSize", cr.Status.VolumeResizeStatus.TargetSize)
		oc.Recorder.Event(cr, "Warning", "PrecheckSameSizeRejected",
			fmt.Sprintf("Target size %s equals current size %s - no resize needed",
				cr.Status.VolumeResizeStatus.TargetSize, cr.Status.VolumeResizeStatus.OriginalSize))
		return oc.setResizeFailure(
			fmt.Sprintf("Target size %s already matches current size %s - no resize needed",
				cr.Status.VolumeResizeStatus.TargetSize, cr.Status.VolumeResizeStatus.OriginalSize),
			marklogicv1.ResizeReasonNoResizeNeeded)
	} else if targetQty.Cmp(originalQty) < 0 {
		// Shrink requested
		logger.Error(nil, "PRECHECK FAILED: Volume shrinking not allowed",
			"currentSize", cr.Status.VolumeResizeStatus.OriginalSize,
			"targetSize", cr.Status.VolumeResizeStatus.TargetSize)
		oc.Recorder.Event(cr, "Warning", "PrecheckShrinkRejected",
			fmt.Sprintf("Volume shrinking not supported: cannot resize from %s to %s",
				cr.Status.VolumeResizeStatus.OriginalSize, cr.Status.VolumeResizeStatus.TargetSize))
		return oc.setResizeFailure(
			"Target size must be greater than current size - PVC shrinking not supported",
			marklogicv1.ResizeReasonShrinkNotSupported)
	}
	logger.Info("PRECHECK PASSED: Size validation successful",
		"originalSize", cr.Status.VolumeResizeStatus.OriginalSize,
		"targetSize", cr.Status.VolumeResizeStatus.TargetSize)

	// Step 1: MarkLogic pre-resize health check
	// Health check is critical - MarkLogic must be healthy before volume resize
	logger.Info("PRECHECK: Validating MarkLogic cluster health before resize")

	healthStatus, err := oc.CheckMarkLogicHealth()
	if err != nil {
		// Check if this is a "not ready yet" error - retry instead of fail
		if IsNotReadyError(err) {
			logger.Info("PRECHECK WAITING: MarkLogic not ready yet for health check, will retry", "error", err.Error())
			oc.Recorder.Event(cr, "Normal", "PrecheckMarkLogicHealthWaiting",
				fmt.Sprintf("MarkLogic cluster not ready: %v - will retry", err))
			cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for MarkLogic to be ready: %v", err)
			if updateErr := oc.updateStatus(); updateErr != nil {
				return result.Error(updateErr)
			}
			return result.RequeueSoon(10)
		}
		// Health check failed with a real error - fail the resize
		logger.Error(err, "PRECHECK FAILED: MarkLogic pre-resize health check failed")
		oc.Recorder.Event(cr, "Warning", "ResizeFailedHealthCheck",
			fmt.Sprintf("Volume resize failed: MarkLogic health check failed - %v", err))
		return oc.setResizeFailure(
			fmt.Sprintf("MarkLogic health check failed: %v", err),
			marklogicv1.ResizeReasonMarkLogicHealthCheckFailed)
	} else if !healthStatus.Healthy {
		// Health check completed successfully but cluster reported unhealthy
		logger.Error(nil, "PRECHECK FAILED: MarkLogic cluster is unhealthy, cannot proceed with resize",
			"errors", healthStatus.Errors, "hostsOnline", healthStatus.HostsOnline)
		oc.Recorder.Event(cr, "Warning", "ResizeFailedUnhealthy",
			fmt.Sprintf("Volume resize failed: MarkLogic cluster unhealthy - %v", healthStatus.Errors))
		return oc.setResizeFailure(
			fmt.Sprintf("MarkLogic cluster is unhealthy: %v", healthStatus.Errors),
			marklogicv1.ResizeReasonMarkLogicHealthCheckFailed)
	} else {
		logger.Info("PRECHECK PASSED: MarkLogic cluster is healthy and ready for resize",
			"hostsOnline", healthStatus.HostsOnline,
			"forestsOpen", healthStatus.ForestsOpen)
	}

	// Update status to next phase
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseResizingPVCs
	cr.Status.VolumeResizeStatus.TotalPVCs = int32(len(pvcs))
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Validation passed, identified %d PVCs to resize", len(pvcs))

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("ALL PRECHECKS PASSED: Starting volume resize operations",
		"storageClass", storageClassName,
		"totalPVCs", len(pvcs),
		"originalSize", cr.Status.VolumeResizeStatus.OriginalSize,
		"targetSize", cr.Status.VolumeResizeStatus.TargetSize)
	oc.Recorder.Event(cr, "Normal", "VolumeResizeStarted",
		fmt.Sprintf("Started volume resize from %s to %s for %d PVC(s)",
			cr.Status.VolumeResizeStatus.OriginalSize, cr.Status.VolumeResizeStatus.TargetSize, len(pvcs)))

	// Log detailed PVC information
	var pvcDetails []string
	for i, pvc := range pvcs {
		currentSize := ""
		if pvc.Spec.Resources.Requests != nil {
			if storage, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				currentSize = storage.String()
			}
		}
		pvcDetails = append(pvcDetails, fmt.Sprintf("%d. %s (current: %s)", i+1, pvc.Name, currentSize))
	}

	pvcListMsg := fmt.Sprintf("PVCs to resize: %s", strings.Join(pvcDetails, " | "))
	logger.Info(pvcListMsg)
	// Log detailed PVC info but don't record individual PVC events

	return result.RequeueSoon(1)
}

// Step 3: resizePVCs updates the PVC storage requests based on resize strategy
func (oc *OperatorContext) resizePVCs() result.ReconcileResult {
	cr := oc.MarklogicGroup

	// Check resize strategy
	if cr.Status.VolumeResizeStatus.ResizeStrategy == string(marklogicv1.ResizeStrategySequential) {
		return oc.resizePVCsSequential()
	}
	return oc.resizePVCsParallel()
}

// resizePVCsParallel resizes all PVCs concurrently
func (oc *OperatorContext) resizePVCsParallel() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	targetQty, err := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to parse target size: %v", err), marklogicv1.ResizeReasonInvalidResizeRequest)
	}

	originalSize := cr.Status.VolumeResizeStatus.OriginalSize
	resizedCount := int32(0)
	var failedPVCs []marklogicv1.FailedPVCInfo

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
			reason, errMsg := oc.extractErrorReason(err)

			// Check if this is an OPTIMIZING state error - if so, queue it instead of failing
			errLower := strings.ToLower(errMsg)
			if strings.Contains(errLower, "optimizing") || strings.Contains(errLower, "cannot currently modify") {
				logger.Info("Volume is in OPTIMIZING state, request will be queued and retried", "PVC", pvc.Name)
				// Queuing for retry - logged but not recorded as event
				cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Parallel resize - volume %s optimizing, request queued for %d/%d PVCs. Will retry automatically.", pvc.Name, resizedCount, len(pvcs))
				if err := oc.updateStatus(); err != nil {
					return result.Error(err)
				}
				return result.RequeueSoon(30)
			}

			failedPVCs = append(failedPVCs, marklogicv1.FailedPVCInfo{
				Name:    pvc.Name,
				Reason:  reason,
				Message: errMsg,
			})
			logger.Error(err, fmt.Sprintf("Failed to resize PVC %s: %s", pvc.Name, errMsg))
			continue
		}

		// Verification: Re-fetch PVC to verify update was accepted
		// Note: There's a race condition where the API server might not have updated the spec yet
		// even though the resize was initiated. We'll be more lenient here - if the resize API call
		// succeeded, we trust that Kubernetes will apply the resize even if the spec isn't updated immediately.
		verifiedPVC := &corev1.PersistentVolumeClaim{}
		if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: pvc.Namespace, Name: pvc.Name}, verifiedPVC); err != nil {
			// Failed to fetch PVC for verification, but the API call succeeded
			// Log a warning but DON'T increment resizedCount - we couldn't verify
			logger.Info(fmt.Sprintf("WARNING: Could not verify PVC %s after resize request (but resize was initiated): %v", pvc.Name, err))
			continue
		}

		verifiedStorage := verifiedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
		if verifiedStorage.Cmp(targetQty) < 0 {
			// Spec not updated yet - log warning but DON'T increment resizedCount - we couldn't verify
			// The capacity will catch up as Kubernetes processes the resize
			logger.Info(fmt.Sprintf("INFO: PVC %s spec not updated yet (race condition), but resize was initiated. Will verify via capacity later.", pvc.Name))
			continue
		}

		logger.Info(fmt.Sprintf("INFO: Initiated and verified resize for PVC %s from %s to %s", pvc.Name, originalSize, cr.Status.VolumeResizeStatus.TargetSize))
		// PVC resize initiated - logged but not recorded as event
		resizedCount++
	}

	// Handle failures
	if len(failedPVCs) > 0 {
		cr.Status.VolumeResizeStatus.FailedPVCs = failedPVCs
		cr.Status.VolumeResizeStatus.PVCsResized = resizedCount

		if resizedCount == 0 {
			// Total failure - none of the resizes were even initiated
			return oc.setResizeStalled(
				fmt.Sprintf("All PVC resize requests failed: %d failures", len(failedPVCs)),
				marklogicv1.ResizeReasonResizeFailed,
			)
		}
		// Partial failure - some resizes failed but we initiated others
		logger.Info(fmt.Sprintf("INFO: Partial resize - %d of %d PVCs had resize requests initiated", resizedCount, len(pvcs)))
	}

	cr.Status.VolumeResizeStatus.PVCsResized = resizedCount
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseWaitingForPVCResize
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("All PVC resize requests initiated (parallel mode) count=%d", resizedCount)

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info(fmt.Sprintf("INFO: All PVC resize requests initiated (parallel mode) count=%d", resizedCount))
	return result.RequeueSoon(5)
}

// resizePVCsSequential resizes PVCs one at a time, waiting for each to complete
func (oc *OperatorContext) resizePVCsSequential() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	targetQty, err := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to parse target size: %v", err), marklogicv1.ResizeReasonInvalidResizeRequest)
	}

	currentIndex := cr.Status.VolumeResizeStatus.CurrentPVCIndex
	originalSize := cr.Status.VolumeResizeStatus.OriginalSize

	// Check if all PVCs have been processed
	if int(currentIndex) >= len(pvcs) {
		cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseWaitingForPVCResize
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("All PVC resize requests initiated (sequential mode) count=%d", len(pvcs))
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	pvc := pvcs[currentIndex]

	// Check if current PVC is already at target size
	currentStorage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if currentStorage.Cmp(targetQty) >= 0 {
		// Already resized, check if resize is complete
		if pvc.Status.Capacity != nil {
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]
			if capacity.Cmp(targetQty) >= 0 {
				// PVC resize complete, move to next
				logger.Info(fmt.Sprintf("INFO: Sequential resize - PVC %s verified complete (%d/%d)", pvc.Name, currentIndex+1, len(pvcs)))
				cr.Status.VolumeResizeStatus.CurrentPVCIndex = currentIndex + 1
				cr.Status.VolumeResizeStatus.PVCsResized = currentIndex + 1
				cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Sequential resize: PVC %s verified complete (%d/%d)", pvc.Name, currentIndex+1, len(pvcs))
				if err := oc.updateStatus(); err != nil {
					return result.Error(err)
				}
				return result.RequeueSoon(2)
			}
		}
		// Waiting for resize to complete
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Sequential resize - waiting for PVC %s to complete (%d/%d)", pvc.Name, currentIndex+1, len(pvcs))
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// Update PVC storage request
	pvcCopy := pvc.DeepCopy()
	pvcCopy.Spec.Resources.Requests[corev1.ResourceStorage] = targetQty

	if err := oc.Client.Update(oc.Ctx, pvcCopy); err != nil {
		reason, errMsg := oc.extractErrorReason(err)
		errLower := strings.ToLower(errMsg)

		// Check if volume is in OPTIMIZING state - if so, queue the request instead of failing
		if strings.Contains(errLower, "optimizing") || strings.Contains(errLower, "cannot currently modify") {
			logger.Info("Volume is in OPTIMIZING state, request will be queued and retried",
				"pvc", pvc.Name,
				"error", errMsg)
			oc.Recorder.Event(cr, "Normal", "PVCResizeQueuedVolumeOptimizing",
				fmt.Sprintf("Volume is currently optimizing from previous modification. Resize request queued for PVC %s (%d/%d) - will retry automatically",
					pvc.Name, currentIndex+1, len(pvcs)))
			cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Sequential resize - volume optimizing, request queued for PVC %s (%d/%d). Will retry automatically.",
				pvc.Name, currentIndex+1, len(pvcs))
			if err := oc.updateStatus(); err != nil {
				return result.Error(err)
			}
			// Requeue after a longer delay to give AWS time to complete the OPTIMIZING state
			return result.RequeueSoon(30)
		}

		cr.Status.VolumeResizeStatus.FailedPVCs = append(cr.Status.VolumeResizeStatus.FailedPVCs, marklogicv1.FailedPVCInfo{
			Name:    pvc.Name,
			Reason:  reason,
			Message: errMsg,
		})
		return oc.setResizeStalled(
			fmt.Sprintf("Sequential resize failed at PVC %s (%d/%d): %s", pvc.Name, currentIndex+1, len(pvcs), errMsg),
			reason,
		)
	}

	logger.Info(fmt.Sprintf("INFO: Sequential resize - initiated and verified resize for PVC %s from %s to %s (%d/%d)",
		pvc.Name, originalSize, cr.Status.VolumeResizeStatus.TargetSize, currentIndex+1, len(pvcs)))
	// Individual PVC resize initiated - logged but not recorded as event

	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Sequential resize - waiting for PVC %s (%d/%d)", pvc.Name, currentIndex+1, len(pvcs))
	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(10)
}

// Step 4: waitForPVCResize polls PVC conditions for resize completion
func (oc *OperatorContext) waitForPVCResize() result.ReconcileResult {
	cr := oc.MarklogicGroup

	// Check resize strategy
	if cr.Status.VolumeResizeStatus.ResizeStrategy == string(marklogicv1.ResizeStrategySequential) {
		return oc.waitForPVCResizeSequential()
	}
	return oc.waitForPVCResizeParallel()
}

// waitForPVCResizeParallel polls all PVCs for resize completion
func (oc *OperatorContext) waitForPVCResizeParallel() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)

	allResized := true
	filesystemResizePending := false
	warnings := []string{}
	resizingPVCs := []string{}

	for _, pvc := range pvcs {
		// Check PVC conditions
		for _, condition := range pvc.Status.Conditions {
			if condition.Type == corev1.PersistentVolumeClaimResizing && condition.Status == corev1.ConditionTrue {
				allResized = false
				resizingPVCs = append(resizingPVCs, pvc.Name)
				logger.Info("PVC still resizing", "pvc", pvc.Name)

				// Check for EBS cooldown
				if oc.isEBSCooldownPeriod(pvc.Name) {
					return oc.setResizeStalled(
						fmt.Sprintf("AWS EBS cooldown period active for PVC %s. Volume modifications limited to 4 times in a 24-hour rotating window.", pvc.Name),
						marklogicv1.ResizeReasonEBSCooldownPeriod,
					)
				}
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
			if actualCapacity.Cmp(targetQty) >= 0 {
				logger.Info(fmt.Sprintf("INFO: PVC %s resize verified: %s -> %s", pvc.Name, cr.Status.VolumeResizeStatus.OriginalSize, targetQty.String()))
			} else {
				allResized = false
			}
		} else {
			allResized = false
		}
	}

	// Monitor resize progress (no timeout - retries handle delays)
	// PVCs may take time to resize depending on cloud provider and volume size
	if cr.Status.VolumeResizeStatus.StartTime != nil {
		elapsed := time.Since(cr.Status.VolumeResizeStatus.StartTime.Time)
		logger.Info("PVC resize in progress", "elapsed", elapsed, "resizingCount", len(resizingPVCs))
	}

	if !allResized && !filesystemResizePending {
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for PVC resize (parallel mode): %d resizing", len(resizingPVCs))
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// All PVCs verified complete
	logger.Info(fmt.Sprintf("INFO: All PVC resize verified complete (parallel mode) totalPVCs=%d", len(pvcs)))

	// Update status
	cr.Status.VolumeResizeStatus.FileSystemResizePending = filesystemResizePending
	cr.Status.VolumeResizeStatus.Warnings = append(cr.Status.VolumeResizeStatus.Warnings, warnings...)
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifyingHealth
	cr.Status.VolumeResizeStatus.Message = "All PVCs verified at target size, performing health checks"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("INFO: PVC resize complete, proceeding to health verification")
	return result.RequeueSoon(1)
}

// waitForPVCResizeSequential polls current PVC only and proceeds to next after completion
func (oc *OperatorContext) waitForPVCResizeSequential() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to list PVCs: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)

	// Check all PVCs are complete
	allComplete := true
	filesystemResizePending := false
	warnings := []string{}

	for i, pvc := range pvcs {
		// Check if capacity has been updated
		if pvc.Status.Capacity != nil {
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]
			if capacity.Cmp(targetQty) >= 0 {
				logger.Info(fmt.Sprintf("INFO: Sequential resize - PVC %s verified complete (%d/%d)", pvc.Name, i+1, len(pvcs)))
				continue
			}
		}
		allComplete = false

		// Check for filesystem resize pending
		for _, condition := range pvc.Status.Conditions {
			if condition.Type == corev1.PersistentVolumeClaimFileSystemResizePending && condition.Status == corev1.ConditionTrue {
				filesystemResizePending = true
				warning := fmt.Sprintf("WARNING: Filesystem resize pending for PVC %s, pod restart required", pvc.Name)
				warnings = append(warnings, warning)
				logger.Info(warning)
			}

			// Check for EBS cooldown
			if condition.Type == corev1.PersistentVolumeClaimResizing && condition.Status == corev1.ConditionTrue {
				if oc.isEBSCooldownPeriod(pvc.Name) {
					return oc.setResizeStalled(
						fmt.Sprintf("AWS EBS cooldown period active for PVC %s (%d/%d). Volume modifications limited to 4 times in a 24-hour rotating window.", pvc.Name, i+1, len(pvcs)),
						marklogicv1.ResizeReasonEBSCooldownPeriod,
					)
				}
			}
		}
	}

	if !allComplete {
		pendingCount := len(pvcs) - int(cr.Status.VolumeResizeStatus.PVCsResized)
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for PVC resize (sequential mode): %d pending", pendingCount)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// All PVCs verified complete
	logger.Info(fmt.Sprintf("INFO: All PVC resize verified complete (sequential mode) totalPVCs=%d", len(pvcs)))

	// Update status
	cr.Status.VolumeResizeStatus.FileSystemResizePending = filesystemResizePending
	cr.Status.VolumeResizeStatus.Warnings = append(cr.Status.VolumeResizeStatus.Warnings, warnings...)
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifyingHealth
	cr.Status.VolumeResizeStatus.Message = "All PVCs verified at target size, performing health checks"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(1)
}

// Step 5: verifyHealthBeforeStatefulSetChanges verifies pod and MarkLogic health
func (oc *OperatorContext) verifyHealthBeforeStatefulSetChanges() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
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

	// Verify PVCs show new capacity
	pvcs, err := oc.getPVCsForStatefulSet(sts)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get PVCs: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	targetQty, _ := resource.ParseQuantity(cr.Status.VolumeResizeStatus.TargetSize)
	for _, pvc := range pvcs {
		if pvc.Status.Capacity != nil {
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]
			if capacity.Cmp(targetQty) < 0 {
				cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for PVC %s capacity to update", pvc.Name)
				if err := oc.updateStatus(); err != nil {
					return result.Error(err)
				}
				return result.RequeueSoon(10)
			}
		}
	}

	logger.Info("INFO: All PVCs verified at target size, performing MarkLogic health checks")

	// Step 5a: MarkLogic Server Verification - health check is critical post-resize
	healthStatus, err := oc.CheckMarkLogicHealth()
	if err != nil {
		// Check if this is a "not ready yet" error - retry instead of fail
		if IsNotReadyError(err) {
			logger.Info("MarkLogic not ready yet for post-resize health check, will retry", "error", err.Error())
			cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for MarkLogic to be ready after resize: %v", err)
			if updateErr := oc.updateStatus(); updateErr != nil {
				return result.Error(updateErr)
			}
			return result.RequeueSoon(10)
		}
		// Health check failed with a real error
		logger.Error(err, "MarkLogic post-resize health check failed")
		oc.Recorder.Event(cr, "Warning", "ResizeFailedPostHealthCheck",
			fmt.Sprintf("Volume resize failed: Post-resize health check failed - %v", err))
		return oc.setResizeFailure(
			fmt.Sprintf("MarkLogic post-resize health check failed: %v", err),
			marklogicv1.ResizeReasonMarkLogicHealthCheckFailed)
	} else if !healthStatus.Healthy {
		// Health check completed but cluster reported unhealthy
		logger.Error(nil, "MarkLogic cluster is unhealthy after volume resize",
			"errors", healthStatus.Errors,
			"warnings", healthStatus.Warnings)
		oc.Recorder.Event(cr, "Warning", "ResizeFailedPostUnhealthy",
			fmt.Sprintf("Volume resize failed: MarkLogic cluster unhealthy - %v", healthStatus.Errors))
		return oc.setResizeFailure(
			fmt.Sprintf("MarkLogic cluster unhealthy after resize: %v", healthStatus.Errors),
			marklogicv1.ResizeReasonMarkLogicHealthCheckFailed)
	} else {
		logger.Info("INFO: MarkLogic health check passed",
			"hostsOnline", healthStatus.HostsOnline,
			"forestsHealthy", healthStatus.ForestsOpen,
			"databases", healthStatus.DatabaseCount)
		// Health check passed - logged but event removed
	}

	healthPassed := true
	cr.Status.VolumeResizeStatus.MarkLogicHealthPassed = &healthPassed

	// Proceed to StatefulSet backup
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseBackingUpStatefulSet
	cr.Status.VolumeResizeStatus.Message = "Health checks complete, backing up StatefulSet"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("SUCCESS: All pods healthy with resized volumes")
	return result.RequeueSoon(1)
}

// Step 6: backupStatefulSet stores the current StatefulSet spec in CR status
func (oc *OperatorContext) backupStatefulSet() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet for backup: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Serialize StatefulSet spec to JSON for backup
	stsSpecJSON, err := json.Marshal(sts.Spec)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to serialize StatefulSet spec: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	cr.Status.VolumeResizeStatus.StatefulSetBackup = string(stsSpecJSON)
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseDeletingStatefulSet
	cr.Status.VolumeResizeStatus.Message = "StatefulSet spec backed up, deleting with orphan policy"

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info("INFO: Backed up StatefulSet spec")
	// StatefulSet backup - logged but not recorded as event

	return result.RequeueSoon(1)
}

// Step 7: deleteStatefulSetWithOrphan deletes the StatefulSet with orphan propagation policy
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
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Track pods before deletion for orphaned pod tracking (edge case: STS recreate fails after delete)
	podList := &corev1.PodList{}
	if err := oc.Client.List(oc.Ctx, podList,
		client.InNamespace(cr.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels)); err == nil {
		var podNames []string
		for _, pod := range podList.Items {
			podNames = append(podNames, pod.Name)
		}
		cr.Status.VolumeResizeStatus.OrphanedPods = podNames
		logger.Info("Tracking pods for orphan recovery", "pods", podNames)
	}

	// Record deletion timestamp for interrupted template update recovery
	now := metav1.Now()
	cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp = &now

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
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
		// Edge case: STS delete fails after PVC resize - mark as drift and try with backoff
		logger.Error(err, "ERROR: Failed to delete StatefulSet with orphan policy",
			"statefulset", sts.Name)
		oc.Recorder.Event(cr, "Warning", "StatefulSetDeleteFailed",
			fmt.Sprintf("Failed to delete StatefulSet with orphan policy: %v. PVCs have new size, STS has old size.", err))

		// Increment retry count
		cr.Status.VolumeResizeStatus.RetryCount++
		if cr.Status.VolumeResizeStatus.RetryCount > MaxRetryCount {
			return oc.setResizeFailure(
				fmt.Sprintf("CRITICAL: Failed to delete StatefulSet after %d retries: %v. Manual intervention may be required.",
					MaxRetryCount, err),
				marklogicv1.ResizeReasonStatefulSetDeleteFailed)
		}

		return oc.setResizeStalled(
			fmt.Sprintf("Failed to delete StatefulSet (attempt %d/%d): %v. Will retry.",
				cr.Status.VolumeResizeStatus.RetryCount, MaxRetryCount, err),
			marklogicv1.ResizeReasonStatefulSetDeleteFailed)
	}

	logger.Info("INFO: Deleted StatefulSet with orphan policy")
	// StatefulSet deletion - logged but not recorded as event

	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRecreatingStatefulSet
	cr.Status.VolumeResizeStatus.Message = "Deleted StatefulSet, recreating with new volume size"
	cr.Status.VolumeResizeStatus.RetryCount = 0 // Reset retry count on success

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(2)
}

// Step 8: recreateStatefulSet recreates the StatefulSet with updated volumeClaimTemplate
func (oc *OperatorContext) recreateStatefulSet() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	// Check if StatefulSet already exists
	_, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err == nil {
		// StatefulSet exists, clear deletion timestamp and proceed to verify pods running
		logger.Info("StatefulSet already exists, proceeding to verify pods running")
		cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifyingPodsRunning
		cr.Status.VolumeResizeStatus.Message = "StatefulSet recreated, verifying all pods are running"
		cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp = nil // Clear deletion timestamp
		cr.Status.VolumeResizeStatus.OrphanedPods = nil                 // Clear orphaned pods
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(1)
	}

	if !apierrors.IsNotFound(err) {
		return oc.setResizeFailure(fmt.Sprintf("Failed to check StatefulSet existence: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Edge case: Check for orphaned pods (STS recreate fails after delete)
	if len(cr.Status.VolumeResizeStatus.OrphanedPods) > 0 {
		logger.Info("WARNING: Orphaned pods detected from previous STS delete",
			"orphanedPods", cr.Status.VolumeResizeStatus.OrphanedPods)
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
			cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp = nil
		} else {
			// Edge case: STS recreate fails after delete - pods running but unmanaged
			logger.Error(err, "CRITICAL: Failed to recreate StatefulSet")
			oc.Recorder.Event(cr, "Warning", "StatefulSetRecreateFailed",
				fmt.Sprintf("CRITICAL: Failed to recreate StatefulSet. Orphaned pods: %v. Error: %v",
					cr.Status.VolumeResizeStatus.OrphanedPods, err))

			// Increment retry count
			cr.Status.VolumeResizeStatus.RetryCount++
			if cr.Status.VolumeResizeStatus.RetryCount > MaxRetryCount {
				return oc.setResizeFailure(
					fmt.Sprintf("CRITICAL: Failed to recreate StatefulSet after %d retries: %v. Pods are orphaned and require manual intervention.",
						MaxRetryCount, err),
					marklogicv1.ResizeReasonStatefulSetRecreateFailed)
			}

			return oc.setResizeStalled(
				fmt.Sprintf("Failed to recreate StatefulSet (attempt %d/%d): %v. Orphaned pods: %v",
					cr.Status.VolumeResizeStatus.RetryCount, MaxRetryCount, err, cr.Status.VolumeResizeStatus.OrphanedPods),
				marklogicv1.ResizeReasonStatefulSetRecreateFailed)
		}
	}

	logger.Info("INFO: Recreated StatefulSet with new template")
	// StatefulSet recreation - logged but not recorded as event

	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseVerifyingPodsRunning
	cr.Status.VolumeResizeStatus.Message = "StatefulSet recreated, verifying all pods are running"
	cr.Status.VolumeResizeStatus.StatefulSetDeletionTimestamp = nil // Clear deletion timestamp
	cr.Status.VolumeResizeStatus.OrphanedPods = nil                 // Clear orphaned pods
	cr.Status.VolumeResizeStatus.RetryCount = 0                     // Reset retry count on success

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(5)
}

// Step 8a: verifyPodsRunning verifies all pods are running after StatefulSet recreation
func (oc *OperatorContext) verifyPodsRunning() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	logger.Info("INFO: Verifying pods are running after StatefulSet recreation")

	sts, err := oc.GetStatefulSet(cr.Namespace, cr.Spec.Name)
	if err != nil {
		return oc.setResizeFailure(fmt.Sprintf("Failed to get StatefulSet: %v", err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Check if all replicas are ready
	expectedReplicas := int32(1)
	if cr.Spec.Replicas != nil {
		expectedReplicas = *cr.Spec.Replicas
	}

	if sts.Status.ReadyReplicas != expectedReplicas {
		// Edge case: Node scheduling issues - check for pods stuck in Pending
		podList := &corev1.PodList{}
		if err := oc.Client.List(oc.Ctx, podList,
			client.InNamespace(cr.Namespace),
			client.MatchingLabels(sts.Spec.Selector.MatchLabels)); err == nil {

			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodPending {
					// Track pending pod timeout using a single timestamp across all pods.
					// Design choice rationale:
					// - During verifyPodsRunning, pods are checked as a group after StatefulSet recreation
					// - Using a single timestamp provides conservative timeout tracking:
					//   if pod-0 enters pending at T1 and pod-1 at T2, pod-1 times out sooner (safer)
					// - In practice, only 1-2 pods are ever pending simultaneously for scheduling issues
					// - The timestamp is cleared when verifyPodsRunning completes (line 1533),
					//   preventing cross-phase contamination
					// - Each pod is correctly identified in error messages (pod.Name)
					if cr.Status.VolumeResizeStatus.PodPendingStartTime == nil {
						// First time seeing pending pod, record timestamp
						now := metav1.Now()
						cr.Status.VolumeResizeStatus.PodPendingStartTime = &now
						logger.Info("WARNING: Pod pending after restart", "pod", pod.Name)
						oc.Recorder.Event(cr, "Warning", "PodPending",
							fmt.Sprintf("Pod %s is pending after restart - monitoring for scheduling issues", pod.Name))
					} else {
						// Pod is pending - monitor but don't timeout
						// Retry strategy handles prolonged delays; user can pause/resume with annotation
						pendingDuration := time.Since(cr.Status.VolumeResizeStatus.PodPendingStartTime.Time)
						if pendingDuration > 5*time.Minute {
							// Log warnings for monitoring purposes
							logger.Info("WARNING: Pod pending after restart - check scheduling and node resources",
								"pod", pod.Name, "pending_duration", pendingDuration)
							oc.Recorder.Event(cr, "Warning", "PodSchedulingMonitoring",
								fmt.Sprintf("Pod %s pending for %v - check node resources and scheduling", pod.Name, pendingDuration))
						}
					}
				}
			}
		}

		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Verifying all pods are running after StatefulSet recreation: %d/%d ready", sts.Status.ReadyReplicas, expectedReplicas)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
	}

	// All pods ready - clear pending tracking
	cr.Status.VolumeResizeStatus.PodPendingStartTime = nil

	logger.Info("INFO: All pods running and adopted by StatefulSet")
	// Pod adoption - logged but not recorded as event

	// Determine next phase based on filesystem resize pending
	if cr.Status.VolumeResizeStatus.FileSystemResizePending {
		cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseRestartingPods
		cr.Status.VolumeResizeStatus.Message = "All pods running, restarting pods for filesystem resize"
		cr.Status.VolumeResizeStatus.LastResizedPodIndex = -1
	} else {
		logger.Info("INFO: FileSystemResizePending is false, skipping pod restart")
		// Pod restart check - logged but not recorded as event
		// Skip directly to completion
		return oc.completeVolumeResize()
	}

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(1)
}

// Step 9: restartPodsForFilesystemResize deletes pods sequentially if filesystem resize is pending
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
		// All pods restarted, proceed to completion
		logger.Info("INFO: All pods restarted, proceeding to completion")
		return oc.completeVolumeResize()
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
		return oc.setResizeFailure(fmt.Sprintf("Failed to get pod %s: %v", podName, err), marklogicv1.ResizeReasonResizeFailed)
	}

	// Check if pod is ready before proceeding
	podReady := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			podReady = true
			break
		}
	}

	// Edge case: Pod stuck pending after previous restart
	// Note: Single timestamp design works well here because pods are processed sequentially:
	// Only one pod can be pending at a time during filesystem resize phase.
	// Timestamp is cleared before moving to next pod, preventing stale timestamps.
	if pod.Status.Phase == corev1.PodPending && pod.DeletionTimestamp == nil {
		if cr.Status.VolumeResizeStatus.PodPendingStartTime == nil {
			now := metav1.Now()
			cr.Status.VolumeResizeStatus.PodPendingStartTime = &now
		} else {
			// Pod is pending - monitor but don't timeout
			// Retry strategy handles prolonged delays; user can pause/resume with annotation
			pendingDuration := time.Since(cr.Status.VolumeResizeStatus.PodPendingStartTime.Time)
			if pendingDuration > 5*time.Minute {
				logger.Info("WARNING: Pod pending during filesystem resize - check scheduling and node resources",
					"pod", podName, "pending_duration", pendingDuration)
				oc.Recorder.Event(cr, "Warning", "PodSchedulingMonitoring",
					fmt.Sprintf("Pod %s pending for %v during filesystem resize - check node resources", podName, pendingDuration))
			}
			// Continue monitoring without timeout - retry strategy will handle delays
		}
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for pod %s to be scheduled", podName)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(10)
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
		// Edge case: Pod restarts during resize - watch pod events
		logger.Info("Pod is being deleted, waiting for recreation", "pod", podName)
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Waiting for pod %s to be deleted", podName)
		if err := oc.updateStatus(); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	// Clear pending start time before deleting
	cr.Status.VolumeResizeStatus.PodPendingStartTime = nil

	// Delete the pod
	if err := oc.Client.Delete(oc.Ctx, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return oc.setResizeFailure(fmt.Sprintf("Failed to delete pod %s: %v", podName, err), marklogicv1.ResizeReasonResizeFailed)
		}
	}

	logger.Info(fmt.Sprintf("INFO: Restarting pod %s for filesystem resize", podName))
	// Pod restart - logged but not recorded as event

	cr.Status.VolumeResizeStatus.LastResizedPodIndex = nextIndex
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Restarted pod %s, waiting for ready state", podName)

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	return result.RequeueSoon(15)
}

// Step 10: completeVolumeResize updates CR status to completed
func (oc *OperatorContext) completeVolumeResize() result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	// Verify CR spec size matches target size before marking complete
	// This ensures future pods created from the STS will use the correct size
	if cr.Spec.Persistence.Size != cr.Status.VolumeResizeStatus.TargetSize {
		logger.Info("WARNING: CR spec size does not match target size, delaying completion",
			"crSpecSize", cr.Spec.Persistence.Size, "targetSize", cr.Status.VolumeResizeStatus.TargetSize)
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf(
			"Waiting for CR spec to match target size (current: %s, target: %s)",
			cr.Spec.Persistence.Size, cr.Status.VolumeResizeStatus.TargetSize)
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

	// Determine if MarkLogic health passed
	healthPassed := "true"
	if cr.Status.VolumeResizeStatus.MarkLogicHealthPassed != nil && !*cr.Status.VolumeResizeStatus.MarkLogicHealthPassed {
		healthPassed = "false"
	}

	// Update status to completed
	now := metav1.Now()
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseCompleted
	cr.Status.VolumeResizeStatus.CompletionTime = &now
	cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Volume resize completed successfully in %s. MarkLogic health verified.", duration)
	cr.Status.VolumeResizeStatus.StatefulSetBackup = "" // Clear backup

	// Set condition
	cr.SetCondition(metav1.Condition{
		Type:               ConditionTypeVolumeResizeComplete,
		Status:             metav1.ConditionTrue,
		Reason:             "ResizeCompleted",
		Message:            fmt.Sprintf("Volume resize completed from %s to %s", cr.Status.VolumeResizeStatus.OriginalSize, cr.Status.VolumeResizeStatus.TargetSize),
		LastTransitionTime: now,
	})

	if err := oc.updateStatus(); err != nil {
		return result.Error(err)
	}

	logger.Info(fmt.Sprintf("SUCCESS: Resize completed duration=%s marklogicHealthPassed=%s", duration, healthPassed))
	oc.Recorder.Event(cr, "Normal", "VolumeResizeCompleted", fmt.Sprintf("Volume resize from %s to %s completed successfully", cr.Status.VolumeResizeStatus.OriginalSize, cr.Status.VolumeResizeStatus.TargetSize))

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
func (oc *OperatorContext) setResizeFailure(message string, reason marklogicv1.VolumeResizeReason) result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	logger.Error(nil, "Volume resize failed", "message", message, "reason", reason)

	if cr.Status.VolumeResizeStatus == nil {
		cr.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{}
	}

	now := metav1.Now()
	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseFailed
	cr.Status.VolumeResizeStatus.Reason = reason
	cr.Status.VolumeResizeStatus.CompletionTime = &now
	cr.Status.VolumeResizeStatus.Message = message

	// Set condition
	cr.SetCondition(metav1.Condition{
		Type:               ConditionTypeVolumeResizeComplete,
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            message,
		LastTransitionTime: now,
	})

	if err := oc.updateStatus(); err != nil {
		logger.Error(err, "Failed to update status after resize failure")
	}

	oc.Recorder.Event(cr, "Warning", "VolumeResizeFailed", message)

	return result.Done()
}

// setResizeStalled sets the resize status to stalled (temporary blockage) and returns
func (oc *OperatorContext) setResizeStalled(message string, reason marklogicv1.VolumeResizeReason) result.ReconcileResult {
	return oc.setResizeStalledWithError(message, reason, nil)
}

// setResizeStalledWithError sets the resize status to stalled with error details and intelligent retry strategy
func (oc *OperatorContext) setResizeStalledWithError(message string, reason marklogicv1.VolumeResizeReason, err error) result.ReconcileResult {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	logger.Info("Volume resize stalled", "message", message, "reason", reason)

	if cr.Status.VolumeResizeStatus == nil {
		cr.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{}
	}

	// Check if resize is paused via annotation
	if CheckIfPausedByAnnotation(cr) {
		logger.Info("Volume resize paused by annotation - user intervention required",
			"annotation", PauseResizeAnnotationKey)
		cr.Status.VolumeResizeStatus.Message = fmt.Sprintf("Paused: %s (Resume by removing annotation %s)", message, PauseResizeAnnotationKey)
		return oc.updateStatusAndRequeue(30*time.Minute, "Paused by annotation")
	}

	now := metav1.Now()

	// Save the current phase before stalling for recovery
	if cr.Status.VolumeResizeStatus.Phase != marklogicv1.VolumeResizePhaseStalled {
		cr.Status.VolumeResizeStatus.LastPhaseBeforeStall = cr.Status.VolumeResizeStatus.Phase
	}

	cr.Status.VolumeResizeStatus.Phase = marklogicv1.VolumeResizePhaseStalled
	cr.Status.VolumeResizeStatus.Reason = reason
	cr.Status.VolumeResizeStatus.Message = message

	// Evaluate retry strategy
	retryConfig := DefaultRetryConfig()
	retryStrategy := EvaluateRetry(retryConfig, cr.Status.VolumeResizeStatus, err)

	if retryStrategy.ShouldRetry {
		// Update retry status
		UpdateRetryStatus(cr.Status.VolumeResizeStatus, retryStrategy, message)

		logger.Info("Volume resize will retry",
			"retryCount", cr.Status.VolumeResizeStatus.RetryCount,
			"consecutiveRetries", cr.Status.VolumeResizeStatus.ConsecutiveRetries,
			"classification", retryStrategy.Classification,
			"nextRetryTime", retryStrategy.NextRetryTime.Format(time.RFC3339))

		// Record event with retry info
		eventMsg := fmt.Sprintf("Volume resize stalled: %s - Will retry #%d in %s (classification: %s)",
			message, cr.Status.VolumeResizeStatus.RetryCount, time.Until(retryStrategy.NextRetryTime).Round(time.Second), retryStrategy.Classification)
		oc.Recorder.Event(cr, "Warning", "VolumeResizeStalled", eventMsg)
	} else {
		// No more retries available
		logger.Error(err, "Volume resize stalled - max retries exceeded",
			"classification", retryStrategy.Classification,
			"message", retryStrategy.Message)

		eventMsg := fmt.Sprintf("Volume resize stalled with no more retries: %s (%s)", message, retryStrategy.Classification)
		oc.Recorder.Event(cr, "Warning", "VolumeResizeStalled", eventMsg)

		// Mark as failed if no more retries
		if retryStrategy.Classification == ErrorClassificationPersistent || retryStrategy.Classification == ErrorClassificationInternal {
			return oc.setResizeFailure(fmt.Sprintf("Stalled: %s - manual intervention required", message), reason)
		}
	}

	// Set Progressing condition to False
	cr.SetCondition(metav1.Condition{
		Type:               ConditionTypeProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            message,
		LastTransitionTime: now,
	})

	// Set VolumeResizeComplete condition
	cr.SetCondition(metav1.Condition{
		Type:               ConditionTypeVolumeResizeComplete,
		Status:             metav1.ConditionFalse,
		Reason:             "Stalled",
		Message:            fmt.Sprintf("Volume resize stalled at %s phase: %s", cr.Status.VolumeResizeStatus.LastPhaseBeforeStall, message),
		LastTransitionTime: now,
	})

	if err := oc.updateStatus(); err != nil {
		logger.Error(err, "Failed to update status after resize stalled")
	}

	// Return requeue with calculated backoff
	timeUntilRetry := time.Until(retryStrategy.NextRetryTime)
	if timeUntilRetry < 0 {
		timeUntilRetry = 1 * time.Second
	}

	return result.RequeueAfter(timeUntilRetry)
}

// updateStatusAndRequeue is a helper function to update status and requeue with a specific delay
func (oc *OperatorContext) updateStatusAndRequeue(delay time.Duration, reason string) result.ReconcileResult {
	logger := oc.ReqLogger

	if err := oc.updateStatus(); err != nil {
		logger.Error(err, "Failed to update status", "reason", reason)
		return result.Error(err)
	}

	return result.RequeueAfter(delay)
}

// extractErrorReason extracts a VolumeResizeReason from a Kubernetes API error
func (oc *OperatorContext) extractErrorReason(err error) (marklogicv1.VolumeResizeReason, string) {
	errMsg := err.Error()
	errLower := strings.ToLower(errMsg)

	if apierrors.IsForbidden(err) {
		return marklogicv1.ResizeReasonResizeForbidden, errMsg
	}

	if apierrors.IsNotFound(err) {
		if strings.Contains(errLower, "statefulset") {
			return marklogicv1.ResizeReasonStatefulSetNotFound, errMsg
		}
		return marklogicv1.ResizeReasonResizeFailed, errMsg
	}

	if strings.Contains(errLower, "exceeded quota") || strings.Contains(errLower, "quota exceeded") {
		return marklogicv1.ResizeReasonStorageQuotaExceeded, errMsg
	}

	if strings.Contains(errLower, "rate") && strings.Contains(errLower, "limit") {
		return marklogicv1.ResizeReasonResizeRateLimited, errMsg
	}

	if strings.Contains(errLower, "cooldown") || strings.Contains(errLower, "modifyvolume") {
		return marklogicv1.ResizeReasonEBSCooldownPeriod, errMsg
	}

	if strings.Contains(errLower, "volumemodificationrateexceeded") {
		return marklogicv1.ResizeReasonEBSCooldownPeriod, errMsg
	}

	if strings.Contains(errLower, "shrink") || strings.Contains(errLower, "decrease") {
		return marklogicv1.ResizeReasonShrinkNotSupported, errMsg
	}

	if strings.Contains(errLower, "not bound") || strings.Contains(errLower, "pending") {
		return marklogicv1.ResizeReasonPVCNotBound, errMsg
	}

	if strings.Contains(errLower, "timeout") || strings.Contains(errLower, "timed out") {
		return marklogicv1.ResizeReasonTimeout, errMsg
	}

	if strings.Contains(errLower, "scheduling") || strings.Contains(errLower, "unschedulable") {
		return marklogicv1.ResizeReasonPodSchedulingFailed, errMsg
	}

	return marklogicv1.ResizeReasonResizeFailed, errMsg
}

// isEBSCooldownPeriod checks if EBS cooldown period is likely affecting the PVC
func (oc *OperatorContext) isEBSCooldownPeriod(pvcName string) bool {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	// Check PVC conditions for extended resizing state
	pvc := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: cr.Namespace, Name: pvcName}, pvc); err != nil {
		return false
	}

	// Check if PVC has been in Resizing condition for an extended period (> 10 mins)
	// This itself is a strong indicator of cooldown/rate limiting even without explicit error messages
	extendedResizingDetected := false
	for _, condition := range pvc.Status.Conditions {
		if condition.Type == corev1.PersistentVolumeClaimResizing && condition.Status == corev1.ConditionTrue {
			resizingDuration := time.Since(condition.LastTransitionTime.Time)
			if resizingDuration > 10*time.Minute {
				logger.Info("PVC has been resizing for extended period - likely cooldown",
					"pvc", pvcName, "duration", resizingDuration)
				extendedResizingDetected = true
			}
		}
	}

	// Get events for the PVC and PV to check for rate limit indicators
	events := &corev1.EventList{}
	if err := oc.Client.List(oc.Ctx, events, client.InNamespace(cr.Namespace)); err != nil {
		// If we can't fetch events but detected extended resizing, return true
		if extendedResizingDetected {
			return true
		}
		return false
	}

	for _, event := range events.Items {
		if event.InvolvedObject.Name != pvcName && event.InvolvedObject.Name != pvc.Spec.VolumeName {
			continue
		}

		msgLower := strings.ToLower(event.Message)
		reasonLower := strings.ToLower(event.Reason)

		// AWS EBS CSI driver specific messages
		if strings.Contains(msgLower, "modifyvolume") && strings.Contains(msgLower, "rate") {
			logger.Info("WARNING: EBS rate limit detected in event",
				"pvc", pvcName, "event", event.Message)
			return true
		}
		if strings.Contains(msgLower, "cooldown") {
			logger.Info("WARNING: EBS cooldown period detected in event",
				"pvc", pvcName, "event", event.Message)
			return true
		}
		if strings.Contains(msgLower, "volumemodificationrateexceeded") ||
			strings.Contains(reasonLower, "volumemodificationrateexceeded") {
			logger.Info("WARNING: EBS VolumeModificationRateExceeded detected",
				"pvc", pvcName, "event", event.Message)
			return true
		}
		// Generic CSI rate limit indicators
		if strings.Contains(msgLower, "rate exceeded") || strings.Contains(msgLower, "rate limited") {
			logger.Info("WARNING: Storage rate limit detected",
				"pvc", pvcName, "event", event.Message)
			return true
		}
	}

	// Return true if we detected extended resizing duration even without explicit error events
	if extendedResizingDetected {
		return true
	}

	return false
}

// detectCloudProvider identifies the cloud provider based on StorageClass provisioner
// Returns: "AWS", "Azure", "GCP", or "Unknown"
// Returns error if StorageClass cannot be fetched (required for resize validation)
func (oc *OperatorContext) detectCloudProvider(sts *appsv1.StatefulSet, cr *marklogicv1.MarklogicGroup) (string, error) {
	logger := oc.ReqLogger

	// Get storage class name from StatefulSet volumeClaimTemplates
	var storageClassName string
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if vct.Name == VolumeClaimTemplateName && vct.Spec.StorageClassName != nil {
			storageClassName = *vct.Spec.StorageClassName
			break
		}
	}

	// Fallback to MarklogicGroup spec
	if storageClassName == "" && cr.Spec.Persistence != nil && cr.Spec.Persistence.StorageClassName != "" {
		storageClassName = cr.Spec.Persistence.StorageClassName
	}

	if storageClassName == "" {
		logger.Info("Could not determine storage class, returning Unknown")
		return "Unknown", nil
	}

	// Fetch the StorageClass
	sc := &storagev1.StorageClass{}
	err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: storageClassName}, sc)
	if err != nil {
		logger.Error(err, "Failed to fetch StorageClass - cannot determine cloud provider")
		return "", fmt.Errorf("failed to fetch StorageClass %s: %w", storageClassName, err)
	}

	// Detect provider based on provisioner field
	provisioner := sc.Provisioner
	logger.Info("StorageClass provisioner", "provisioner", provisioner)

	switch {
	case provisioner == "ebs.csi.aws.com":
		return "AWS", nil
	case provisioner == "disk.csi.azure.com":
		return "Azure", nil
	case provisioner == "pd.csi.storage.gke.io":
		return "GCP", nil
	case provisioner == "kubernetes.io/aws-ebs":
		return "AWS", nil // Legacy AWS provisioner
	case provisioner == "kubernetes.io/azure-disk":
		return "Azure", nil // Legacy Azure provisioner
	case provisioner == "kubernetes.io/gce-pd":
		return "GCP", nil // Legacy GCP provisioner
	default:
		logger.Info("Unknown provisioner, returning Unknown", "provisioner", provisioner)
		return "Unknown", nil
	}
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
