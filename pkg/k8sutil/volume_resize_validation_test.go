package k8sutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResizeValidationSuccessInitializesStatus(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete this reconcile step")
	}
	output, err := res.Output()
	if err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}
	if !output.Requeue {
		t.Fatalf("expected requeue after validation success")
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be initialized")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseResizingPVCs {
		t.Fatalf("expected phase ResizingPVCs, got %s", status.Phase)
	}
	if status.CurrentSize != "20Gi" {
		t.Fatalf("expected currentSize 20Gi, got %s", status.CurrentSize)
	}
	if status.TargetSize != "50Gi" {
		t.Fatalf("expected targetSize 50Gi, got %s", status.TargetSize)
	}
	if status.TotalPVCs != 2 {
		t.Fatalf("expected totalPvcs 2, got %d", status.TotalPVCs)
	}
	if status.OperationID == "" {
		t.Fatalf("expected non-empty operationID")
	}
	if status.FirstStartedTime == nil {
		t.Fatalf("expected firstStartedTime to be set")
	}
	if len(status.PVCStatuses) != 2 {
		t.Fatalf("expected two pvcStatuses entries, got %d", len(status.PVCStatuses))
	}
}

func TestResizeValidationShrinkFails(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "10Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete this reconcile step")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseFailed {
		t.Fatalf("expected phase Failed, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonShrinkNotSupported {
		t.Fatalf("expected reason ShrinkNotSupported, got %s", status.Reason)
	}
}

func TestResizeValidationRequiresOnDelete(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.RollingUpdateStatefulSetStrategyType})
	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete this reconcile step")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseFailed {
		t.Fatalf("expected phase Failed, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonInvalidResizeRequest {
		t.Fatalf("expected reason InvalidResizeRequest, got %s", status.Reason)
	}
}

func TestActiveOperationDefersNewerTarget(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	now := metav1.Now()
	oc.MarklogicGroup.Status.VolumeResizeStatus = &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-active",
		ObservedGeneration: 1,
		Phase:              marklogicv1.VolumeResizePhaseValidating,
		CurrentSize:        "20Gi",
		TargetSize:         "30Gi",
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
	}
	if err := oc.Client.Status().Update(oc.Ctx, oc.MarklogicGroup); err != nil {
		t.Fatalf("failed to seed active status: %v", err)
	}

	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected active operation fencing to complete")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.TargetSize != "30Gi" {
		t.Fatalf("expected active target to remain 30Gi, got %s", status.TargetSize)
	}
	if status.DeferredTargetSize != "50Gi" {
		t.Fatalf("expected deferredTargetSize 50Gi, got %s", status.DeferredTargetSize)
	}
	if status.DeferredObservedGeneration != updated.Generation {
		t.Fatalf("expected deferredObservedGeneration %d, got %d", updated.Generation, status.DeferredObservedGeneration)
	}
}

func TestPausedAnnotationStallsResize(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	oc.MarklogicGroup.Annotations = map[string]string{resizePauseAnnotationKey: "true"}
	if err := oc.Client.Update(oc.Ctx, oc.MarklogicGroup); err != nil {
		t.Fatalf("failed to seed paused annotation: %v", err)
	}

	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected paused validation to complete")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseStalled {
		t.Fatalf("expected phase Stalled, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonPaused {
		t.Fatalf("expected reason Paused, got %s", status.Reason)
	}
}

func TestNoResizeRequestedContinuesNormalFlow(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "20Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	res := oc.ReconcileVolumeResizeValidation()
	if res.Completed() {
		t.Fatalf("expected no-resize case to continue normal reconcile flow")
	}

	updated := getUpdatedGroup(t, oc)
	if updated.Status.VolumeResizeStatus != nil {
		t.Fatalf("expected volumeResizeStatus to remain unset when no resize is requested")
	}
}

func TestResizeValidationMissingPVCStalls(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})

	missing := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "datadir-dnode-1", Namespace: "testns"}}
	if err := oc.Client.Delete(oc.Ctx, missing); err != nil {
		t.Fatalf("failed to delete pvc for missing-pvc scenario: %v", err)
	}

	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete this reconcile step")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseStalled {
		t.Fatalf("expected phase Stalled, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonPVCNotBound {
		t.Fatalf("expected reason PVCNotBound, got %s", status.Reason)
	}
	if !strings.Contains(status.Message, "Target PVCs not found") {
		t.Fatalf("expected missing PVC message, got %q", status.Message)
	}
}

