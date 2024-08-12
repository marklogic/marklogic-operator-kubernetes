package k8sutil

import (
	"github.com/go-logr/logr"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type markLogicServerParameters struct {
	Replicas *int32
	Name     string
	Image    string
	License  *databasev1alpha1.License
	// PersistentVolumeClaim         corev1.PersistentVolumeClaim
	TerminationGracePeriodSeconds *int64
	Resources                     *corev1.ResourceRequirements
	EnableConverters              bool
	PriorityClassName             string
	ClusterDomain                 string
	UpdateStrategy                appsv1.StatefulSetUpdateStrategyType
	Affinity                      *corev1.Affinity
	NodeSelector                  map[string]string
	TopologySpreadConstraints     []corev1.TopologySpreadConstraint
	PodSecurityContext            *corev1.PodSecurityContext
	ContainerSecurityContext      *corev1.SecurityContext
	NetworkPolicy                 *networkingv1.NetworkPolicy
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
			License:                       params.License,
			TerminationGracePeriodSeconds: params.TerminationGracePeriodSeconds,
			BootstrapHost:                 generateBootstrapHost(cr.Spec.MarkLogicGroups[index].IsBootstrap),
			Resources:                     params.Resources,
			EnableConverters:              params.EnableConverters,
			PriorityClassName:             params.PriorityClassName,
			ClusterDomain:                 params.ClusterDomain,
			UpdateStrategy:                params.UpdateStrategy,
			Affinity:                      params.Affinity,
			NodeSelector:                  params.NodeSelector,
			TopologySpreadConstraints:     params.TopologySpreadConstraints,
			PodSecurityContext:            params.PodSecurityContext,
			ContainerSecurityContext:      params.ContainerSecurityContext,
			NetworkPolicy:                 params.NetworkPolicy,
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
		License:                       cr.Spec.MarkLogicGroups[index].License,
		TerminationGracePeriodSeconds: cr.Spec.MarkLogicGroups[index].TerminationGracePeriodSeconds,
		Resources:                     cr.Spec.MarkLogicGroups[index].Resources,
		EnableConverters:              cr.Spec.MarkLogicGroups[index].EnableConverters,
		PriorityClassName:             cr.Spec.MarkLogicGroups[index].PriorityClassName,
		ClusterDomain:                 cr.Spec.MarkLogicGroups[index].ClusterDomain,
		UpdateStrategy:                cr.Spec.MarkLogicGroups[index].UpdateStrategy,
		Affinity:                      cr.Spec.MarkLogicGroups[index].Affinity,
		NodeSelector:                  cr.Spec.MarkLogicGroups[index].NodeSelector,
		TopologySpreadConstraints:     cr.Spec.MarkLogicGroups[index].TopologySpreadConstraints,
		PodSecurityContext:            cr.Spec.MarkLogicGroups[index].PodSecurityContext,
		ContainerSecurityContext:      cr.Spec.MarkLogicGroups[index].ContainerSecurityContext,
		NetworkPolicy:                 cr.Spec.MarkLogicGroups[index].NetworkPolicy,
	}
	// if cr.Spec.Storage != nil {
	// 	params.PersistentVolumeClaim = generatePVCTemplate(cr.Spec.Storage.Size)
	// }
	return markLogicServerParameters
}
