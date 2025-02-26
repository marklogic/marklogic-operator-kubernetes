package k8sutil

import (
	"context"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	controllerClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type OperatorContext struct {
	Ctx context.Context

	Request        *reconcile.Request
	Client         controllerClient.Client
	Scheme         *runtime.Scheme
	MarklogicGroup *marklogicv1.MarklogicGroup
	ReqLogger      logr.Logger
	Recorder       record.EventRecorder

	Services     []*corev1.Service
	StatefulSets []*appsv1.StatefulSet
}

type ClusterContext struct {
	Ctx context.Context

	Request          *reconcile.Request
	Client           controllerClient.Client
	Scheme           *runtime.Scheme
	MarklogicCluster *marklogicv1.MarklogicCluster
	ReqLogger        logr.Logger
	Recorder         record.EventRecorder

	Services     []*corev1.Service
	StatefulSets []*appsv1.StatefulSet
}

func CreateOperatorContext(
	ctx context.Context,
	request *reconcile.Request,
	client controllerClient.Client,
	scheme *runtime.Scheme,
	rec record.EventRecorder) (*OperatorContext, error) {

	oc := &OperatorContext{}
	reqLogger := log.FromContext(ctx)
	oc.Ctx = ctx
	oc.Request = request
	oc.Client = client
	oc.Scheme = scheme
	oc.ReqLogger = reqLogger
	oc.Recorder = rec
	mlg := &marklogicv1.MarklogicGroup{}
	if err := retrieveMarkLogicGroup(oc, request, mlg); err != nil {
		oc.ReqLogger.Error(err, "Failed to retrieve MarkLogicServer")
		return nil, err
	}
	// if err := retrieveMarklogicCluster(oc, request, mlc); err != nil {
	// 	oc.ReqLogger.Error(err, "Failed to retrieve MarkLogicCluster")
	// 	return nil, err
	// }
	oc.MarklogicGroup = mlg
	// oc.MarklogicCluster = mlc

	oc.ReqLogger.Info("==== CreateOperatorContext")

	oc.ReqLogger = oc.ReqLogger.WithValues("ML server name", mlg.Spec.Name)
	log.IntoContext(ctx, oc.ReqLogger)

	return oc, nil
}

func CreateClusterContext(
	ctx context.Context,
	request *reconcile.Request,
	client controllerClient.Client,
	scheme *runtime.Scheme,
	rec record.EventRecorder) (*ClusterContext, error) {

	cc := &ClusterContext{}
	reqLogger := log.FromContext(ctx)
	cc.Ctx = ctx
	cc.Request = request
	cc.Client = client
	cc.Scheme = scheme
	cc.ReqLogger = reqLogger
	cc.Recorder = rec
	mlc := &marklogicv1.MarklogicCluster{}

	if err := retrieveMarklogicCluster(cc, request, mlc); err != nil {
		cc.ReqLogger.Error(err, "Failed to retrieve MarkLogicCluster")
		return nil, err
	}
	cc.MarklogicCluster = mlc

	cc.ReqLogger.Info("==== CreateOperatorContext")

	// cc.ReqLogger = cc.ReqLogger.WithValues("ML server name")
	log.IntoContext(ctx, cc.ReqLogger)

	return cc, nil
}

func retrieveMarkLogicGroup(oc *OperatorContext, request *reconcile.Request, mlg *marklogicv1.MarklogicGroup) error {
	err := oc.Client.Get(oc.Ctx, request.NamespacedName, mlg)
	return err
}

func retrieveMarklogicCluster(cc *ClusterContext, request *reconcile.Request, mlc *marklogicv1.MarklogicCluster) error {
	err := cc.Client.Get(cc.Ctx, request.NamespacedName, mlc)
	return err
}

func (cc *ClusterContext) GetMarkLogicCluster() *marklogicv1.MarklogicCluster {
	return cc.MarklogicCluster
}

func (oc *OperatorContext) GetMarkLogicServer() *marklogicv1.MarklogicGroup {
	return oc.MarklogicGroup
}

func (oc *OperatorContext) GetLogger() logr.Logger {
	return oc.ReqLogger
}

func (oc *OperatorContext) GetClient() controllerClient.Client {
	return oc.Client
}

func (oc *OperatorContext) GetContext() context.Context {
	return oc.Ctx
}