func TestResizeValidationUnboundPVCStalls(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})

	pvcName := "datadir-dnode-1"
	pvc := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: pvcName, Namespace: "testns"}, pvc); err != nil {
		t.Fatalf("failed to get pvc for unbound scenario: %v", err)
	}
	if err := oc.Client.Delete(oc.Ctx, pvc); err != nil {
		t.Fatalf("failed to delete pvc for unbound scenario: %v", err)
	}
	replacement := newBoundPVC(pvcName, "20Gi")
	replacement.Status.Phase = corev1.ClaimPending
	if err := oc.Client.Create(oc.Ctx, replacement); err != nil {
		t.Fatalf("failed to recreate pvc for unbound scenario: %v", err)
	}

	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete this reconcile step")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseStalled {
		t.Fatalf("expected phase Stalled, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonPVCNotBound {
		t.Fatalf("expected reason PVCNotBound, got %s", status.Reason)
	}
	if !strings.Contains(status.Message, "PVCs are not Bound") {
		t.Fatalf("expected unbound PVC message, got %q", status.Message)
	}
}

func TestResizeValidationStorageClassNotExpandableFails(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})

	sc := &storagev1.StorageClass{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "standard"}, sc); err != nil {
		t.Fatalf("failed to get storageclass for expansion scenario: %v", err)
	}
	allowExpansion := false
	sc.AllowVolumeExpansion = &allowExpansion
	if err := oc.Client.Update(oc.Ctx, sc); err != nil {
		t.Fatalf("failed to update storageclass for expansion scenario: %v", err)
	}

	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete this reconcile step")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be set")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseFailed {
		t.Fatalf("expected phase Failed, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonStorageClassNotExpandable {
		t.Fatalf("expected reason StorageClassNotExpandable, got %s", status.Reason)
	}
}

func TestResizeParallelPatchesAllPendingPVCs(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during resize submission pass: %v", err)
	}

	updated0 := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "datadir-dnode-0", Namespace: "testns"}, updated0); err != nil {
		t.Fatalf("failed to get pvc0: %v", err)
	}
	pvc0Req := updated0.Spec.Resources.Requests[corev1.ResourceStorage]
	if pvc0Req.Cmp(resourceMustParse("50Gi")) != 0 {
		t.Fatalf("expected pvc0 request to be 50Gi")
	}

	updated1 := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "datadir-dnode-1", Namespace: "testns"}, updated1); err != nil {
		t.Fatalf("failed to get pvc1: %v", err)
	}
	pvc1Req := updated1.Spec.Resources.Requests[corev1.ResourceStorage]
	if pvc1Req.Cmp(resourceMustParse("50Gi")) != 0 {
		t.Fatalf("expected pvc1 request to be 50Gi")
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status == nil || status.Phase != marklogicv1.VolumeResizePhaseWaitingForPVCResize {
		t.Fatalf("expected WaitingForPVCResize phase after parallel submission")
	}
}

func TestResizeSequentialPatchesOneAndAdvancesActivePVC(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	oc.MarklogicGroup.Spec.Persistence.ResizeStrategy = marklogicv1.VolumeResizeStrategySequential
	if err := oc.Client.Update(oc.Ctx, oc.MarklogicGroup); err != nil {
		t.Fatalf("failed to update group strategy: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during sequential submission pass: %v", err)
	}

	pvc0 := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "datadir-dnode-0", Namespace: "testns"}, pvc0); err != nil {
		t.Fatalf("failed to get pvc0: %v", err)
	}
	pvc0Req := pvc0.Spec.Resources.Requests[corev1.ResourceStorage]
	if pvc0Req.Cmp(resourceMustParse("50Gi")) != 0 {
		t.Fatalf("expected pvc0 request to be 50Gi")
	}

	pvc1 := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "datadir-dnode-1", Namespace: "testns"}, pvc1); err != nil {
		t.Fatalf("failed to get pvc1: %v", err)
	}
	pvc1Req := pvc1.Spec.Resources.Requests[corev1.ResourceStorage]
	if pvc1Req.Cmp(resourceMustParse("20Gi")) != 0 {
		t.Fatalf("expected pvc1 request to remain 20Gi")
	}

	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "50Gi"))
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during waiting pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.ActivePVC != "datadir-dnode-1" {
		t.Fatalf("expected activePVC to advance to datadir-dnode-1, got %s", status.ActivePVC)
	}
}

