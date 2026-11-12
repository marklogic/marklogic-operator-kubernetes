/*
Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/k8sutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MarklogicGroupReconciler reconciles a MarklogicGroup object
type MarklogicGroupReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Log          logr.Logger
	Recorder     record.EventRecorder
	VACValidator *k8sutil.VolumeAttributesClassValidator
	APIReader    client.Reader
}

const (
	ConditionTypeReady      = "Ready"
	ConditionTypeInProgress = "InProgress"
	ConditionTypeError      = "Error"
)

//+kubebuilder:rbac:groups=marklogic.progress.com,resources=marklogicgroups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=marklogic.progress.com,resources=marklogicgroups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=marklogic.progress.com,resources=marklogicgroups/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets;replicasets;deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods;services;secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;patch;update
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattributesclasses,verbs=get
//+kubebuilder:rbac:groups=core;events.k8s.io,resources=events,verbs=create;patch;update
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MarklogicGroup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *MarklogicGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	logger := r.Log.
		WithValues("marklogicGroups", req.NamespacedName).
		WithValues("requestNamespace", req.Namespace)

	log.IntoContext(ctx, logger)

	oc, err := k8sutil.CreateOperatorContext(ctx, &req, r.Client, r.Scheme, r.Recorder)

	logger.Info("==== Reconciling MarklogicGroup")

	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogicServer resource not found. Exiting reconcile loop since there is nothing to do")
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get MarkLogicServer")
		return ctrl.Result{}, err
	}

	oc.VACValidator = r.VACValidator
	oc.APIReader = r.APIReader
	result, err := oc.ReconsileMarklogicGroupHandler()
	if err != nil {
		logger.Error(err, "Error reconciling statefulset")
		return ctrl.Result{}, err
	}

	if meta.FindStatusCondition(oc.MarklogicGroup.Status.Conditions, k8sutil.VolumeAttributesClassValidationCondition) != nil && (result.RequeueAfter == 0 || result.RequeueAfter > 30*time.Second) {
		result.RequeueAfter = 30 * time.Second
	}
	return result, nil
}

func markLogicGroupCreateUpdateDeletePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true // Reconcile on create
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			switch e.ObjectNew.(type) {
			case *marklogicv1.MarklogicGroup:
				if !reflect.DeepEqual(comparableAnnotations(e.ObjectOld), comparableAnnotations(e.ObjectNew)) {
					return true // Reconcile if annotations have changed
				}
				oldLables := e.ObjectOld.GetLabels()
				newLabels := e.ObjectNew.GetLabels()
				if !reflect.DeepEqual(oldLables, newLabels) {
					return true // Reconcile if labels have changed
				}
				oldObj := e.ObjectOld.(*marklogicv1.MarklogicGroup)
				newObj := e.ObjectNew.(*marklogicv1.MarklogicGroup)
				if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
					return true // Reconcile if the spec has changed
				}
				return false
			case *appsv1.StatefulSet:
				return true // Reconcile on update of StatefulSet
			case *corev1.Service:
				oldObj := e.ObjectOld.(*corev1.Service)
				newObj := e.ObjectNew.(*corev1.Service)
				if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
					return true // Reconcile if the spec has changed
				}
				return false // Reconcile on update of Service
			case *corev1.Pod:
				return true // Reconcile on pod updates for dynamic host finalizer lifecycle
			case *corev1.PersistentVolumeClaim:
				return true
			default:
				return false // Ignore updates for other types
			}
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true // Reconcile on delete
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false // Ignore generic events (optional)
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MarklogicGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.PersistentVolumeClaim{}, k8sutil.PrecreatedPVCGroupUIDIndex, k8sutil.PrecreatedPVCGroupUIDs); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, k8sutil.PodPVCReferenceIndex, k8sutil.PodPVCReferences); err != nil {
		return err
	}
	if r.VACValidator == nil {
		config := rest.CopyConfig(mgr.GetConfig())
		config.Timeout = 10 * time.Second
		discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			return err
		}
		r.VACValidator = k8sutil.NewVolumeAttributesClassValidator(mgr.GetAPIReader(), discoveryClient)
	}

	builder := ctrl.NewControllerManagedBy(mgr).
		Named("marklogicgroup-controller").
		For(&marklogicv1.MarklogicGroup{}).
		WithEventFilter(markLogicGroupCreateUpdateDeletePredicate()).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.pvcToMarklogicGroup)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podToMarklogicGroup))

	return builder.Complete(r)
}

func (r *MarklogicGroupReconciler) pvcToMarklogicGroup(ctx context.Context, object client.Object) []reconcile.Request {
	claim, ok := object.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	statefulSets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, statefulSets, client.InNamespace(claim.Namespace)); err != nil {
		r.Log.Error(err, "Cannot map PVC event to a MarklogicGroup", "pvc", claim.Name)
		return nil
	}
	requests := []reconcile.Request{}
	seen := map[string]bool{}
	for _, statefulSet := range statefulSets.Items {
		owner := metav1.GetControllerOf(&statefulSet)
		if owner == nil || owner.Kind != "MarklogicGroup" || owner.APIVersion != marklogicv1.GroupVersion.String() || owner.UID == "" {
			continue
		}
		start, replicas := int64(0), int64(1)
		if statefulSet.Spec.Ordinals != nil {
			start = int64(statefulSet.Spec.Ordinals.Start)
		}
		if statefulSet.Spec.Replicas != nil {
			replicas = int64(*statefulSet.Spec.Replicas)
		}
		for _, template := range statefulSet.Spec.VolumeClaimTemplates {
			prefix := template.Name + "-" + statefulSet.Name + "-"
			if !strings.HasPrefix(claim.Name, prefix) {
				continue
			}
			ordinal, err := strconv.ParseInt(strings.TrimPrefix(claim.Name, prefix), 10, 64)
			active := ordinal >= start && ordinal < start+replicas
			precreated := claim.Annotations[k8sutil.PrecreatedPVCGroupUIDIndex] == string(owner.UID)
			if err != nil || ordinal < 0 || !active && !precreated || claim.Name != fmt.Sprintf("%s%d", prefix, ordinal) || seen[owner.Name] {
				continue
			}
			seen[owner.Name] = true
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: claim.Namespace, Name: owner.Name}})
		}
	}
	return requests
}

// podToMarklogicGroup maps a Pod to its owning MarklogicGroup by traversing
// the ownership chain: Pod -> StatefulSet -> MarklogicGroup.
func (r *MarklogicGroupReconciler) podToMarklogicGroup(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	requests := []reconcile.Request{}
	seen := map[client.ObjectKey]bool{}
	appendRequest := func(key client.ObjectKey) {
		if !seen[key] {
			seen[key] = true
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}

	// Find the StatefulSet that owns this pod
	for _, ownerRef := range pod.GetOwnerReferences() {
		if ownerRef.Kind != "StatefulSet" {
			continue
		}

		// Get the StatefulSet
		var sts appsv1.StatefulSet
		if err := r.Get(ctx, types.NamespacedName{Name: ownerRef.Name, Namespace: pod.Namespace}, &sts); err != nil {
			continue
		}

		// Find the MarklogicGroup that owns the StatefulSet
		for _, stsOwnerRef := range sts.GetOwnerReferences() {
			if stsOwnerRef.Kind == "MarklogicGroup" {
				appendRequest(client.ObjectKey{Name: stsOwnerRef.Name, Namespace: pod.Namespace})
			}
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		claim := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, claim); err != nil || claim.Annotations[k8sutil.PrecreatedPVCGroupUIDIndex] == "" {
			continue
		}
		for _, request := range r.pvcToMarklogicGroup(ctx, claim) {
			appendRequest(request.NamespacedName)
		}
	}
	return requests
}
