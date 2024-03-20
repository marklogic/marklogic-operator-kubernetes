package k8sutil

import (
	operatorv1alpha1 "github.com/example/marklogic-operator/api/v1alpha1"
	"github.com/go-logr/logr"
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

func ReconcileMarkLogicCluster(cr *operatorv1alpha1.MarklogicCluster, index int) *operatorv1alpha1.MarklogicGroup {
	logger := MarkLogicServerLogger(cr.Namespace, cr.ObjectMeta.Name)
	logger.Info("ReconcileMarkLogicCluster")
	labels := getMarkLogicLabels(cr.ObjectMeta.Name)
	annotations := map[string]string{}
	objectMeta := generateObjectMeta(cr.ObjectMeta.Name, cr.Namespace, labels, annotations)
	objectMeta.Name = cr.Spec.MarkLogicGroups[index].Name
	params := generateMarkLogicServerParams(cr, index)
	ownerDef := marklogicClusterAsOwner(cr)
	MarkLogicServerDef := &operatorv1alpha1.MarklogicGroup{
		TypeMeta:   generateTypeMeta("MarklogicGroup", "operator.marklogic.com/v1alpha1"),
		ObjectMeta: objectMeta,
		Spec: operatorv1alpha1.MarklogicGroupSpec{
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

// func createMarkLogicServer(namespace string, markLogicServer *operatorv1alpha1.MarklogicGroup) error {
// 	logger := statefulSetLogger(namespace, markLogicServer.Name)
// 	_, err := generateK8sClient().OperatorV1alpha1().MarklogicServers(namespace).Create(context.Background(), markLogicServer, metav1.CreateOptions{})
// 	if err != nil {
// 		logger.Error(err, "MarkLogic stateful creation failed")
// 		return err
// 	}
// 	logger.Info("MarkLogic stateful successfully created")
// 	return nil
// }

func generateBootstrapHost(isBootstrap bool) string {
	if isBootstrap {
		return ""
	} else {
		return "dnode-0.dnode.default.svc.cluster.local"
	}
}

func generateMarkLogicServerParams(cr *operatorv1alpha1.MarklogicCluster, index int) markLogicServerParameters {
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