func TestResizeCheckpointClassificationOnlineAndOffline(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during submission pass: %v", err)
	}

	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "50Gi"))
	offlinePVC := newBoundPVC("datadir-dnode-1", "50Gi")
	offlinePVC.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
		Status: corev1.ConditionTrue,
	}}
	replacePVC(t, oc, offlinePVC)

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during waiting classification pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if len(status.PVCStatuses) != 2 {
		t.Fatalf("expected two pvc statuses, got %d", len(status.PVCStatuses))
	}

	if status.PVCStatuses[0].CheckpointType != marklogicv1.PVCResizeCheckpointTypeOnlineComplete {
		t.Fatalf("expected pvc0 online checkpoint, got %s", status.PVCStatuses[0].CheckpointType)
	}
	if status.PVCStatuses[1].CheckpointType != marklogicv1.PVCResizeCheckpointTypeOfflinePending {
		t.Fatalf("expected pvc1 offline checkpoint, got %s", status.PVCStatuses[1].CheckpointType)
	}
	if !status.PVCStatuses[1].RestartRequired {
		t.Fatalf("expected pvc1 restartRequired=true for offline checkpoint")
	}
}

func TestResizePartialFailureRetriesNonCheckpointedOnly(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during submission pass: %v", err)
	}

	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "50Gi"))
	missing := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "datadir-dnode-1", Namespace: "testns"}}
	if err := oc.Client.Delete(oc.Ctx, missing); err != nil {
		t.Fatalf("failed to delete pvc1 to induce partial failure: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during waiting failure pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseStalled {
		t.Fatalf("expected stalled phase after partial failure, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonPartialResizeFailure {
		t.Fatalf("expected stalled reason PartialResizeFailure, got %s", status.Reason)
	}
	if status.RetryCount == 0 || status.NextRetryTime == nil {
		t.Fatalf("expected retry fields to be populated after partial failure")
	}

	replacement := newBoundPVC("datadir-dnode-1", "20Gi")
	replacePVC(t, oc, replacement)
	status.NextRetryTime = &metav1.Time{Time: metav1.Now().Add(-time.Second)}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to force retry window open: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during retry transition pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during retry submission pass: %v", err)
	}

	pvc0 := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "datadir-dnode-0", Namespace: "testns"}, pvc0); err != nil {
		t.Fatalf("failed to get pvc0 after retry: %v", err)
	}
	pvc0Req := pvc0.Spec.Resources.Requests[corev1.ResourceStorage]
	if pvc0Req.Cmp(resourceMustParse("50Gi")) != 0 {
		t.Fatalf("expected checkpointed pvc0 to remain 50Gi")
	}
}

func TestResizeWaitingDoesNotCheckpointWhenRequestedBelowTarget(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	status.Phase = marklogicv1.VolumeResizePhaseWaitingForPVCResize
	status.PVCStatuses = []marklogicv1.PVCResizeStatus{
		{Name: "datadir-dnode-0", State: marklogicv1.PVCResizeStateWaitingForCheckpoint},
		{Name: "datadir-dnode-1", State: marklogicv1.PVCResizeStateWaitingForCheckpoint},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed waiting phase status: %v", err)
	}

	requestedLowObservedHigh := newBoundPVC("datadir-dnode-0", "20Gi")
	requestedLowObservedHigh.Status.Capacity[corev1.ResourceStorage] = resourceMustParse("50Gi")
	replacePVC(t, oc, requestedLowObservedHigh)

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during waiting pass: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	if len(updated.Status.VolumeResizeStatus.PVCStatuses) == 0 {
		t.Fatalf("expected pvcStatuses to be present")
	}
	entry := updated.Status.VolumeResizeStatus.PVCStatuses[0]
	if entry.State != marklogicv1.PVCResizeStateWaitingForCheckpoint {
		t.Fatalf("expected WaitingForCheckpoint when requested is below target, got %s", entry.State)
	}
	if entry.CheckpointType != "" {
		t.Fatalf("expected empty checkpointType, got %s", entry.CheckpointType)
	}
}

func TestResizeWaitingFailsAfterMaxRetries(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during submission pass: %v", err)
	}

	missing0 := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "datadir-dnode-0", Namespace: "testns"}}
	missing1 := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "datadir-dnode-1", Namespace: "testns"}}
	if err := oc.Client.Delete(oc.Ctx, missing0); err != nil {
		t.Fatalf("failed to delete pvc0: %v", err)
	}
	if err := oc.Client.Delete(oc.Ctx, missing1); err != nil {
		t.Fatalf("failed to delete pvc1: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	status.RetryCount = resizeMaxRetries
	status.NextRetryTime = &metav1.Time{Time: metav1.Now().Add(-time.Second)}
	status.Phase = marklogicv1.VolumeResizePhaseWaitingForPVCResize
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed retry threshold state: %v", err)
	}

	res := oc.ReconcileVolumeResizeValidation()
	if res.Completed() {
		if _, err := res.Output(); err != nil {
			t.Fatalf("unexpected error during failure transition: %v", err)
		}
	}

	status = getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseFailed {
		t.Fatalf("expected failed phase after max retries, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonMaxRetriesExceeded {
		t.Fatalf("expected MaxRetriesExceeded reason, got %s", status.Reason)
	}
}

func TestResizeTransitionsToStatefulSetSyncAfterAllCheckpointed(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during validation pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during submission pass: %v", err)
	}

	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "50Gi"))
	replacePVC(t, oc, newBoundPVC("datadir-dnode-1", "50Gi"))

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during waiting pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseSynchronizingStatefulSet {
		t.Fatalf("expected phase SynchronizingStatefulSet, got %s", status.Phase)
	}
}

