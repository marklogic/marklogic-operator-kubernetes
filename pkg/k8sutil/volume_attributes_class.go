package k8sutil

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const volumeAttributesClassDiscoveryTTL = 5 * time.Minute

const VolumeAttributesClassValidationCondition = "StorageAttributesValidated"

const volumeAttributesClassAdmissionRetryAnnotation = "marklogic.progress.com/vac-admission-retry"

type VolumeAttributesClassDiscovery interface {
	ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error)
}

type VolumeAttributesClassValidator struct {
	reader    client.Reader
	discovery VolumeAttributesClassDiscovery
	mu        sync.Mutex
	version   string
	expires   time.Time
}

type volumeAttributesClassValidationError struct {
	reason  string
	message string
	cause   error
}

func (failure *volumeAttributesClassValidationError) Error() string { return failure.message }
func (failure *volumeAttributesClassValidationError) Unwrap() error { return failure.cause }

func vacValidationError(reason, message string, cause error) error {
	return &volumeAttributesClassValidationError{reason: reason, message: message, cause: cause}
}

func NewVolumeAttributesClassValidator(reader client.Reader, discovery VolumeAttributesClassDiscovery) *VolumeAttributesClassValidator {
	return &VolumeAttributesClassValidator{reader: reader, discovery: discovery}
}

func (validator *VolumeAttributesClassValidator) invalidateDiscovery() {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	validator.expires = time.Time{}
}

func (validator *VolumeAttributesClassValidator) discoverVersion() (string, error) {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	if time.Now().Before(validator.expires) {
		if validator.version != "" {
			return validator.version, nil
		}
		return "", vacValidationError("VACUnsupported", "No served VolumeAttributesClass API was discovered", nil)
	}
	if validator.discovery == nil {
		return "", vacValidationError("VACDiscoveryFailed", "VolumeAttributesClass discovery is not configured", nil)
	}
	for _, version := range []string{"v1", "v1beta1"} {
		resources, err := validator.discovery.ServerResourcesForGroupVersion("storage.k8s.io/" + version)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", vacValidationError("VACDiscoveryFailed", fmt.Sprintf("Cannot discover VolumeAttributesClass API %s: %v", version, err), err)
		}
		if resources != nil {
			for _, resource := range resources.APIResources {
				if resource.Name == "volumeattributesclasses" && !resource.Namespaced {
					validator.version = version
					validator.expires = time.Now().Add(volumeAttributesClassDiscoveryTTL)
					return version, nil
				}
			}
		}
	}
	validator.version = ""
	validator.expires = time.Now().Add(volumeAttributesClassDiscoveryTTL)
	return "", vacValidationError("VACUnsupported", "No served VolumeAttributesClass API was discovered", nil)
}

