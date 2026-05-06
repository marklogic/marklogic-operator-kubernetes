package k8sutil

import (
	"context"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	if _, err := res.Output(); err != nil {
		t.Fatalf("unexpected result error: %v", err)
	}

	updated := getUpdatedGroup(t, oc)
	status := updated.Status.VolumeResizeStatus
	if status == nil {
		t.Fatalf("expected volumeResizeStatus to be initialized")
	}
	if status.Phase != marklogicv1.VolumeResizePhaseValidating {
		t.Fatalf("expected phase Validating, got %s", status.Phase)
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

type resizeTestInput struct {
	desiredSize    string
	currentSize    string
	updateStrategy appsv1.StatefulSetUpdateStrategyType
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

	replicas := int32(2)
	group := &marklogicv1.MarklogicGroup{
		TypeMeta: metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicGroup"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "dnode",
			Namespace:  "testns",
			Generation: 5,
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:           "dnode",
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
		},
	}

	pvc0 := newBoundPVC("datadir-dnode-0", in.currentSize)
	pvc1 := newBoundPVC("datadir-dnode-1", in.currentSize)
	allowExpansion := true
	sc := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "standard"},
		AllowVolumeExpansion: &allowExpansion,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&marklogicv1.MarklogicGroup{}).
		WithObjects(group, sts, pvc0, pvc1, sc).
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