func TestResizeSyncMarkersPersistAndResume(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-sync-markers",
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseSynchronizingStatefulSet,
		CurrentSize:        "20Gi",
		TargetSize:         "50Gi",
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          2,
		PVCsCheckpointed:   2,
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		Warnings:           []string{resizeMarkerSyncStarted, resizeMarkerTemplateSynced},
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflinePending, RestartRequired: true},
			{Name: "datadir-dnode-1", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
		},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed sync phase status: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during sync resume pass: %v", err)
	}

	updated := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if updated.Phase != marklogicv1.VolumeResizePhaseRestartingPods {
		t.Fatalf("expected phase RestartingPods, got %s", updated.Phase)
	}
	if !containsString(updated.Warnings, resizeMarkerTemplateSynced) {
		t.Fatalf("expected sync marker %q to persist", resizeMarkerTemplateSynced)
	}
	if !containsString(updated.Warnings, resizeMarkerRestartPlanPrepared) {
		t.Fatalf("expected restart-plan marker %q to be set", resizeMarkerRestartPlanPrepared)
	}
}

func TestResizeRestartCandidatesOnlyOfflinePending(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-sync-candidates",
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseSynchronizingStatefulSet,
		CurrentSize:        "20Gi",
		TargetSize:         "50Gi",
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          2,
		PVCsCheckpointed:   2,
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		Warnings:           []string{resizeMarkerSyncStarted, resizeMarkerTemplateSynced},
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflinePending, RestartRequired: true},
			{Name: "datadir-dnode-1", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
		},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed sync phase status: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during sync pass: %v", err)
	}

	updated := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	entry0 := findPVCStatus(updated, "datadir-dnode-0")
	entry1 := findPVCStatus(updated, "datadir-dnode-1")
	if entry0 == nil || entry1 == nil {
		t.Fatalf("expected both pvc status entries")
	}
	if entry0.State != marklogicv1.PVCResizeStateRestartPending {
		t.Fatalf("expected offline pvc to be RestartPending, got %s", entry0.State)
	}
	if entry1.State == marklogicv1.PVCResizeStateRestartPending {
		t.Fatalf("expected online pvc to avoid restart queue")
	}
}

func TestResizeRestartOrderReverseOrdinal(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType, replicas: 3})
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-restart-order",
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseRestartingPods,
		CurrentSize:        "20Gi",
		TargetSize:         "50Gi",
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          3,
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", PodName: "dnode-0", State: marklogicv1.PVCResizeStateRestartPending, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflinePending, RestartRequired: true},
			{Name: "datadir-dnode-1", PodName: "dnode-1", State: marklogicv1.PVCResizeStateRestartPending, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflinePending, RestartRequired: true},
			{Name: "datadir-dnode-2", PodName: "dnode-2", State: marklogicv1.PVCResizeStateRestartPending, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflinePending, RestartRequired: true},
		},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed restart phase status: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during restart pass: %v", err)
	}

	updated := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if updated.ActivePVC != "datadir-dnode-2" {
		t.Fatalf("expected highest ordinal pvc to restart first, got %s", updated.ActivePVC)
	}
	if updated.Phase != marklogicv1.VolumeResizePhaseWaitingForPodsReady {
		t.Fatalf("expected WaitingForPodsReady after restart trigger, got %s", updated.Phase)
	}

	deletedPod := &corev1.Pod{}
	err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "dnode-2", Namespace: "testns"}, deletedPod)
	if err == nil {
		t.Fatalf("expected dnode-2 pod to be deleted for restart")
	}
}

func TestResizeWaitingForPodsReadyBlocksUntilReadyThenAdvances(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-wait-ready",
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseWaitingForPodsReady,
		CurrentSize:        "20Gi",
		TargetSize:         "50Gi",
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          2,
		ActivePVC:          "datadir-dnode-1",
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", PodName: "dnode-0", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
			{Name: "datadir-dnode-1", PodName: "dnode-1", State: marklogicv1.PVCResizeStateRestartPending, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflinePending, RestartRequired: true},
		},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed waiting-for-ready phase status: %v", err)
	}

	replacePod(t, oc, newGroupPod("dnode-1", false))
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error while waiting for not-ready pod: %v", err)
	}

	blocked := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if blocked.Phase != marklogicv1.VolumeResizePhaseWaitingForPodsReady {
		t.Fatalf("expected phase to remain WaitingForPodsReady while pod is not ready, got %s", blocked.Phase)
	}

	replacePod(t, oc, newGroupPod("dnode-1", true))
	if err := runResizeStep(t, oc); err != nil {
		t.Fatalf("unexpected error while resuming after pod becomes ready: %v", err)
	}

	advanced := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if advanced.Phase != marklogicv1.VolumeResizePhaseVerifyingResizeOutcome {
		t.Fatalf("expected phase VerifyingResizeOutcome after pods become ready, got %s", advanced.Phase)
	}
}