func (validator *VolumeAttributesClassValidator) Validate(ctx context.Context, templates []corev1.PersistentVolumeClaim) error {
	managed := false
	for _, template := range templates {
		if className := template.Spec.VolumeAttributesClassName; className != nil && *className != "" {
			managed = true
			break
		}
	}
	if !managed {
		return nil
	}
	if validator == nil {
		return vacValidationError("VACDiscoveryFailed", "VolumeAttributesClass validation is not configured", nil)
	}
	version, err := validator.discoverVersion()
	if err != nil {
		return err
	}
	if validator.reader == nil {
		return vacValidationError("VACValidationFailed", "VolumeAttributesClass reader is not configured", nil)
	}
	classDrivers := map[string]string{}
	provisioners := map[string]string{}
	defaultClassName := ""
	defaultResolved := false
	for _, template := range templates {
		className := template.Spec.VolumeAttributesClassName
		if className == nil || *className == "" {
			continue
		}
		driver, found := classDrivers[*className]
		if !found {
			class := &unstructured.Unstructured{}
			class.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage.k8s.io", Version: version, Kind: "VolumeAttributesClass"})
			if err := validator.reader.Get(ctx, client.ObjectKey{Name: *className}, class); err != nil {
				var response apierrors.APIStatus
				endpointMissing := apierrors.IsNotFound(err) && errors.As(err, &response) && (response.Status().Details == nil || response.Status().Details.Name == "")
				if meta.IsNoMatchError(err) || endpointMissing {
					validator.invalidateDiscovery()
				}
				reason := "VACValidationFailed"
				if apierrors.IsNotFound(err) {
					reason = "VACNotFound"
				} else if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					reason = "VACAccessDenied"
				}
				if meta.IsNoMatchError(err) || endpointMissing {
					reason = "VACDiscoveryFailed"
				}
				return vacValidationError(reason, fmt.Sprintf("PVC template %s: cannot read VolumeAttributesClass %s: %v", template.Name, *className, err), err)
			}
			driver, _, err = unstructured.NestedString(class.Object, "driverName")
			if err != nil || driver == "" {
				return vacValidationError("VACInvalid", fmt.Sprintf("VolumeAttributesClass %s has no valid driverName", *className), err)
			}
			classDrivers[*className] = driver
		}
		storageClassName := ""
		if template.Spec.StorageClassName != nil {
			storageClassName = *template.Spec.StorageClassName
		} else {
			if !defaultResolved {
				classes := &storagev1.StorageClassList{}
				if err := validator.reader.List(ctx, classes); err != nil {
					return vacValidationError("VACStorageClassUnavailable", fmt.Sprintf("Cannot resolve the default StorageClass: %v", err), err)
				}
				var selected *storagev1.StorageClass
				for index := range classes.Items {
					candidate := &classes.Items[index]
					if candidate.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" && candidate.Annotations["storageclass.beta.kubernetes.io/is-default-class"] != "true" {
						continue
					}
					if selected == nil || candidate.CreationTimestamp.After(selected.CreationTimestamp.Time) || candidate.CreationTimestamp.Equal(&selected.CreationTimestamp) && candidate.Name < selected.Name {
						selected = candidate
					}
				}
				if selected != nil {
					defaultClassName = selected.Name
					provisioners[selected.Name] = selected.Provisioner
				}
				defaultResolved = true
			}
			storageClassName = defaultClassName
		}
		if storageClassName == "" {
			return vacValidationError("VACStorageClassUnavailable", fmt.Sprintf("PVC template %s requires a resolvable StorageClass to validate VolumeAttributesClass %s", template.Name, *className), nil)
		}
		provisioner, found := provisioners[storageClassName]
		if !found {
			storageClass := &storagev1.StorageClass{}
			if err := validator.reader.Get(ctx, client.ObjectKey{Name: storageClassName}, storageClass); err != nil {
				return vacValidationError("VACStorageClassUnavailable", fmt.Sprintf("PVC template %s: cannot read StorageClass %s: %v", template.Name, storageClassName, err), err)
			}
			provisioner = storageClass.Provisioner
			provisioners[storageClassName] = provisioner
		}
		if driver != provisioner {
			return vacValidationError("VACDriverMismatch", fmt.Sprintf("PVC template %s: VolumeAttributesClass %s driver %s does not match StorageClass %s provisioner %s", template.Name, *className, driver, storageClassName, provisioner), nil)
		}
	}
	return nil
}

