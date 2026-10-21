package k8sutil

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const VolumeAttributesClassSyncedCondition = "StorageAttributesSynced"

const StorageClaimsInUseCondition = "StorageClaimsInUse"

const PrecreatedPVCGroupUIDIndex = dataPVCGroupUIDAnnotation

const PodPVCReferenceIndex = "marklogic.progress.com/pvc-reference"

const volumeAttributesClassStallTimeout = 30 * time.Minute

func PrecreatedPVCGroupUIDs(object client.Object) []string {
	if identity := object.GetAnnotations()[dataPVCGroupUIDAnnotation]; identity != "" {
		return []string{identity}
	}
	return nil
}

func PodPVCReferences(object client.Object) []string {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return nil
	}
	references := []string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			references = append(references, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	return references
}

type volumeAttributesClassClaimError struct {
	name string
	err  error
}

func (failure *volumeAttributesClassClaimError) Error() string {
	return fmt.Sprintf("update VAC for PVC %s: %v", failure.name, failure.err)
}

func (failure *volumeAttributesClassClaimError) Unwrap() error { return failure.err }

func (oc *OperatorContext) unusedPrecreatedPVCs(group *marklogicv1.MarklogicGroup) ([]marklogicv1.UnusedPVCStatus, bool, error) {
	claims := &corev1.PersistentVolumeClaimList{}
	reader := oc.volumeAttributesClassReader()
	if err := oc.Client.List(oc.Ctx, claims, client.InNamespace(group.Namespace), client.MatchingFields{PrecreatedPVCGroupUIDIndex: string(group.UID)}); err != nil {
		return nil, true, fmt.Errorf("list precreated PVCs: %w", err)
	}
	tracked := []corev1.PersistentVolumeClaim{}
	for _, claim := range claims.Items {
		if claim.UID != "" && claim.DeletionTimestamp == nil && claim.Annotations[dataPVCGroupUIDAnnotation] == string(group.UID) {
			tracked = append(tracked, claim)
		}
	}
	if len(tracked) == 0 {
		return nil, false, nil
	}
	statefulSet := &appsv1.StatefulSet{}
	if err := reader.Get(oc.Ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Spec.Name}, statefulSet); err != nil {
		return nil, true, fmt.Errorf("read StatefulSet for precreated PVC observation: %w", err)
	}
	if statefulSet.UID == "" || statefulSet.DeletionTimestamp != nil || !metav1.IsControlledBy(statefulSet, group) {
		return nil, true, fmt.Errorf("cannot verify StatefulSet identity, ownership or lifecycle for precreated PVC observation")
	}
	start, liveReplicas, desiredReplicas := int64(0), int32(1), int32(1)
	if statefulSet.Spec.Ordinals != nil {
		start = int64(statefulSet.Spec.Ordinals.Start)
	}
	if statefulSet.Spec.Replicas != nil {
		liveReplicas = *statefulSet.Spec.Replicas
	}
	if group.Spec.Replicas != nil {
		desiredReplicas = *group.Spec.Replicas
	}
	previous := map[string]marklogicv1.UnusedPVCStatus{}
	if group.Status.Storage != nil {
		for _, claim := range group.Status.Storage.UnusedPVCs {
			previous[claim.Name] = claim
		}
	}
	var candidates []marklogicv1.UnusedPVCStatus
	for _, claim := range tracked {
		for _, template := range statefulSet.Spec.VolumeClaimTemplates {
			prefix := template.Name + "-" + statefulSet.Name + "-"
			if !strings.HasPrefix(claim.Name, prefix) {
				continue
			}
			ordinal, err := strconv.ParseInt(strings.TrimPrefix(claim.Name, prefix), 10, 32)
			if err != nil || ordinal < 0 || fmt.Sprintf("%s%d", prefix, ordinal) != claim.Name || ordinal >= start && ordinal < start+int64(liveReplicas) {
				continue
			}
			current := &corev1.PersistentVolumeClaim{}
			if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(&claim), current); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, true, fmt.Errorf("verify precreated PVC %s: %w", claim.Name, err)
			}
			if current.UID != claim.UID || current.DeletionTimestamp != nil || current.Annotations[dataPVCGroupUIDAnnotation] != string(group.UID) {
				continue
			}
			verified := true
			for _, owner := range current.OwnerReferences {
				if !(owner.APIVersion == marklogicv1.GroupVersion.String() && owner.Kind == "MarklogicGroup" && owner.Name == group.Name && owner.UID == group.UID ||
					owner.APIVersion == appsv1.SchemeGroupVersion.String() && owner.Kind == "StatefulSet" && owner.Name == statefulSet.Name && owner.UID == statefulSet.UID) {
					verified = false
				}
			}
			if !verified {
				continue
			}
			claim = *current
			status := marklogicv1.UnusedPVCStatus{
				Name: claim.Name, UID: string(claim.UID), TemplateName: template.Name, Phase: claim.Status.Phase,
				VolumeName: claim.Spec.VolumeName, VolumeAttributesClassName: claim.Spec.DeepCopy().VolumeAttributesClassName,
				Reason: "OutsideReplicaRange", Message: "Precreated PVC is outside the desired and live replica ranges; retained for reuse or manual review",
				FirstObservedTime: metav1.Now(),
			}
			if ordinal >= start && ordinal < start+int64(desiredReplicas) {
				status.Reason = "ScaleUpPending"
				status.Message = "PVC was precreated for a desired replica that is not active in the StatefulSet; retained for automatic reuse when scale-up succeeds"
			}
			status.Message += ". This is an unused candidate, not proof that the volume is empty; verify Pod references and retention/reclaim policies before any manual deletion. No automatic cleanup is performed."
			if claim.Status.Phase == corev1.ClaimBound || claim.Spec.VolumeName != "" {
				status.Message += " This claim has bound or assigned storage that may contain data; deleting it may delete the backing volume."
			}
			if before := previous[claim.Name]; before.UID == status.UID && !before.FirstObservedTime.IsZero() {
				status.FirstObservedTime = before.FirstObservedTime
			}
			candidates = append(candidates, status)
			break
		}
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	var unused []marklogicv1.UnusedPVCStatus
	for _, status := range candidates {
		pods := &corev1.PodList{}
		if err := oc.Client.List(oc.Ctx, pods, client.InNamespace(group.Namespace), client.MatchingFields{PodPVCReferenceIndex: status.Name}); err != nil {
			return nil, true, fmt.Errorf("list Pod references for precreated PVC observation: %w", err)
		}
		if len(pods.Items) == 0 {
			unused = append(unused, status)
		}
	}
	sort.Slice(unused, func(first, second int) bool { return unused[first].Name < unused[second].Name })
	return unused, len(unused) > 0, nil
}