func TestResizeVerificationTransitionsToCompleted(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "50Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	seedVerificationStatus(t, oc, "verify-success", "50Gi", "")

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during verification completion pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseCompleted {
		t.Fatalf("expected Completed phase, got %s", status.Phase)
	}
	if status.CompletionTime == nil {
		t.Fatalf("expected completionTime to be set")
	}
	if status.Reason != "" {
		t.Fatalf("expected empty reason on successful completion, got %s", status.Reason)
	}
	if status.PVCsCheckpointed != status.TotalPVCs {
		t.Fatalf("expected pvcsCheckpointed (%d) to equal totalPvcs (%d)", status.PVCsCheckpointed, status.TotalPVCs)
	}
}

func TestResizeVerificationRetryPathStallsThenResumes(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	seedVerificationStatus(t, oc, "verify-retry", "50Gi", "")
	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "20Gi"))

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during verification stall pass: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseStalled {
		t.Fatalf("expected Stalled phase, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonMarkLogicHealthCheckFailed {
		t.Fatalf("expected MarkLogicHealthCheckFailed reason, got %s", status.Reason)
	}

	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "50Gi"))
	replacePVC(t, oc, newBoundPVC("datadir-dnode-1", "50Gi"))
	status.NextRetryTime = &metav1.Time{Time: metav1.Now().Add(-time.Second)}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to open retry window: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during stalled resume pass: %v", err)
	}
	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during verification completion pass: %v", err)
	}

	status = getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseCompleted {
		t.Fatalf("expected Completed phase after retry resume, got %s", status.Phase)
	}
}

func TestResizeVerificationTerminalFailureAfterMaxRetries(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	seedVerificationStatus(t, oc, "verify-max-retries", "50Gi", "")
	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", "20Gi"))

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	status.RetryCount = resizeMaxRetries
	status.NextRetryTime = &metav1.Time{Time: metav1.Now().Add(-time.Second)}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed max-retry verification state: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during terminal verification failure pass: %v", err)
	}

	status = getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseFailed {
		t.Fatalf("expected Failed phase, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonMaxRetriesExceeded {
		t.Fatalf("expected MaxRetriesExceeded reason, got %s", status.Reason)
	}
}

func TestResizeDeferredTargetStartsAfterTerminalPersistence(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "60Gi", currentSize: "50Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	seedVerificationStatus(t, oc, "verify-deferred", "50Gi", "60Gi")

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during first verification completion pass: %v", err)
	}

	first := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if first.Phase != marklogicv1.VolumeResizePhaseCompleted {
		t.Fatalf("expected first pass to persist Completed, got %s", first.Phase)
	}
	if first.DeferredTargetSize != "60Gi" {
		t.Fatalf("expected deferred target to remain visible, got %s", first.DeferredTargetSize)
	}

	var second *marklogicv1.VolumeResizeStatus
	for i := 0; i < 4; i++ {
		if err := runResizeStep(t, oc); err != nil {
			t.Fatalf("unexpected error during new deferred operation start pass: %v", err)
		}
		second = getUpdatedGroup(t, oc).Status.VolumeResizeStatus
		if second != nil && second.Phase == marklogicv1.VolumeResizePhaseResizingPVCs && second.OperationID != first.OperationID {
			break
		}
	}

	if second == nil {
		t.Fatalf("expected status to be present while starting deferred operation")
	}
	if second.Phase != marklogicv1.VolumeResizePhaseResizingPVCs {
		t.Fatalf("expected deferred target to start new operation in ResizingPVCs, got %s", second.Phase)
	}
	if second.TargetSize != "60Gi" {
		t.Fatalf("expected new operation target 60Gi, got %s", second.TargetSize)
	}
	if second.OperationID == first.OperationID {
		t.Fatalf("expected a new operationID for deferred target operation")
	}
}