func (oc *OperatorContext) ReconcileVolumeAttributesClasses() (reconcile.Result, error) {
	group := oc.MarklogicGroup
	if group == nil || group.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	if active, err := oc.reconcileStorageOperation(); active || err != nil {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	templates := volumeAttributesClassTemplates(group)
	managed := false
	for _, template := range templates {
		managed = managed || template.Spec.VolumeAttributesClassName != nil && *template.Spec.VolumeAttributesClassName != ""
	}
	if !managed {
		return reconcile.Result{}, nil
	}
	next := reconcile.Result{RequeueAfter: 30 * time.Second}
	if isResizeOperationActive(group.Status.VolumeResizeStatus) {
		return next, nil
	}
	statefulSet := &appsv1.StatefulSet{}
	reader := oc.volumeAttributesClassReader()
	if err := reader.Get(oc.Ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Spec.Name}, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return next, nil
		}
		return next, err
	}
	if group.UID == "" || statefulSet.UID == "" || statefulSet.DeletionTimestamp != nil || !metav1.IsControlledBy(statefulSet, group) {
		return next, fmt.Errorf("cannot reconcile VACs for an unverified or terminating StatefulSet %s", statefulSet.Name)
	}
	type target struct {
		template corev1.PersistentVolumeClaim
		ordinal  int64
		claim    *corev1.PersistentVolumeClaim
	}
	targets := []target{}
	failures := []error{}
	start, end := activeVolumeAttributesClassOrdinals(group, statefulSet)
	for _, template := range templates {
		declared := false
		for _, current := range statefulSet.Spec.VolumeClaimTemplates {
			declared = declared || current.Name == template.Name
		}
		if !declared {
			continue
		}
		for ordinal := start; ordinal < end; ordinal++ {
			claim := &corev1.PersistentVolumeClaim{}
			key := client.ObjectKey{Namespace: statefulSet.Namespace, Name: fmt.Sprintf("%s-%s-%d", template.Name, statefulSet.Name, ordinal)}
			if err := reader.Get(oc.Ctx, key, claim); err != nil {
				if !apierrors.IsNotFound(err) {
					failures = append(failures, fmt.Errorf("read VAC target PVC %s: %w", key.Name, err))
				}
				continue
			}
			if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" || claim.DeletionTimestamp != nil {
				oc.ReqLogger.V(1).Info("Waiting for managed PVC binding before VAC convergence", "pvc", claim.Name)
				continue
			}
			if err := oc.verifyVolumeAttributesClassClaim(group, statefulSet, template.Name, ordinal, claim); err != nil {
				failures = append(failures, err)
				continue
			}
			if volumeAttributesClassOperationPending(claim) {
				return next, errors.Join(failures...)
			}
			targets = append(targets, target{template: template, ordinal: ordinal, claim: claim})
		}
	}
	for _, target := range targets {
		attempted, err := oc.reconcileBoundVolumeAttributesClass(statefulSet, target.template, target.ordinal, target.claim)
		if err != nil {
			failures = append(failures, &volumeAttributesClassClaimError{name: target.claim.Name, err: err})
		}
		if attempted {
			break
		}
	}
	return next, errors.Join(failures...)
}

func (oc *OperatorContext) volumeAttributesClassReader() client.Reader {
	if oc.APIReader != nil {
		return oc.APIReader
	}
	return oc.Client
}

func volumeAttributesClassTemplates(group *marklogicv1.MarklogicGroup) []corev1.PersistentVolumeClaim {
	parameters := generateStatefulSetsParams(group)
	templates := []corev1.PersistentVolumeClaim{}
	if parameters.PersistentVolumeClaim.Name != "" {
		templates = append(templates, parameters.PersistentVolumeClaim)
	}
	if parameters.AdditionalVolumeClaimTemplates != nil {
		templates = append(templates, (*parameters.AdditionalVolumeClaimTemplates)...)
	}
	return templates
}

func activeVolumeAttributesClassOrdinals(group *marklogicv1.MarklogicGroup, statefulSet *appsv1.StatefulSet) (int64, int64) {
	start, replicas := int64(0), int32(1)
	if statefulSet.Spec.Ordinals != nil {
		start = int64(statefulSet.Spec.Ordinals.Start)
	}
	if statefulSet.Spec.Replicas != nil {
		replicas = *statefulSet.Spec.Replicas
	}
	if group.Spec.Replicas != nil && *group.Spec.Replicas < replicas {
		replicas = *group.Spec.Replicas
	}
	return start, start + int64(replicas)
}