func (oc *OperatorContext) observeUnusedPrecreatedPVCs() (reconcile.Result, error) {
	group := oc.MarklogicGroup
	if group == nil || group.UID == "" || group.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	fresh := &marklogicv1.MarklogicGroup{}
	reader := oc.volumeAttributesClassReader()
	if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(group), fresh); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.UID != group.UID || fresh.Generation != group.Generation || fresh.DeletionTimestamp != nil {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	unused, tracked, observationErr := oc.unusedPrecreatedPVCs(fresh)
	next := reconcile.Result{}
	if tracked || fresh.Status.Storage != nil && len(fresh.Status.Storage.UnusedPVCs) != 0 {
		next.RequeueAfter = 30 * time.Second
	}
	patchErr := retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		latest := &marklogicv1.MarklogicGroup{}
		if err := reader.Get(oc.Ctx, client.ObjectKeyFromObject(fresh), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != fresh.UID || latest.Generation != fresh.Generation || latest.DeletionTimestamp != nil {
			return nil
		}
		original := latest.DeepCopy()
		var previous []marklogicv1.UnusedPVCStatus
		if latest.Status.Storage != nil {
			previous = latest.Status.Storage.UnusedPVCs
		}
		before := meta.FindStatusCondition(latest.Status.Conditions, StorageClaimsInUseCondition)
		if before != nil {
			before = before.DeepCopy()
		}
		if observationErr != nil {
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{Type: StorageClaimsInUseCondition,
				Status: metav1.ConditionUnknown, ObservedGeneration: latest.Generation, Reason: "PrecreatedPVCObservationFailed", Message: observationErr.Error()})
		} else {
			if len(unused) > 0 && latest.Status.Storage == nil {
				latest.Status.Storage = &marklogicv1.StorageStatus{}
			}
			if latest.Status.Storage != nil {
				latest.Status.Storage.UnusedPVCs = unused
				if reflect.DeepEqual(latest.Status.Storage, &marklogicv1.StorageStatus{}) {
					latest.Status.Storage = nil
				}
			}
			if len(unused) > 0 {
				meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{Type: StorageClaimsInUseCondition,
					Status: metav1.ConditionFalse, ObservedGeneration: latest.Generation, Reason: "UnusedPrecreatedPVCs",
					Message: fmt.Sprintf("%d precreated PVCs have no observed Pod references and are outside the live replica range; retained without automatic deletion. See status.storage.unusedPVCs", len(unused))})
			} else {
				meta.RemoveStatusCondition(&latest.Status.Conditions, StorageClaimsInUseCondition)
			}
		}
		if reflect.DeepEqual(original, latest) {
			return nil
		}
		if err := oc.Client.Status().Patch(oc.Ctx, latest, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		if observationErr != nil {
			if before == nil || before.Status != metav1.ConditionUnknown || before.Message != observationErr.Error() {
				oc.ReqLogger.Info("WARNING: cannot determine unused precreated PVCs", "message", observationErr.Error())
				if oc.Recorder != nil {
					oc.Recorder.Event(latest, corev1.EventTypeWarning, "PrecreatedPVCObservationFailed", observationErr.Error())
				}
			}
			return nil
		}
		old := map[string]marklogicv1.UnusedPVCStatus{}
		for _, status := range previous {
			old[status.Name] = status
		}
		for _, status := range unused {
			if !reflect.DeepEqual(old[status.Name], status) {
				oc.ReqLogger.Info("WARNING: potentially unused precreated PVC retained", "pvc", status.Name, "uid", status.UID, "reason", status.Reason, "message", status.Message)
				if oc.Recorder != nil {
					oc.Recorder.Eventf(latest, corev1.EventTypeWarning, "UnusedPrecreatedPVC", "PVC %s (%s): %s", status.Name, status.Reason, status.Message)
				}
			}
		}
		if len(previous) > 0 && len(unused) == 0 && oc.Recorder != nil {
			oc.Recorder.Event(latest, corev1.EventTypeNormal, "UnusedPrecreatedPVCsResolved", "Previously reported PVCs are no longer unused candidates; they may have been reused, referenced, or removed")
		}
		return nil
	})
	return next, errors.Join(observationErr, patchErr)
}

