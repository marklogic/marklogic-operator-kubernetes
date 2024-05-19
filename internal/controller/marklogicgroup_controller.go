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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/k8sutil"
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

//+kubebuilder:rbac:groups=operator.marklogic.com,resources=marklogicgroups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operator.marklogic.com,resources=marklogicgroups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=operator.marklogic.com,resources=marklogicgroups/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets;replicasets;deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods;services,verbs=get;list;watch;create;update;patch;delete

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

	// logger.Info("Operator Status: ", "Conditions", &operatorCR.Status.Conditions)
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

	result, err := oc.ReconsileHandler()
	if err != nil {
		logger.Error(err, "Error reconciling statefulset")
		return ctrl.Result{}, err
	}

	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MarklogicGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {

	builder := ctrl.NewControllerManagedBy(mgr).
		Named("marklogicgroup-controller").
		For(&databasev1alpha1.MarklogicGroup{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{})

	return builder.Complete(r)
}
