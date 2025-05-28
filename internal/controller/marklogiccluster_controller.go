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
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/k8sutil"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// MarklogicClusterReconciler reconciles a MarklogicCluster object
type MarklogicClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Log      logr.Logger
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=marklogic.progress.com,resources=marklogicclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=marklogic.progress.com,resources=marklogicclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=marklogic.progress.com,resources=marklogicclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MarklogicCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *MarklogicClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info(fmt.Sprintf("Reconciling MarklogicGroup %s", req.NamespacedName))

	cc, err := k8sutil.CreateClusterContext(ctx, &req, r.Client, r.Scheme, r.Recorder)

	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogicCluster resource not found. Exiting reconcile loop since there is nothing to do")
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get MarkLogicCluster resource")
		return ctrl.Result{}, err
	}

	result, err := cc.ReconsileMarklogicClusterHandler()

	if err != nil {
		logger.Error(err, "Error reconciling marklogic cluster")
		return ctrl.Result{}, err
	}

	return result, nil
}

func markLogicClusterCreateUpdateDeletePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true // Reconcile on create
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			switch e.ObjectNew.(type) {
			case *marklogicv1.MarklogicCluster:
				oldAnnotations := e.ObjectOld.GetAnnotations()
				newAnnotations := e.ObjectNew.GetAnnotations()
				// delete(newAnnotations, "banzaicloud.com/last-applied")
				// delete(oldAnnotations, "banzaicloud.com/last-applied")
				// delete(newAnnotations, "kubectl.kubernetes.io/last-applied-configuration")
				// delete(oldAnnotations, "kubectl.kubernetes.io/last-applied-configuration")
				if !reflect.DeepEqual(oldAnnotations, newAnnotations) {
					return true // Reconcile if annotations have changed
				}
				oldLables := e.ObjectOld.GetLabels()
				newLabels := e.ObjectNew.GetLabels()
				if !reflect.DeepEqual(oldLables, newLabels) {
					return true // Reconcile if labels have changed
				}
				// If annotations and labels are the same, check if the spec has changed
				oldObj := e.ObjectOld.(*marklogicv1.MarklogicGroup)
				// Check if the spec has changed
				newObj := e.ObjectNew.(*marklogicv1.MarklogicGroup)
				if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
					return true // Reconcile if spec has changed
				}
			default:
				return false // Ignore updates for other types
			}
			return false // Reconcile on update of MarklogicCluster

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
func (r *MarklogicClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&marklogicv1.MarklogicCluster{}).
		WithEventFilter(markLogicClusterCreateUpdateDeletePredicate()).
		Owns(&marklogicv1.MarklogicGroup{}).
		Complete(r)
}