func (oc *OperatorContext) observeVolumeAttributesClasses(dispatchErr error) (reconcile.Result, error) {
	group := oc.MarklogicGroup
	if group == nil || group.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	fresh := &marklogicv1.MarklogicGroup{}
	if err := oc.volumeAttributesClassReader().Get(oc.Ctx, client.ObjectKeyFromObject(group), fresh); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.UID != group.UID || fresh.Generation != group.Generation || fresh.DeletionTimestamp != nil {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	group = fresh
	templates := selectedVolumeAttributesClassTemplates(group)
	if len(templates) == 0 {
		return reconcile.Result{}, patchVolumeAttributesClassProgress(oc.Ctx, oc.Client, oc.volumeAttributesClassReader(), group, nil, oc.Recorder)
	}
	next := reconcile.Result{RequeueAfter: 30 * time.Second}
	observation := &marklogicv1.VolumeAttributesClassStatus{ObservedGeneration: group.Generation}
	statefulSet := &appsv1.StatefulSet{}
	setErr := oc.volumeAttributesClassReader().Get(oc.Ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Spec.Name}, statefulSet)
	start, replicas := int64(0), int32(1)
	if setErr == nil {
		start, _ = activeVolumeAttributesClassOrdinals(group, statefulSet)
		if statefulSet.Spec.Replicas != nil {
			replicas = *statefulSet.Spec.Replicas
		}
	}
	if group.Spec.Replicas != nil {
		replicas = *group.Spec.Replicas
	}
	previous := map[string]marklogicv1.PVCVolumeAttributesClassStatus{}
	if group.Status.Storage != nil && group.Status.Storage.VolumeAttributesClass != nil {
		for _, status := range group.Status.Storage.VolumeAttributesClass.PVCs {
			previous[status.Name] = status
		}
	}
	for _, template := range templates {
		validationErr := oc.VACValidator.Validate(oc.Ctx, []corev1.PersistentVolumeClaim{template})
		for ordinal := start; ordinal < start+int64(replicas); ordinal++ {
			status := marklogicv1.PVCVolumeAttributesClassStatus{
				Name:         fmt.Sprintf("%s-%s-%d", template.Name, group.Spec.Name, ordinal),
				TemplateName: template.Name, DesiredClassName: *template.Spec.VolumeAttributesClassName,
				State: marklogicv1.VolumeAttributesClassPending, Reason: "PVCNotFound", Message: "Waiting for the managed PVC to be created",
			}
			claim := &corev1.PersistentVolumeClaim{}
			readErr := oc.volumeAttributesClassReader().Get(oc.Ctx, client.ObjectKey{Namespace: group.Namespace, Name: status.Name}, claim)
			switch {
			case readErr != nil && !apierrors.IsNotFound(readErr):
				status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassUnknown, "PVCReadFailed", readErr.Error()
			case readErr == nil:
				status.UID = string(claim.UID)
				status.RequestedClassName = claim.DeepCopy().Spec.VolumeAttributesClassName
				status.CurrentClassName = claim.DeepCopy().Status.CurrentVolumeAttributesClassName
				status.State, status.Reason, status.Message = volumeAttributesClassNativeProgress(claim, status.DesiredClassName)
			}
			if setErr != nil && !apierrors.IsNotFound(setErr) {
				status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassUnknown, "StatefulSetReadFailed", setErr.Error()
			} else if setErr != nil {
				status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassPending, "WorkloadNotReady", "Waiting for the managed StatefulSet"
			} else if group.UID == "" || statefulSet.UID == "" || statefulSet.DeletionTimestamp != nil || !metav1.IsControlledBy(statefulSet, group) {
				status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassFailed, "WorkloadUnverified", "StatefulSet identity, ownership or lifecycle could not be verified"
			} else if apierrors.IsNotFound(readErr) && validationErr != nil {
				status.State, status.Reason, status.Message = volumeAttributesClassFailureProgress(validationErr)
			} else if readErr == nil {
				declared := false
				for _, current := range statefulSet.Spec.VolumeClaimTemplates {
					declared = declared || current.Name == template.Name
				}
				if !declared {
					status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassPending, "TemplateNotReady", "Waiting for the managed StatefulSet claim template"
				} else if err := oc.verifyVolumeAttributesClassClaim(group, statefulSet, template.Name, ordinal, claim); err != nil {
					status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassUnknown, "PVCOwnershipUnverified", err.Error()
				} else if claim.DeletionTimestamp != nil {
					status.State, status.Reason, status.Message = volumeAttributesClassNativeProgress(claim, status.DesiredClassName)
				} else if claim.Status.Phase == corev1.ClaimBound && claim.Spec.VolumeName != "" {
					if err := oc.validateVolumeAttributesClassBinding(claim, template.Spec.VolumeAttributesClassName); err != nil {
						status.State, status.Reason, status.Message = volumeAttributesClassFailureProgress(err)
					} else if err := oc.volumeAttributesClassAdmissionFailure(group, claim, status.DesiredClassName); err != nil {
						status.State, status.Reason, status.Message = volumeAttributesClassFailureProgress(err)
					}
				} else if err := oc.VACValidator.Validate(oc.Ctx, []corev1.PersistentVolumeClaim{*claim}); err != nil {
					status.State, status.Reason, status.Message = volumeAttributesClassFailureProgress(err)
					status.Message = "Saved PVC class reference: " + status.Message
					var failure *volumeAttributesClassValidationError
					if errors.As(err, &failure) && failure.reason == "VACNotFound" {
						status.Reason = "ProvisioningBlocked"
						status.Message += "; repair the original provisioning prerequisite, since the desired class cannot replace this reference before binding"
					}
				} else if validationErr != nil {
					status.State, status.Reason, status.Message = volumeAttributesClassFailureProgress(validationErr)
				}
			}
			if failure := volumeAttributesClassClaimFailure(dispatchErr, status.Name); failure != nil {
				status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassUnknown, "VACRequestUnconfirmed", failure.err.Error()
				var validationFailure *volumeAttributesClassValidationError
				if errors.As(failure.err, &validationFailure) {
					status.State, status.Reason, status.Message = volumeAttributesClassFailureProgress(failure.err)
				}
				if apierrors.IsInvalid(failure.err) || apierrors.IsForbidden(failure.err) || apierrors.IsBadRequest(failure.err) {
					status.State, status.Reason = marklogicv1.VolumeAttributesClassFailed, "VACRequestRejected"
				}
			}
			if readErr == nil && status.State == marklogicv1.VolumeAttributesClassInProgress {
				for _, condition := range claim.Status.Conditions {
					if condition.Type == "ModifyVolumeError" && condition.Status == corev1.ConditionTrue {
						status.ModifyVolumeError = true
						break
					}
				}
			}
			preserveVolumeAttributesClassProgressTime(&status, previous[status.Name], metav1.Now())
			observation.PVCs = append(observation.PVCs, status)
		}
	}
	sort.Slice(observation.PVCs, func(first, second int) bool { return observation.PVCs[first].Name < observation.PVCs[second].Name })
	summarizeVolumeAttributesClassProgress(observation)
	return next, patchVolumeAttributesClassProgress(oc.Ctx, oc.Client, oc.volumeAttributesClassReader(), group, observation, oc.Recorder)
}