func TestResizeFinalStatusFieldsConsistentOnCompletion(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "50Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	seedVerificationStatus(t, oc, "verify-consistency", "50Gi", "")

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	status.RetryCount = 2
	status.NextRetryTime = &metav1.Time{Time: metav1.Now().Add(-time.Second)}
	status.ActivePVC = "datadir-dnode-1"
	status.FailedPVCs = []marklogicv1.FailedPVCStatus{{Name: "datadir-dnode-0", Reason: "OldError", Message: "stale"}}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed inconsistent verification status: %v", err)
	}

	if _, err := oc.ReconcileVolumeResizeValidation().Output(); err != nil {
		t.Fatalf("unexpected error during consistency verification pass: %v", err)
	}

	updated := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if updated.Phase != marklogicv1.VolumeResizePhaseCompleted {
		t.Fatalf("expected Completed phase, got %s", updated.Phase)
	}
	if updated.CompletionTime == nil {
		t.Fatalf("expected completionTime to be set")
	}
	if updated.RetryCount != 0 || updated.NextRetryTime != nil {
		t.Fatalf("expected retry fields to be cleared, got retryCount=%d nextRetry=%v", updated.RetryCount, updated.NextRetryTime)
	}
	if updated.ActivePVC != "" {
		t.Fatalf("expected activePVC to be cleared, got %s", updated.ActivePVC)
	}
	if len(updated.FailedPVCs) != 0 {
		t.Fatalf("expected failedPVCs to be empty, got %d", len(updated.FailedPVCs))
	}
	if updated.PVCsCheckpointed != updated.TotalPVCs {
		t.Fatalf("expected checkpoint counters to be consistent, got checkpointed=%d total=%d", updated.PVCsCheckpointed, updated.TotalPVCs)
	}
	for _, pvc := range updated.PVCStatuses {
		if pvc.State == marklogicv1.PVCResizeStateRestartPending {
			t.Fatalf("expected no RestartPending pvc status at completion")
		}
		if pvc.RestartRequired {
			t.Fatalf("expected restartRequired=false at completion for pvc %s", pvc.Name)
		}
	}
}

func TestResizeSyncUsesImmutableSafeDeleteRecreateFlow(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-immutable-sync",
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseSynchronizingStatefulSet,
		CurrentSize:        "20Gi",
		TargetSize:         "50Gi",
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          2,
		PVCsCheckpointed:   2,
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
			{Name: "datadir-dnode-1", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
		},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed sync status: %v", err)
	}

	if err := runResizeStep(t, oc); err != nil {
		t.Fatalf("unexpected error during delete step: %v", err)
	}

	deleted := &appsv1.StatefulSet{}
	err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "dnode", Namespace: "testns"}, deleted)
	if err == nil {
		t.Fatalf("expected statefulset to be deleted in immutable-safe flow")
	}

	updated := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if !containsString(updated.Warnings, resizeMarkerTemplateRecreateStarted) {
		t.Fatalf("expected marker %q", resizeMarkerTemplateRecreateStarted)
	}
	if !containsString(updated.Warnings, resizeMarkerTemplateDeleted) {
		t.Fatalf("expected marker %q", resizeMarkerTemplateDeleted)
	}
	if containsString(updated.Warnings, resizeMarkerTemplateSynced) {
		t.Fatalf("did not expect sync marker before recreate")
	}

	replaceStatefulSet(t, oc, "50Gi")
	if err := runResizeStep(t, oc); err != nil {
		t.Fatalf("unexpected error after recreate: %v", err)
	}

	updated = getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if !containsString(updated.Warnings, resizeMarkerTemplateRecreated) {
		t.Fatalf("expected marker %q", resizeMarkerTemplateRecreated)
	}
	if !containsString(updated.Warnings, resizeMarkerTemplateSynced) {
		t.Fatalf("expected marker %q", resizeMarkerTemplateSynced)
	}
}

func TestResizeSyncCrashResumeBetweenDeleteAndRecreateConverges(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        "resize-crash-resume-sync",
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseSynchronizingStatefulSet,
		CurrentSize:        "20Gi",
		TargetSize:         "50Gi",
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          2,
		PVCsCheckpointed:   2,
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		Warnings:           []string{resizeMarkerSyncStarted, resizeMarkerTemplateRecreateStarted, resizeMarkerTemplateDeleted},
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
			{Name: "datadir-dnode-1", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete},
		},
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed crash-resume status: %v", err)
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns"}}
	if err := oc.Client.Delete(oc.Ctx, sts); err != nil {
		t.Fatalf("failed to remove statefulset to simulate crash gap: %v", err)
	}

	if err := runResizeStep(t, oc); err != nil {
		t.Fatalf("unexpected error while statefulset is absent during crash-resume: %v", err)
	}

	replaceStatefulSet(t, oc, "50Gi")
	if err := runResizeStep(t, oc); err != nil {
		t.Fatalf("unexpected error after statefulset recreate in crash-resume: %v", err)
	}
	if err := runResizeStep(t, oc); err != nil {
		t.Fatalf("unexpected error while advancing from synced marker to next phase: %v", err)
	}

	updated := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if !containsString(updated.Warnings, resizeMarkerTemplateSynced) {
		t.Fatalf("expected sync marker after crash-resume convergence")
	}
	if updated.Phase != marklogicv1.VolumeResizePhaseWaitingForPodsReady {
		t.Fatalf("expected flow to advance after sync, got %s", updated.Phase)
	}
}