func volumeAttributesClassOperationPending(claim *corev1.PersistentVolumeClaim) bool {
	if modification := claim.Status.ModifyVolumeStatus; modification != nil {
		switch string(modification.Status) {
		case "Pending", "InProgress":
			return true
		case "Infeasible":
			if requested := claim.Spec.VolumeAttributesClassName; requested != nil && modification.TargetVolumeAttributesClassName == *requested {
				return false
			}
		default:
			return true
		}
	}
	for _, condition := range claim.Status.Conditions {
		if string(condition.Type) == "ModifyingVolume" && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	requested := claim.Spec.VolumeAttributesClassName
	return requested != nil && *requested != "" && (claim.Status.CurrentVolumeAttributesClassName == nil || *claim.Status.CurrentVolumeAttributesClassName != *requested)
}

func (oc *OperatorContext) verifyVolumeAttributesClassClaim(group *marklogicv1.MarklogicGroup, statefulSet *appsv1.StatefulSet, templateName string, ordinal int64, claim *corev1.PersistentVolumeClaim) error {
	if identity := claim.Annotations[dataPVCGroupUIDAnnotation]; identity != "" && identity != string(group.UID) {
		return fmt.Errorf("PVC %s belongs to a different group UID", claim.Name)
	}
	verified := claim.Annotations[dataPVCGroupUIDAnnotation] == string(group.UID)
	for _, owner := range claim.OwnerReferences {
		if owner.Kind == "MarklogicGroup" && owner.APIVersion == marklogicv1.GroupVersion.String() && owner.Name == group.Name && owner.UID == group.UID || owner.Kind == "StatefulSet" && owner.APIVersion == "apps/v1" && owner.Name == statefulSet.Name && owner.UID == statefulSet.UID {
			verified = true
		} else {
			return fmt.Errorf("PVC %s has an unverified owner", claim.Name)
		}
	}
	if verified {
		return nil
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: statefulSet.Namespace, Name: fmt.Sprintf("%s-%d", statefulSet.Name, ordinal)}
	if err := oc.volumeAttributesClassReader().Get(oc.Ctx, key, pod); err != nil {
		return fmt.Errorf("verify managed Pod for PVC %s: %w", claim.Name, err)
	}
	if pod.UID == "" || pod.DeletionTimestamp != nil || !metav1.IsControlledBy(pod, statefulSet) {
		return fmt.Errorf("PVC %s has no verified active managed Pod", claim.Name)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == templateName && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim.Name {
			return nil
		}
	}
	return fmt.Errorf("PVC %s is not referenced by its managed Pod template volume", claim.Name)
}

func (oc *OperatorContext) reconcileBoundVolumeAttributesClass(statefulSet *appsv1.StatefulSet, template corev1.PersistentVolumeClaim, ordinal int64, expected *corev1.PersistentVolumeClaim) (bool, error) {
	requested := template.Spec.VolumeAttributesClassName
	if requested == nil || *requested == "" {
		return false, nil
	}
	attempted := false
	err := retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		reader := oc.volumeAttributesClassReader()
		group := &marklogicv1.MarklogicGroup{}
		if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(oc.MarklogicGroup), group); err != nil {
			return err
		}
		if group.UID == "" || group.UID != oc.MarklogicGroup.UID || group.Generation != oc.MarklogicGroup.Generation || group.DeletionTimestamp != nil || isResizeOperationActive(group.Status.VolumeResizeStatus) {
			return nil
		}
		selected := false
		for _, current := range volumeAttributesClassTemplates(group) {
			if current.Name == template.Name && current.Spec.VolumeAttributesClassName != nil && *current.Spec.VolumeAttributesClassName == *requested {
				selected = true
				break
			}
		}
		if !selected {
			return nil
		}
		currentSet := &appsv1.StatefulSet{}
		if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(statefulSet), currentSet); err != nil {
			return err
		}
		if statefulSet.UID == "" || currentSet.UID != statefulSet.UID || currentSet.DeletionTimestamp != nil || !metav1.IsControlledBy(currentSet, group) {
			return fmt.Errorf("StatefulSet %s identity or ownership changed before VAC update", statefulSet.Name)
		}
		start, end := activeVolumeAttributesClassOrdinals(group, currentSet)
		if ordinal < start || ordinal >= end {
			return nil
		}
		declared := false
		for _, current := range currentSet.Spec.VolumeClaimTemplates {
			declared = declared || current.Name == template.Name
		}
		if !declared {
			return fmt.Errorf("PVC template %s is not declared by StatefulSet %s", template.Name, currentSet.Name)
		}
		claim := &corev1.PersistentVolumeClaim{}
		key := client.ObjectKey{Namespace: currentSet.Namespace, Name: fmt.Sprintf("%s-%s-%d", template.Name, currentSet.Name, ordinal)}
		if err := reader.Get(oc.Ctx, key, claim); err != nil {
			return err
		}
		if expected.UID == "" || claim.UID != expected.UID || claim.Spec.VolumeName != expected.Spec.VolumeName || claim.DeletionTimestamp != nil {
			return fmt.Errorf("PVC %s identity, binding, or lifecycle changed before VAC update", claim.Name)
		}
		if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
			return nil
		}
		if err := oc.verifyVolumeAttributesClassClaim(group, currentSet, template.Name, ordinal, claim); err != nil {
			return err
		}
		if claim.Spec.VolumeAttributesClassName != nil && *claim.Spec.VolumeAttributesClassName == *requested {
			return nil
		}
		if volumeAttributesClassOperationPending(claim) || len(claim.Status.AllocatedResourceStatuses) != 0 {
			return nil
		}
		for _, condition := range claim.Status.Conditions {
			if (condition.Type == corev1.PersistentVolumeClaimResizing || condition.Type == corev1.PersistentVolumeClaimFileSystemResizePending) && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
		requestedSize := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		capacity := claim.Status.Capacity[corev1.ResourceStorage]
		if !capacity.IsZero() && requestedSize.Cmp(capacity) > 0 {
			return nil
		}
		if err := oc.validateVolumeAttributesClassBinding(claim, requested); err != nil {
			return err
		}
		if err := oc.volumeAttributesClassAdmissionFailure(group, claim, *requested); err != nil {
			return err
		}
		fingerprint, err := oc.volumeAttributesClassAdmissionFingerprint(group, *requested)
		if err != nil {
			return err
		}
		admissionTarget := &marklogicv1.VolumeAttributesClassAdmissionFailure{
			PVCName: claim.Name, PVCUID: string(claim.UID), TemplateName: template.Name,
			DesiredClassName: *requested, Fingerprint: fingerprint,
		}
		group, operation, err := oc.claimVACStorageOperation(group, claim, admissionTarget)
		if err != nil || operation == nil {
			return err
		}
		latestGroup := &marklogicv1.MarklogicGroup{}
		if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(group), latestGroup); err != nil {
			return err
		}
		if latestGroup.ResourceVersion != group.ResourceVersion || latestGroup.UID != group.UID || latestGroup.Generation != group.Generation || latestGroup.DeletionTimestamp != nil || isResizeOperationActive(latestGroup.Status.VolumeResizeStatus) {
			return nil
		}
		if savedOperation := storageOperation(latestGroup); savedOperation == nil || savedOperation.ID != operation.ID {
			return fmt.Errorf("storage-operation guard was not persisted for PVC %s", claim.Name)
		} else if savedOperation.AdmissionTarget == nil || *savedOperation.AdmissionTarget != *admissionTarget {
			return vacValidationError("VACAdmissionStateNotPersisted", "The API did not retain the VAC admission target; install the updated operator CRDs before submitting PVC changes", nil)
		}
		original := claim.DeepCopy()
		claim.Spec.VolumeAttributesClassName = requested
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[storageOperationAnnotation] = operation.ID
		claim.Annotations[dataPVCGroupUIDAnnotation] = string(group.UID)
		attempted = true
		if err := oc.Client.Patch(oc.Ctx, claim, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		saved := &corev1.PersistentVolumeClaim{}
		if err := reader.Get(oc.Ctx, key, saved); err != nil {
			return err
		}
		if saved.UID != claim.UID || saved.Spec.VolumeName != claim.Spec.VolumeName {
			return fmt.Errorf("PVC %s identity or binding changed during VAC readback", claim.Name)
		}
		if saved.Spec.VolumeAttributesClassName == nil || *saved.Spec.VolumeAttributesClassName != *requested {
			failure := volumeAttributesClassAdmissionError(claim.Name, *requested)
			persistErr := oc.persistVolumeAttributesClassAdmissionFailure(group, *admissionTarget)
			return errors.Join(failure, persistErr)
		}
		if group.Status.Storage != nil && len(group.Status.Storage.VolumeAttributesClassAdmissionFailures) > 0 {
			resolved := *admissionTarget
			resolved.Fingerprint = ""
			if err := oc.persistVolumeAttributesClassAdmissionFailure(group, resolved); err != nil {
				return err
			}
		}
		if oc.Recorder != nil {
			oc.Recorder.Eventf(group, corev1.EventTypeNormal, "VolumeAttributesClassRequested", "Requested VolumeAttributesClass %s for PVC %s; awaiting CSI application", *requested, claim.Name)
		}
		return nil
	})
	return attempted, err
}