func volumeAttributesClassClaimFailure(err error, name string) *volumeAttributesClassClaimError {
	if failure, ok := err.(*volumeAttributesClassClaimError); ok && failure.name == name {
		return failure
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if failure := volumeAttributesClassClaimFailure(cause, name); failure != nil {
				return failure
			}
		}
	}
	if cause := errors.Unwrap(err); cause != nil {
		return volumeAttributesClassClaimFailure(cause, name)
	}
	return nil
}

func selectedVolumeAttributesClassTemplates(group *marklogicv1.MarklogicGroup) []corev1.PersistentVolumeClaim {
	selected := []corev1.PersistentVolumeClaim{}
	for _, template := range volumeAttributesClassTemplates(group) {
		if template.Spec.VolumeAttributesClassName != nil && *template.Spec.VolumeAttributesClassName != "" {
			selected = append(selected, template)
		}
	}
	return selected
}

func (cc *ClusterContext) observeVolumeAttributesClasses() (reconcile.Result, error) {
	cluster := cc.MarklogicCluster
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	var reader client.Reader = cc.Client
	if cc.APIReader != nil {
		reader = cc.APIReader
	}
	observation := &marklogicv1.VolumeAttributesClassStatus{ObservedGeneration: cluster.Generation}
	parameters := generateMarkLogicClusterParams(cluster)
	for index := range cluster.Spec.MarkLogicGroups {
		desired := cc.GenerateMarkLogicGroupDef(cluster, index, generateMarkLogicGroupParams(cluster, index, parameters))
		if len(selectedVolumeAttributesClassTemplates(desired)) == 0 {
			continue
		}
		status := marklogicv1.GroupVolumeAttributesClassStatus{Name: desired.Name, State: marklogicv1.VolumeAttributesClassUnknown,
			Reason: "ChildStatusMissing", Message: "Waiting for managed group VAC progress"}
		child := &marklogicv1.MarklogicGroup{}
		readErr := reader.Get(cc.Ctx, client.ObjectKeyFromObject(desired), child)
		switch {
		case readErr != nil:
			if !apierrors.IsNotFound(readErr) {
				status.Reason, status.Message = "ChildReadFailed", readErr.Error()
			}
		case child.UID == "" || !metav1.IsControlledBy(child, cluster) || child.DeletionTimestamp != nil:
			status.Reason, status.Message = "ChildUnverified", "Group identity, ownership or lifecycle could not be verified"
		case !volumeAttributesClassSelectionsMatch(desired, child):
			status.Reason, status.Message = "ChildSpecNotConverged", "Group VAC selection or replica count does not yet match the latest cluster spec"
		default:
			condition := meta.FindStatusCondition(child.Status.Conditions, VolumeAttributesClassSyncedCondition)
			if child.Status.Storage != nil && child.Status.Storage.VolumeAttributesClass != nil {
				progress := child.Status.Storage.VolumeAttributesClass
				status.ObservedGeneration = progress.ObservedGeneration
				if progress.ObservedGeneration != child.Generation || condition == nil || condition.ObservedGeneration != child.Generation {
					status.Reason, status.Message = "ChildStatusStale", "Group VAC progress does not describe its latest generation"
				} else if volumeAttributesClassStatePriority(progress.State) == 5 && progress.State != marklogicv1.VolumeAttributesClassUnknown {
					status.Reason, status.Message = "ChildStatusUnknown", "Group reported an unrecognized VAC state"
				} else if progress.State == marklogicv1.VolumeAttributesClassCompleted && (condition.Status != metav1.ConditionTrue || !volumeAttributesClassChildCompletionMatches(child, progress)) {
					status.Reason, status.Message = "ChildStatusStale", "Group completion lacks current per-PVC evidence for all desired classes"
				} else {
					status.State, status.Reason, status.Message = progress.State, progress.Reason, progress.Message
				}
			}
		}
		observation.Groups = append(observation.Groups, status)
	}
	if len(observation.Groups) == 0 {
		return reconcile.Result{}, patchVolumeAttributesClassProgress(cc.Ctx, cc.Client, reader, cluster, nil, cc.Recorder)
	}
	sort.Slice(observation.Groups, func(first, second int) bool { return observation.Groups[first].Name < observation.Groups[second].Name })
	priority := 0
	for _, group := range observation.Groups {
		if current := volumeAttributesClassStatePriority(group.State); current > priority {
			priority = current
			observation.State, observation.Reason, observation.Message = group.State, group.Reason, fmt.Sprintf("Group %s: %s", group.Name, group.Message)
		}
	}
	if observation.State == marklogicv1.VolumeAttributesClassCompleted {
		observation.Reason, observation.Message = "VACApplied", fmt.Sprintf("All %d configured managed groups have applied their desired classes", len(observation.Groups))
	}
	return reconcile.Result{RequeueAfter: 30 * time.Second}, patchVolumeAttributesClassProgress(cc.Ctx, cc.Client, reader, cluster, observation, cc.Recorder)
}

