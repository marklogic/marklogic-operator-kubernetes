package k8sutil

import (
	"github.com/go-logr/logr"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type markLogicServerParameters struct {
	Replicas *int32
	Name     string
	Image    string
	// PersistentVolumeClaim         corev1.PersistentVolumeClaim
	TerminationGracePeriodSeconds *int64
}

func MarkLogicServerLogger(namespace string, name string) logr.Logger {
	var log = log.Log.WithName("controller_marlogic")
	reqLogger := log.WithValues("Request.StatefulSet.Namespace", namespace, "Request.MarkLogicServer.Name", name)
	return reqLogger
}

func ReconcileMarkLogicCluster(cr *databasev1alpha1.MarklogicCluster, index int) *databasev1alpha1.MarklogicGroup {
	logger := MarkLogicServerLogger(cr.Namespace, cr.ObjectMeta.Name)
	logger.Info("ReconcileMarkLogicCluster")
	labels := getMarkLogicLabels(cr.ObjectMeta.Name)
	annotations := map[string]string{}
	objectMeta := generateObjectMeta(cr.ObjectMeta.Name, cr.Namespace, labels, annotations)
	objectMeta.Name = cr.Spec.MarkLogicGroups[index].Name
	params := generateMarkLogicServerParams(cr, index)
	ownerDef := marklogicClusterAsOwner(cr)
	MarkLogicServerDef := &databasev1alpha1.MarklogicGroup{
		TypeMeta:   generateTypeMeta("MarklogicGroup", "operator.marklogic.com/v1alpha1"),
		ObjectMeta: objectMeta,
		Spec: databasev1alpha1.MarklogicGroupSpec{
			Replicas:                      params.Replicas,
			Name:                          params.Name,
			Image:                         params.Image,
			TerminationGracePeriodSeconds: params.TerminationGracePeriodSeconds,
			BootstrapHost:                 generateBootstrapHost(cr.Spec.MarkLogicGroups[index].IsBootstrap),
		},
	}
	AddOwnerRefToObject(MarkLogicServerDef, ownerDef)
	return MarkLogicServerDef
}

func generateBootstrapHost(isBootstrap bool) string {
	if isBootstrap {
		return ""
	} else {
		return "dnode-0.dnode.default.svc.cluster.local"
	}
}

func generateMarkLogicServerParams(cr *databasev1alpha1.MarklogicCluster, index int) markLogicServerParameters {
	markLogicServerParameters := markLogicServerParameters{
		Replicas:                      cr.Spec.MarkLogicGroups[index].Replicas,
		Name:                          cr.Spec.MarkLogicGroups[index].Name,
		Image:                         cr.Spec.MarkLogicGroups[index].Image,
		TerminationGracePeriodSeconds: cr.Spec.MarkLogicGroups[index].TerminationGracePeriodSeconds,
	}
	// if cr.Spec.Storage != nil {
	// 	params.PersistentVolumeClaim = generatePVCTemplate(cr.Spec.Storage.Size)
	// }
	return markLogicServerParameters
}
