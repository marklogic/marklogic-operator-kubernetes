// This file coordinates VAC changes and volume resizing within a group.
// Persisted guards and PVC operation IDs preserve safety across retries and operator restarts.
package k8sutil

import (
	"fmt"
	"reflect"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const storageOperationAnnotation = "marklogic.progress.com/storage-operation-id"

func storageOperation(group *marklogicv1.MarklogicGroup) *marklogicv1.StorageOperationStatus {
	if group.Status.Storage == nil {
		return nil
	}
	return group.Status.Storage.Operation
}

func (oc *OperatorContext) claimVACStorageOperation(group *marklogicv1.MarklogicGroup, claim *corev1.PersistentVolumeClaim, target *marklogicv1.VolumeAttributesClassAdmissionFailure) (*marklogicv1.MarklogicGroup, *marklogicv1.StorageOperationStatus, error) {
	if isResizeOperationActive(group.Status.VolumeResizeStatus) {
		return nil, nil, nil
	}
	if operation := storageOperation(group); operation != nil {
		if operation.PVCName != claim.Name || operation.PVCUID != string(claim.UID) {
			return nil, nil, nil
		}
		return group, operation, nil
	}
	original := group.DeepCopy()
	if group.Status.Storage == nil {
		group.Status.Storage = &marklogicv1.StorageStatus{}
	}
	operation := &marklogicv1.StorageOperationStatus{ID: string(uuid.NewUUID()), PVCName: claim.Name, PVCUID: string(claim.UID), AdmissionTarget: target}
	group.Status.Storage.Operation = operation
	if err := oc.Client.Status().Patch(oc.Ctx, group, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, nil, err
	}
	oc.MarklogicGroup.ResourceVersion = group.ResourceVersion
	oc.MarklogicGroup.Status.Storage = group.Status.Storage.DeepCopy()
	return group, operation, nil
}

func (oc *OperatorContext) reconcileStorageOperation() (bool, error) {
	group := &marklogicv1.MarklogicGroup{}
	reader := oc.volumeAttributesClassReader()
	if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(oc.MarklogicGroup), group); err != nil {
		return true, client.IgnoreNotFound(err)
	}
	if group.UID != oc.MarklogicGroup.UID || group.DeletionTimestamp != nil {
		return true, nil
	}
	operation := storageOperation(group)
	if operation == nil {
		return false, nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKey{Namespace: group.Namespace, Name: operation.PVCName}
	readErr := reader.Get(oc.Ctx, key, claim)
	if readErr != nil && !apierrors.IsNotFound(readErr) {
		return true, readErr
	}
	if readErr == nil && string(claim.UID) == operation.PVCUID && claim.DeletionTimestamp == nil {
		if volumeAttributesClassOperationPending(claim) {
			return true, nil
		}
		admissionRejected := false
		if target := operation.AdmissionTarget; target != nil && (claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != target.DesiredClassName) {
			knownFailure := false
			for _, failure := range group.Status.Storage.VolumeAttributesClassAdmissionFailures {
				knownFailure = knownFailure || failure == *target
			}
			admissionRejected = knownFailure || claim.Annotations[storageOperationAnnotation] == operation.ID
			if admissionRejected && !knownFailure {
				if err := oc.persistVolumeAttributesClassAdmissionFailure(group, *target); err != nil {
					return true, err
				}
				if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(group), group); err != nil {
					return true, err
				}
				if latest := storageOperation(group); latest == nil || latest.ID != operation.ID {
					return true, nil
				}
			}
		}
		if !admissionRejected && claim.Annotations[storageOperationAnnotation] != operation.ID {
			original := claim.DeepCopy()
			if claim.Annotations == nil {
				claim.Annotations = map[string]string{}
			}
			claim.Annotations[storageOperationAnnotation] = operation.ID
			if err := oc.Client.Patch(oc.Ctx, claim, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) {
					return true, nil
				}
				return true, err
			}
			if err := reader.Get(oc.Ctx, key, claim); err != nil {
				return true, err
			}
			if string(claim.UID) != operation.PVCUID || claim.Annotations[storageOperationAnnotation] != operation.ID {
				return true, fmt.Errorf("PVC %s did not retain its storage-operation fence", claim.Name)
			}
			if volumeAttributesClassOperationPending(claim) {
				return true, nil
			}
		}
	}
	original := group.DeepCopy()
	group.Status.Storage.Operation = nil
	if reflect.DeepEqual(group.Status.Storage, &marklogicv1.StorageStatus{}) {
		group.Status.Storage = nil
	}
	if err := oc.Client.Status().Patch(oc.Ctx, group, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return true, nil
		}
		return true, err
	}
	oc.MarklogicGroup.ResourceVersion = group.ResourceVersion
	oc.MarklogicGroup.Status.Storage = group.Status.Storage.DeepCopy()
	return false, nil
}