func volumeAttributesClassSelectionsMatch(desired, current *marklogicv1.MarklogicGroup) bool {
	selections := func(group *marklogicv1.MarklogicGroup) map[string]string {
		classes := map[string]string{}
		for _, template := range selectedVolumeAttributesClassTemplates(group) {
			classes[template.Name] = *template.Spec.VolumeAttributesClassName
		}
		return classes
	}
	replicas := func(group *marklogicv1.MarklogicGroup) int32 {
		if group.Spec.Replicas != nil {
			return *group.Spec.Replicas
		}
		return 1
	}
	return desired.Spec.Name == current.Spec.Name && replicas(desired) == replicas(current) && reflect.DeepEqual(selections(desired), selections(current))
}

func volumeAttributesClassChildCompletionMatches(group *marklogicv1.MarklogicGroup, progress *marklogicv1.VolumeAttributesClassStatus) bool {
	classes := map[string]string{}
	for _, template := range selectedVolumeAttributesClassTemplates(group) {
		classes[template.Name] = *template.Spec.VolumeAttributesClassName
	}
	replicas := int32(1)
	if group.Spec.Replicas != nil {
		replicas = *group.Spec.Replicas
	}
	if len(progress.PVCs) == 0 || len(progress.PVCs) != int(replicas)*len(classes) {
		return false
	}
	counts, names := map[string]int{}, map[string]bool{}
	for _, claim := range progress.PVCs {
		if names[claim.Name] || claim.State != marklogicv1.VolumeAttributesClassCompleted || claim.DesiredClassName == "" || claim.DesiredClassName != classes[claim.TemplateName] || claim.RequestedClassName == nil || *claim.RequestedClassName != claim.DesiredClassName || claim.CurrentClassName == nil || *claim.CurrentClassName != claim.DesiredClassName {
			return false
		}
		names[claim.Name] = true
		counts[claim.TemplateName]++
	}
	for template := range classes {
		if counts[template] != int(replicas) {
			return false
		}
	}
	return true
}