func TestResizeValidationStorageClassForbiddenShowsNamespaceScopeMessage(t *testing.T) {
	oc := newResizeTestContext(t, resizeTestInput{desiredSize: "50Gi", currentSize: "20Gi", updateStrategy: appsv1.OnDeleteStatefulSetStrategyType})
	oc.Client = &forbiddenStorageClassClient{Client: oc.Client}

	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		t.Fatalf("expected validation to complete")
	}
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	status := getUpdatedGroup(t, oc).Status.VolumeResizeStatus
	if status.Phase != marklogicv1.VolumeResizePhaseFailed {
		t.Fatalf("expected Failed phase, got %s", status.Phase)
	}
	if status.Reason != marklogicv1.VolumeResizeReasonStorageClassNotExpandable {
		t.Fatalf("expected StorageClassNotExpandable reason, got %s", status.Reason)
	}
	if !strings.Contains(status.Message, "cluster-scoped access to storageclasses") {
		t.Fatalf("expected explicit scope/permission message, got %q", status.Message)
	}
}

type resizeTestInput struct {
	desiredSize    string
	currentSize    string
	updateStrategy appsv1.StatefulSetUpdateStrategyType
	replicas       int32
}

func newResizeTestContext(t *testing.T, in resizeTestInput) *OperatorContext {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add marklogic scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add storage scheme: %v", err)
	}

	replicas := in.replicas
	if replicas == 0 {
		replicas = 2
	}
	group := &marklogicv1.MarklogicGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicGroup"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "dnode",
			Namespace:  "testns",
			Generation: 5,
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:           "dnode",
			Replicas:       &replicas,
			UpdateStrategy: in.updateStrategy,
			Persistence: &marklogicv1.Persistence{
				Enabled:          true,
				Size:             in.desiredSize,
				StorageClassName: "standard",
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: dataDirPVCName},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resourceMustParse(in.currentSize)},
						},
					},
				},
			},
		},
	}

	allowExpansion := true
	sc := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "standard"},
		AllowVolumeExpansion: &allowExpansion,
	}

	objects := []client.Object{group, sts, sc}
	for i := int32(0); i < replicas; i++ {
		pvcName := fmt.Sprintf("datadir-dnode-%d", i)
		podName := fmt.Sprintf("dnode-%d", i)
		objects = append(objects, newBoundPVC(pvcName, in.currentSize), newGroupPod(podName, true))
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
		WithObjects(objects...).
		Build()

	return &OperatorContext{
		Ctx:            context.Background(),
		Client:         fakeClient,
		Scheme:         scheme,
		MarklogicGroup: group,
		Recorder:       record.NewFakeRecorder(20),
	}
}

func newBoundPVC(name, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "testns"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: stringPtr("standard"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resourceMustParse(size)},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resourceMustParse(size),
			},
		},
	}
}

func resourceMustParse(val string) resource.Quantity {
	q, err := resource.ParseQuantity(val)
	if err != nil {
		panic(err)
	}
	return q
}

func replacePVC(t *testing.T, oc *OperatorContext, pvc *corev1.PersistentVolumeClaim) {
	t.Helper()
	existing := &corev1.PersistentVolumeClaim{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: pvc.Name, Namespace: pvc.Namespace}, existing); err == nil {
		if delErr := oc.Client.Delete(oc.Ctx, existing); delErr != nil {
			t.Fatalf("failed to delete existing pvc %s: %v", pvc.Name, delErr)
		}
	}
	if err := oc.Client.Create(oc.Ctx, pvc); err != nil {
		t.Fatalf("failed to create pvc %s: %v", pvc.Name, err)
	}
}

func newGroupPod(name string, ready bool) *corev1.Pod {
	readyStatus := corev1.ConditionFalse
	phase := corev1.PodPending
	if ready {
		readyStatus = corev1.ConditionTrue
		phase = corev1.PodRunning
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "testns",
			Labels: map[string]string{
				"app.kubernetes.io/name":     "marklogic",
				"app.kubernetes.io/instance": "dnode",
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: readyStatus},
			},
		},
	}
}