func volumeAttributesClassAdmissionError(name, requested string) error {
	return vacValidationError("VACFieldNotPersisted", fmt.Sprintf("PVC %s did not retain requested VolumeAttributesClass %s; automatic resubmission is paused until its target or capability changes (set %s after repairing admission)", name, requested, volumeAttributesClassAdmissionRetryAnnotation), nil)
}

func (oc *OperatorContext) volumeAttributesClassAdmissionFingerprint(group *marklogicv1.MarklogicGroup, requested string) (string, error) {
	if oc.VACValidator == nil || oc.VACValidator.reader == nil {
		return "", vacValidationError("VACDiscoveryFailed", "VolumeAttributesClass admission retry validation is not configured", nil)
	}
	version, err := oc.VACValidator.discoverVersion()
	if err != nil {
		return "", err
	}
	class := &unstructured.Unstructured{}
	class.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage.k8s.io", Version: version, Kind: "VolumeAttributesClass"})
	if err := oc.VACValidator.reader.Get(oc.Ctx, client.ObjectKey{Name: requested}, class); err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]string{
		"apiVersion": version, "classUID": string(class.GetUID()), "target": requested,
		"retryToken": group.Annotations[volumeAttributesClassAdmissionRetryAnnotation],
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (oc *OperatorContext) volumeAttributesClassAdmissionFailure(group *marklogicv1.MarklogicGroup, claim *corev1.PersistentVolumeClaim, requested string) error {
	if group.Status.Storage == nil || claim.Spec.VolumeAttributesClassName != nil && *claim.Spec.VolumeAttributesClassName == requested {
		return nil
	}
	for _, failure := range group.Status.Storage.VolumeAttributesClassAdmissionFailures {
		if failure.PVCName != claim.Name || failure.PVCUID != string(claim.UID) || failure.DesiredClassName != requested {
			continue
		}
		fingerprint, err := oc.volumeAttributesClassAdmissionFingerprint(group, requested)
		if err != nil {
			return err
		}
		if fingerprint == failure.Fingerprint {
			return volumeAttributesClassAdmissionError(claim.Name, requested)
		}
	}
	return nil
}

func (oc *OperatorContext) persistVolumeAttributesClassAdmissionFailure(expected *marklogicv1.MarklogicGroup, failure marklogicv1.VolumeAttributesClassAdmissionFailure) error {
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		latest := &marklogicv1.MarklogicGroup{}
		if err := oc.volumeAttributesClassReader().Get(oc.Ctx, client.ObjectKeyFromObject(expected), latest); err != nil {
			return err
		}
		if latest.UID != expected.UID || latest.DeletionTimestamp != nil {
			return nil
		}
		selected := false
		for _, template := range volumeAttributesClassTemplates(latest) {
			selected = selected || template.Name == failure.TemplateName && template.Spec.VolumeAttributesClassName != nil && *template.Spec.VolumeAttributesClassName == failure.DesiredClassName
		}
		if !selected {
			return nil
		}
		original := latest.DeepCopy()
		if latest.Status.Storage == nil {
			latest.Status.Storage = &marklogicv1.StorageStatus{}
		}
		failures := []marklogicv1.VolumeAttributesClassAdmissionFailure{}
		for _, previous := range latest.Status.Storage.VolumeAttributesClassAdmissionFailures {
			if previous.PVCName != failure.PVCName {
				failures = append(failures, previous)
			}
		}
		if failure.Fingerprint != "" {
			failures = append(failures, failure)
		}
		if len(failures) == 0 {
			failures = nil
		}
		sort.Slice(failures, func(first, second int) bool { return failures[first].PVCName < failures[second].PVCName })
		latest.Status.Storage.VolumeAttributesClassAdmissionFailures = failures
		if reflect.DeepEqual(original, latest) {
			return nil
		}
		if err := oc.Client.Status().Patch(oc.Ctx, latest, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		saved := &marklogicv1.MarklogicGroup{}
		if err := oc.volumeAttributesClassReader().Get(oc.Ctx, client.ObjectKeyFromObject(expected), saved); err != nil {
			return err
		}
		retained := failure.Fingerprint == ""
		if saved.UID == expected.UID && saved.Status.Storage != nil {
			for _, previous := range saved.Status.Storage.VolumeAttributesClassAdmissionFailures {
				if previous.PVCName == failure.PVCName {
					retained = failure.Fingerprint != "" && previous == failure
					break
				}
			}
		} else {
			retained = false
		}
		if !retained {
			return vacValidationError("VACAdmissionStateNotPersisted", "The API did not retain the VAC admission failure latch; verify the operator CRD upgrade and status admission", nil)
		}
		oc.MarklogicGroup.ResourceVersion = saved.ResourceVersion
		oc.MarklogicGroup.Status.Storage = saved.Status.Storage.DeepCopy()
		return nil
	})
}