func volumeAttributesClassNativeProgress(claim *corev1.PersistentVolumeClaim, desired string) (marklogicv1.VolumeAttributesClassState, string, string) {
	if claim.DeletionTimestamp != nil {
		return marklogicv1.VolumeAttributesClassPending, "PVCTerminating", "Waiting for the managed PVC replacement"
	}
	if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
		if claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != desired {
			return marklogicv1.VolumeAttributesClassPending, "PendingClassMismatch", "PVC must bind before its class reference can converge to the latest selection"
		}
		return marklogicv1.VolumeAttributesClassPending, "PVCNotBound", "Waiting for PVC binding and CSI application"
	}
	var nativeCondition *corev1.PersistentVolumeClaimCondition
	for index := range claim.Status.Conditions {
		condition := &claim.Status.Conditions[index]
		if condition.Status == corev1.ConditionTrue && (string(condition.Type) == "ModifyVolumeError" || string(condition.Type) == "ModifyingVolume") {
			nativeCondition = condition
			if string(condition.Type) == "ModifyVolumeError" {
				break
			}
		}
	}
	modification := claim.Status.ModifyVolumeStatus
	if modification != nil {
		if modification.TargetVolumeAttributesClassName != desired {
			return marklogicv1.VolumeAttributesClassPending, "PreviousTargetPending", "Native modification status refers to a previous target; awaiting fresh evidence for the desired class"
		}
		switch string(modification.Status) {
		case "Infeasible":
			message := "CSI reported the desired VolumeAttributesClass as infeasible; correct the class or driver prerequisites"
			if nativeCondition != nil {
				message += fmt.Sprintf(" (%s: %s)", nativeCondition.Reason, nativeCondition.Message)
			}
			return marklogicv1.VolumeAttributesClassFailed, "VACInfeasible", message
		case "Pending", "InProgress":
		default:
			return marklogicv1.VolumeAttributesClassUnknown, "VACNativeStateUnknown", fmt.Sprintf("Unrecognized native modification status %q", modification.Status)
		}
	}
	if nativeCondition != nil {
		if claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != desired {
			return marklogicv1.VolumeAttributesClassPending, "PreviousTargetPending", "Waiting for a previous modification before submitting the desired class"
		}
		reason, message := nativeCondition.Reason, nativeCondition.Message
		if reason == "" {
			reason = string(nativeCondition.Type)
		}
		if message == "" {
			message = "Waiting for CSI modification to complete"
		}
		return marklogicv1.VolumeAttributesClassInProgress, reason, message
	}
	if modification != nil {
		if string(modification.Status) == "Pending" {
			return marklogicv1.VolumeAttributesClassPending, "VACPending", "CSI modification is pending"
		}
		return marklogicv1.VolumeAttributesClassInProgress, "VACInProgress", "CSI modification is in progress"
	}
	if claim.Spec.VolumeAttributesClassName == nil || *claim.Spec.VolumeAttributesClassName != desired {
		return marklogicv1.VolumeAttributesClassPending, "VACNotRequested", "Waiting to submit the desired class reference"
	}
	if claim.Status.CurrentVolumeAttributesClassName == nil || *claim.Status.CurrentVolumeAttributesClassName != desired {
		return marklogicv1.VolumeAttributesClassPending, "VACAwaitingApplication", "Desired class reference is saved; awaiting Kubernetes-reported CSI application"
	}
	return marklogicv1.VolumeAttributesClassCompleted, "VACApplied", "PVC requested and applied classes match the desired class with no active modification or error"
}