func replacePod(t *testing.T, oc *OperatorContext, pod *corev1.Pod) {
	t.Helper()
	existing := &corev1.Pod{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, existing); err == nil {
		if delErr := oc.Client.Delete(oc.Ctx, existing); delErr != nil {
			t.Fatalf("failed to delete existing pod %s: %v", pod.Name, delErr)
		}
	}
	if err := oc.Client.Create(oc.Ctx, pod); err != nil {
		t.Fatalf("failed to create pod %s: %v", pod.Name, err)
	}
}

func runResizeStep(t *testing.T, oc *OperatorContext) error {
	t.Helper()
	res := oc.ReconcileVolumeResizeValidation()
	if !res.Completed() {
		return nil
	}
	_, err := res.Output()
	return err
}

func seedVerificationStatus(t *testing.T, oc *OperatorContext, operationID, targetSize, deferredTarget string) {
	t.Helper()
	now := metav1.Now()
	status := &marklogicv1.VolumeResizeStatus{
		OperationID:        operationID,
		ObservedGeneration: oc.MarklogicGroup.Generation,
		Phase:              marklogicv1.VolumeResizePhaseVerifyingResizeOutcome,
		CurrentSize:        "20Gi",
		TargetSize:         targetSize,
		DeferredTargetSize: deferredTarget,
		ResizeStrategy:     marklogicv1.VolumeResizeStrategyParallel,
		TotalPVCs:          2,
		FirstStartedTime:   &now,
		LastTransitionTime: &now,
		PVCStatuses: []marklogicv1.PVCResizeStatus{
			{Name: "datadir-dnode-0", PodName: "dnode-0", State: marklogicv1.PVCResizeStateCheckpointed, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOnlineComplete, RestartRequired: false},
			{Name: "datadir-dnode-1", PodName: "dnode-1", State: marklogicv1.PVCResizeStateRestarted, CheckpointType: marklogicv1.PVCResizeCheckpointTypeOfflineComplete, RestartRequired: false},
		},
	}
	if deferredTarget != "" {
		status.DeferredObservedGeneration = oc.MarklogicGroup.Generation
	}
	if err := oc.patchResizeStatus(status); err != nil {
		t.Fatalf("failed to seed verification status: %v", err)
	}

	if err := setStatefulSetTemplateRequest(oc, targetSize); err != nil {
		t.Fatalf("failed to set statefulset template request: %v", err)
	}
	replacePVC(t, oc, newBoundPVC("datadir-dnode-0", targetSize))
	replacePVC(t, oc, newBoundPVC("datadir-dnode-1", targetSize))
}

func setStatefulSetTemplateRequest(oc *OperatorContext, size string) error {
	sts := &appsv1.StatefulSet{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "dnode", Namespace: "testns"}, sts); err != nil {
		return err
	}
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: dataDirPVCName}}}
	}
	if sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests == nil {
		sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests = corev1.ResourceList{}
	}
	sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resourceMustParse(size)
	return oc.Client.Update(oc.Ctx, sts)
}

func replaceStatefulSet(t *testing.T, oc *OperatorContext, size string) {
	t.Helper()
	existing := &appsv1.StatefulSet{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: "dnode", Namespace: "testns"}, existing); err == nil {
		if delErr := oc.Client.Delete(oc.Ctx, existing); delErr != nil {
			t.Fatalf("failed to delete existing statefulset: %v", delErr)
		}
	}
	replicas := int32(2)
	if oc.MarklogicGroup.Spec.Replicas != nil {
		replicas = *oc.MarklogicGroup.Spec.Replicas
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "dnode", Namespace: "testns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: dataDirPVCName},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resourceMustParse(size)},
						},
					},
				},
			},
		},
	}
	if err := oc.Client.Create(oc.Ctx, sts); err != nil {
		t.Fatalf("failed to recreate statefulset: %v", err)
	}
}

type forbiddenStorageClassClient struct {
	client.Client
}

func (c *forbiddenStorageClassClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*storagev1.StorageClass); ok {
		return apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, key.Name, fmt.Errorf("forbidden for test"))
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func findPVCStatus(status *marklogicv1.VolumeResizeStatus, name string) *marklogicv1.PVCResizeStatus {
	if status == nil {
		return nil
	}
	for i := range status.PVCStatuses {
		if status.PVCStatuses[i].Name == name {
			return &status.PVCStatuses[i]
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stringPtr(v string) *string {
	return &v
}

func getUpdatedGroup(t *testing.T, oc *OperatorContext) *marklogicv1.MarklogicGroup {
	t.Helper()
	updated := &marklogicv1.MarklogicGroup{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Name: oc.MarklogicGroup.Name, Namespace: oc.MarklogicGroup.Namespace}, updated); err != nil {
		t.Fatalf("failed to fetch updated group: %v", err)
	}
	return updated
}