func (oc *OperatorContext) validateVolumeAttributesClassBinding(claim *corev1.PersistentVolumeClaim, requested *string) error {
	reader := oc.volumeAttributesClassReader()
	volume := &corev1.PersistentVolume{}
	if err := reader.Get(oc.Ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
		return vacValidationError("VACValidationFailed", fmt.Sprintf("read bound PV for PVC %s: %v", claim.Name, err), err)
	}
	binding := volume.Spec.ClaimRef
	if volume.UID == "" || volume.DeletionTimestamp != nil || binding == nil || binding.UID != claim.UID || binding.Name != claim.Name || binding.Namespace != claim.Namespace || volume.Spec.CSI == nil {
		return vacValidationError("VACBindingUnverified", fmt.Sprintf("PVC %s has no verified CSI PV binding", claim.Name), nil)
	}
	validationClaim := *claim.DeepCopy()
	validationClaim.Spec.VolumeAttributesClassName = requested
	if err := oc.VACValidator.Validate(oc.Ctx, []corev1.PersistentVolumeClaim{validationClaim}); err != nil {
		return err
	}
	storageClass := &storagev1.StorageClass{}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName == "" {
		return vacValidationError("VACStorageClassUnavailable", fmt.Sprintf("PVC %s has no StorageClass for bound driver validation", claim.Name), nil)
	}
	if err := reader.Get(oc.Ctx, client.ObjectKey{Name: *claim.Spec.StorageClassName}, storageClass); err != nil {
		return vacValidationError("VACValidationFailed", err.Error(), err)
	}
	if storageClass.Provisioner != volume.Spec.CSI.Driver {
		return vacValidationError("VACDriverMismatch", fmt.Sprintf("PVC %s StorageClass provisioner does not match bound PV CSI driver", claim.Name), nil)
	}
	return nil
}