func volumeAttributesClassFailureProgress(err error) (marklogicv1.VolumeAttributesClassState, string, string) {
	var failure *volumeAttributesClassValidationError
	if !errors.As(err, &failure) {
		return marklogicv1.VolumeAttributesClassUnknown, "VACObservationFailed", err.Error()
	}
	state := marklogicv1.VolumeAttributesClassFailed
	if failure.reason == "VACUnsupported" || failure.reason == "VACDiscoveryFailed" || failure.reason == "VACAccessDenied" || failure.reason == "VACValidationFailed" || failure.cause != nil && !apierrors.IsNotFound(failure.cause) {
		state = marklogicv1.VolumeAttributesClassUnknown
	}
	return state, failure.reason, failure.message
}

func preserveVolumeAttributesClassProgressTime(status *marklogicv1.PVCVolumeAttributesClassStatus, previous marklogicv1.PVCVolumeAttributesClassStatus, now metav1.Time) {
	current := *status
	current.LastProgressTime = metav1.Time{}
	before := previous
	before.LastProgressTime = metav1.Time{}
	if before.State == marklogicv1.VolumeAttributesClassStalled && (current.State == marklogicv1.VolumeAttributesClassPending || current.State == marklogicv1.VolumeAttributesClassInProgress) {
		before.State = current.State
	}
	status.LastProgressTime = now
	if !previous.LastProgressTime.IsZero() && reflect.DeepEqual(before, current) {
		status.LastProgressTime = previous.LastProgressTime
	}
	if (status.State == marklogicv1.VolumeAttributesClassPending || status.State == marklogicv1.VolumeAttributesClassInProgress) && now.Sub(status.LastProgressTime.Time) >= volumeAttributesClassStallTimeout {
		status.State = marklogicv1.VolumeAttributesClassStalled
	}
}

func volumeAttributesClassStatePriority(state marklogicv1.VolumeAttributesClassState) int {
	switch state {
	case marklogicv1.VolumeAttributesClassFailed:
		return 6
	case marklogicv1.VolumeAttributesClassUnknown:
		return 5
	case marklogicv1.VolumeAttributesClassStalled:
		return 4
	case marklogicv1.VolumeAttributesClassInProgress:
		return 3
	case marklogicv1.VolumeAttributesClassPending:
		return 2
	case marklogicv1.VolumeAttributesClassCompleted:
		return 1
	default:
		return 5
	}
}

func summarizeVolumeAttributesClassProgress(status *marklogicv1.VolumeAttributesClassStatus) {
	status.State, status.Reason, status.Message = marklogicv1.VolumeAttributesClassUnknown, "NoActivePVCs", "No active managed PVC targets; no CSI operation is reported as completed"
	priority := 0
	for _, claim := range status.PVCs {
		if current := volumeAttributesClassStatePriority(claim.State); current > priority {
			priority = current
			status.State, status.Reason, status.Message = claim.State, claim.Reason, fmt.Sprintf("PVC %s: %s", claim.Name, claim.Message)
		}
	}
	if status.State == marklogicv1.VolumeAttributesClassStalled {
		status.Reason = "VACStalled"
		status.Message = "No VAC progress for at least 30 minutes; " + status.Message
	}
	if status.State == marklogicv1.VolumeAttributesClassCompleted {
		status.Reason, status.Message = "VACApplied", fmt.Sprintf("All %d active managed PVCs have applied their desired classes", len(status.PVCs))
	}
}

func volumeAttributesClassStatusFields(object client.Object) (**marklogicv1.StorageStatus, *[]metav1.Condition) {
	switch resource := object.(type) {
	case *marklogicv1.MarklogicGroup:
		return &resource.Status.Storage, &resource.Status.Conditions
	case *marklogicv1.MarklogicCluster:
		return &resource.Status.Storage, &resource.Status.Conditions
	default:
		panic("VAC status requires a MarklogicGroup or MarklogicCluster")
	}
}

