package k8sutil

import (
	"context"

	"github.com/go-logr/logr"
	operatorv1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
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
	MarklogicGroup *operatorv1alpha1.MarklogicGroup
	ReqLogger      logr.Logger
	Recorder       record.EventRecorder

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
	mls := &operatorv1alpha1.MarklogicGroup{}
	if err := retrieveMarkLogicServer(oc, request, mls); err != nil {
		oc.ReqLogger.Error(err, "Failed to retrieve MarkLogicServer")
		return nil, err
	}
	oc.MarklogicGroup = mls

	oc.ReqLogger.Info("==== CreateOperatorContext")

	oc.ReqLogger = oc.ReqLogger.WithValues("ML server name", mls.Spec.Name)
	log.IntoContext(ctx, oc.ReqLogger)

	return oc, nil
}

func retrieveMarkLogicServer(oc *OperatorContext, request *reconcile.Request, mls *operatorv1alpha1.MarklogicGroup) error {
	err := oc.Client.Get(oc.Ctx, request.NamespacedName, mls)

	return err
}

func (oc *OperatorContext) GetMarkLogicServer() *operatorv1alpha1.MarklogicGroup {
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
