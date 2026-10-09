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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"reflect"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/k8sutil"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// objectStorageSecretIndex indexes clusters by the Secret names their object storage providers reference.
const objectStorageSecretIndex = ".spec.objectStorage.secretNames"

// trackingAnnotations change on every apply and must not trigger reconciliation.
var trackingAnnotations = []string{"banzaicloud.com/last-applied", "kubectl.kubernetes.io/last-applied-configuration"}

// MarklogicClusterReconciler reconciles a MarklogicCluster object
type MarklogicClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Log      logr.Logger
	Recorder record.EventRecorder

	// APIReader reads uncached; nil falls back to the cached client.
	APIReader client.Reader
	// CredentialClientFactory builds the object storage credential client; nil uses the default.
	CredentialClientFactory func(mlmanage.ClientOptions) mlmanage.CredentialClient
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

	cc.APIReader = r.APIReader
	cc.CredentialClientFactory = r.CredentialClientFactory

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
				if !reflect.DeepEqual(comparableAnnotations(e.ObjectOld), comparableAnnotations(e.ObjectNew)) {
					return true // Reconcile if annotations have changed
				}
				oldLables := e.ObjectOld.GetLabels()
				newLabels := e.ObjectNew.GetLabels()
				if !reflect.DeepEqual(oldLables, newLabels) {
					return true // Reconcile if labels have changed
				}
				// If annotations and labels are the same, check if the spec has changed
				oldObj := e.ObjectOld.(*marklogicv1.MarklogicCluster)
				// Check if the spec has changed
				newObj := e.ObjectNew.(*marklogicv1.MarklogicCluster)
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

// comparableAnnotations copies the annotations without the tracking keys; informer objects are never modified.
func comparableAnnotations(obj client.Object) map[string]string {
	annotations := make(map[string]string, len(obj.GetAnnotations()))
	for key, value := range obj.GetAnnotations() {
		annotations[key] = value
	}
	for _, key := range trackingAnnotations {
		delete(annotations, key)
	}
	return annotations
}

// secretRevisionPredicate admits every referenced Secret revision, including metadata-only updates.
func secretRevisionPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetResourceVersion() != e.ObjectNew.GetResourceVersion()
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// objectStorageSecretNames lists the unique Secret names a cluster's object storage providers reference.
func objectStorageSecretNames(obj client.Object) []string {
	cluster, ok := obj.(*marklogicv1.MarklogicCluster)
	if !ok || cluster.Spec.ObjectStorage == nil {
		return nil
	}
	unique := map[string]struct{}{}
	if aws := cluster.Spec.ObjectStorage.AWS; aws != nil && strings.TrimSpace(aws.SecretName) != "" {
		unique[strings.TrimSpace(aws.SecretName)] = struct{}{}
	}
	if azure := cluster.Spec.ObjectStorage.Azure; azure != nil && strings.TrimSpace(azure.SecretName) != "" {
		unique[strings.TrimSpace(azure.SecretName)] = struct{}{}
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// secretToClusters enqueues each cluster in the Secret's namespace that references it.
func (r *MarklogicClusterReconciler) secretToClusters(ctx context.Context, obj client.Object) []reconcile.Request {
	clusters := &marklogicv1.MarklogicClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(obj.GetNamespace()), client.MatchingFields{objectStorageSecretIndex: obj.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to map Secret to MarklogicClusters")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(clusters.Items))
	for i := range clusters.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&clusters.Items[i])})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *MarklogicClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &marklogicv1.MarklogicCluster{}, objectStorageSecretIndex, objectStorageSecretNames); err != nil {
		return err
	}
	clusterPredicates := builder.WithPredicates(markLogicClusterCreateUpdateDeletePredicate())
	return ctrl.NewControllerManagedBy(mgr).
		For(&marklogicv1.MarklogicCluster{}, clusterPredicates).
		Owns(&marklogicv1.MarklogicGroup{}, clusterPredicates).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToClusters), builder.WithPredicates(secretRevisionPredicate())).
		Complete(r)
}