func patchVolumeAttributesClassProgress(ctx context.Context, writer client.Client, reader client.Reader, expected client.Object, observation *marklogicv1.VolumeAttributesClassStatus, recorder record.EventRecorder) error {
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		latest := expected.DeepCopyObject().(client.Object)
		if err := reader.Get(ctx, client.ObjectKeyFromObject(expected), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.GetUID() != expected.GetUID() || latest.GetGeneration() != expected.GetGeneration() || latest.GetDeletionTimestamp() != nil {
			return nil
		}
		original := latest.DeepCopyObject().(client.Object)
		if group, ok := latest.(*marklogicv1.MarklogicGroup); ok && group.Status.Storage != nil {
			selected := map[string]string{}
			for _, template := range selectedVolumeAttributesClassTemplates(group) {
				selected[template.Name] = *template.Spec.VolumeAttributesClassName
			}
			if operation := storageOperation(group); operation != nil && operation.AdmissionTarget != nil && selected[operation.AdmissionTarget.TemplateName] != operation.AdmissionTarget.DesiredClassName {
				operation.AdmissionTarget = nil
			}
			var failures []marklogicv1.VolumeAttributesClassAdmissionFailure
			for _, failure := range group.Status.Storage.VolumeAttributesClassAdmissionFailures {
				if selected[failure.TemplateName] == failure.DesiredClassName {
					failures = append(failures, failure)
				}
			}
			group.Status.Storage.VolumeAttributesClassAdmissionFailures = failures
		}
		storage, conditions := volumeAttributesClassStatusFields(latest)
		var previous *marklogicv1.VolumeAttributesClassStatus
		if *storage != nil {
			previous = (*storage).VolumeAttributesClass
		}
		if observation == nil {
			if *storage != nil {
				(*storage).VolumeAttributesClass = nil
				if reflect.DeepEqual(*storage, &marklogicv1.StorageStatus{}) {
					*storage = nil
				}
			}
			meta.RemoveStatusCondition(conditions, VolumeAttributesClassSyncedCondition)
		} else {
			if *storage == nil {
				*storage = &marklogicv1.StorageStatus{}
			}
			(*storage).VolumeAttributesClass = observation.DeepCopy()
			conditionStatus := metav1.ConditionFalse
			if observation.State == marklogicv1.VolumeAttributesClassCompleted {
				conditionStatus = metav1.ConditionTrue
			} else if observation.State == marklogicv1.VolumeAttributesClassUnknown {
				conditionStatus = metav1.ConditionUnknown
			}
			condition := metav1.Condition{Type: VolumeAttributesClassSyncedCondition, Status: conditionStatus,
				ObservedGeneration: observation.ObservedGeneration, Reason: observation.Reason, Message: observation.Message, LastTransitionTime: metav1.Now()}
			if len(metavalidation.ValidateConditions([]metav1.Condition{condition}, field.NewPath("conditions"))) != 0 {
				condition.Reason = "VAC" + string(observation.State)
				condition.Message = fmt.Sprintf("VAC state %s; full diagnostics are recorded under status.storage.volumeAttributesClass", observation.State)
			}
			meta.SetStatusCondition(conditions, condition)
		}
		if reflect.DeepEqual(original, latest) {
			return nil
		}
		if err := writer.Status().Patch(ctx, latest, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		emitVolumeAttributesClassProgressEvents(recorder, latest, previous, observation)
		return nil
	})
}

func emitVolumeAttributesClassProgressEvents(recorder record.EventRecorder, object client.Object, previous, current *marklogicv1.VolumeAttributesClassStatus) {
	if recorder == nil || current == nil {
		return
	}
	old := map[string]marklogicv1.PVCVolumeAttributesClassStatus{}
	if previous != nil {
		for _, status := range previous.PVCs {
			old[status.Name] = status
		}
	}
	emit := func(state marklogicv1.VolumeAttributesClassState, message string) {
		eventType := corev1.EventTypeWarning
		switch state {
		case marklogicv1.VolumeAttributesClassCompleted:
			eventType = corev1.EventTypeNormal
		case marklogicv1.VolumeAttributesClassFailed, marklogicv1.VolumeAttributesClassStalled, marklogicv1.VolumeAttributesClassUnknown:
		default:
			return
		}
		recorder.Event(object, eventType, "VolumeAttributesClass"+string(state), message)
	}
	for _, status := range current.PVCs {
		before := old[status.Name]
		if status.ModifyVolumeError && (!before.ModifyVolumeError || before.UID != status.UID || before.DesiredClassName != status.DesiredClassName || before.Reason != status.Reason) {
			recorder.Event(object, corev1.EventTypeWarning, "VolumeAttributesClassModifyError",
				fmt.Sprintf("PVC %s, desired class %s: %s: %s", status.Name, status.DesiredClassName, status.Reason, status.Message))
		}
		if before.UID != status.UID || before.DesiredClassName != status.DesiredClassName || before.State != status.State || before.Reason != status.Reason {
			emit(status.State, fmt.Sprintf("PVC %s, desired class %s: %s", status.Name, status.DesiredClassName, status.Message))
		}
	}
	if len(current.PVCs) == 0 && (previous == nil || previous.State != current.State || previous.Reason != current.Reason) {
		emit(current.State, current.Message)
	}
}