func volumeAttributesClassProvisioningFallback(err error) bool {
	var failure *volumeAttributesClassValidationError
	return errors.As(err, &failure) && (failure.reason == "VACUnsupported" || failure.reason == "VACNotFound")
}

func (oc *OperatorContext) validateVolumeAttributesClassTemplates(templates []corev1.PersistentVolumeClaim) (error, error) {
	validationErr := oc.VACValidator.Validate(oc.Ctx, templates)
	managed := false
	for _, template := range templates {
		if className := template.Spec.VolumeAttributesClassName; className != nil && *className != "" {
			managed = true
			break
		}
	}
	previous := meta.FindStatusCondition(oc.MarklogicGroup.Status.Conditions, VolumeAttributesClassValidationCondition)
	if previous != nil {
		previous = previous.DeepCopy()
	}
	if !managed && previous == nil {
		return nil, nil
	}
	original := oc.MarklogicGroup.DeepCopy()
	changed := false
	eventType, reason, message := corev1.EventTypeNormal, "VACValidated", "VAC API, class references, and StorageClass drivers validated; CSI capabilities and parameter acceptance are not prevalidated"
	status := metav1.ConditionTrue
	if !managed {
		changed = meta.RemoveStatusCondition(&oc.MarklogicGroup.Status.Conditions, VolumeAttributesClassValidationCondition)
	} else {
		if validationErr != nil {
			var failure *volumeAttributesClassValidationError
			errors.As(validationErr, &failure)
			eventType, reason, message = corev1.EventTypeWarning, failure.reason, failure.message
			if volumeAttributesClassProvisioningFallback(validationErr) {
				message += "; new PVCs will be provisioned without this VAC, using StorageClass/driver defaults. "
				if reason == "VACUnsupported" {
					message += "To fix: enable a supported storage.k8s.io VolumeAttributesClass API/feature configuration or upgrade Kubernetes, then verify CSI provisioner/resizer support; remove the VAC selection if it is not required."
				} else {
					message += "To fix: create the referenced VolumeAttributesClass with a matching CSI driverName, correct the selected class name, or remove the VAC selection if it is not required."
				}
			}
			status = metav1.ConditionFalse
			if reason == "VACDiscoveryFailed" || reason == "VACAccessDenied" || reason == "VACValidationFailed" || failure.cause != nil && !apierrors.IsNotFound(failure.cause) {
				status = metav1.ConditionUnknown
			}
		}
		changed = meta.SetStatusCondition(&oc.MarklogicGroup.Status.Conditions, metav1.Condition{
			Type: VolumeAttributesClassValidationCondition, Status: status,
			ObservedGeneration: oc.MarklogicGroup.Generation, Reason: reason, Message: message,
		})
	}
	if changed {
		if err := oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return validationErr, fmt.Errorf("record VolumeAttributesClass validation: %w", err)
		}
		if managed && oc.Recorder != nil && (previous == nil || previous.Status != status || previous.Reason != reason || previous.Message != message) {
			oc.Recorder.Event(oc.MarklogicGroup, eventType, reason, message)
		}
		if managed && volumeAttributesClassProvisioningFallback(validationErr) && (previous == nil || previous.Status != status || previous.Reason != reason || previous.Message != message) {
			oc.ReqLogger.Info("WARNING: provisioning without unavailable VolumeAttributesClass", "reason", reason, "message", message)
		}
	}
	return validationErr, nil
}
