/*
Copyright 2024.

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

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/k8sutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// MarklogicGroupReconciler reconciles a MarklogicGroup object
type MarklogicGroupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Log      logr.Logger
	Recorder record.EventRecorder
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

	result, err := oc.ReconsileMarklogicGroupHandler()
	if err != nil {
		logger.Error(err, "Error reconciling statefulset")
		return ctrl.Result{}, err
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
				oldAnnotations := e.ObjectOld.GetAnnotations()
				newAnnotations := e.ObjectNew.GetAnnotations()
				delete(newAnnotations, "banzaicloud.com/last-applied")
				delete(oldAnnotations, "banzaicloud.com/last-applied")
				delete(newAnnotations, "kubectl.kubernetes.io/last-applied-configuration")
				delete(oldAnnotations, "kubectl.kubernetes.io/last-applied-configuration")
				if !reflect.DeepEqual(oldAnnotations, newAnnotations) {
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

	builder := ctrl.NewControllerManagedBy(mgr).
		Named("marklogicgroup-controller").
		For(&marklogicv1.MarklogicGroup{}).
		WithEventFilter(markLogicGroupCreateUpdateDeletePredicate()).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{})

	return builder.Complete(r)
}
