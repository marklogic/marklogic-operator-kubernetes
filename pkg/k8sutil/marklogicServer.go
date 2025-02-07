package k8sutil

import (
	"fmt"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	"github.com/go-logr/logr"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type MarkLogicGroupParameters struct {
	Replicas                      *int32
	Name                          string
	GroupConfig                   *databasev1alpha1.GroupConfig
	Image                         string
	ImagePullPolicy               string
	ImagePullSecrets              []corev1.LocalObjectReference
	License                       *databasev1alpha1.License
	Service                       databasev1alpha1.Service
	Storage                       *databasev1alpha1.Storage
	Auth                          *databasev1alpha1.AdminAuth
	TerminationGracePeriodSeconds *int64
	Resources                     *corev1.ResourceRequirements
	EnableConverters              bool
	PriorityClassName             string
	ClusterDomain                 string
	UpdateStrategy                appsv1.StatefulSetUpdateStrategyType
	Affinity                      *corev1.Affinity
	NodeSelector                  map[string]string
	TopologySpreadConstraints     []corev1.TopologySpreadConstraint
	HugePages                     *databasev1alpha1.HugePages
	PodSecurityContext            *corev1.PodSecurityContext
	ContainerSecurityContext      *corev1.SecurityContext
	IsBootstrap                   bool
	LogCollection                 *databasev1alpha1.LogCollection
	PathBasedRouting              bool
	Tls                           *databasev1alpha1.Tls
	AdditionalVolumes             *[]corev1.Volume
	AdditionalVolumeMounts        *[]corev1.VolumeMount
}

type MarkLogicClusterParameters struct {
	Auth                          *databasev1alpha1.AdminAuth
	Replicas                      *int32
	Name                          string
	Image                         string
	ImagePullPolicy               string
	ImagePullSecrets              []corev1.LocalObjectReference
	ClusterDomain                 string
	Storage                       *databasev1alpha1.Storage
	License                       *databasev1alpha1.License
	Affinity                      *corev1.Affinity
	NodeSelector                  map[string]string
	TopologySpreadConstraints     []corev1.TopologySpreadConstraint
	PriorityClassName             string
	EnableConverters              bool
	Resources                     *corev1.ResourceRequirements
	HugePages                     *databasev1alpha1.HugePages
	LogCollection                 *databasev1alpha1.LogCollection
	PodSecurityContext            *corev1.PodSecurityContext
	ContainerSecurityContext      *corev1.SecurityContext
	PathBasedRouting              bool
	Tls                           *databasev1alpha1.Tls
	TerminationGracePeriodSeconds *int64
	AdditionalVolumes             *[]corev1.Volume
	AdditionalVolumeMounts        *[]corev1.VolumeMount
}

func MarkLogicGroupLogger(namespace string, name string) logr.Logger {
	var log = log.Log.WithName("controller_marlogic")
	reqLogger := log.WithValues("Request.StatefulSet.Namespace", namespace, "Request.MarkLogicGroup.Name", name)
	return reqLogger
}

func GenerateMarkLogicGroupDef(cr *databasev1alpha1.MarklogicCluster, index int, params *MarkLogicGroupParameters) *databasev1alpha1.MarklogicGroup {
	logger := MarkLogicGroupLogger(cr.Namespace, cr.ObjectMeta.Name)
	logger.Info("ReconcileMarkLogicCluster")
	labels := getMarkLogicLabels(cr.ObjectMeta.Name)
	annotations := map[string]string{}
	objectMeta := generateObjectMeta(cr.Spec.MarkLogicGroups[index].Name, cr.Namespace, labels, annotations)
	bootStrapHostName := ""
	bootStrapName := ""
	for _, group := range cr.Spec.MarkLogicGroups {
		if group.IsBootstrap {
			bootStrapName = group.Name
		}
	}
	if !cr.Spec.MarkLogicGroups[index].IsBootstrap {
		nsName := cr.ObjectMeta.Namespace
		clusterName := cr.Spec.ClusterDomain
		bootStrapHostName = fmt.Sprintf("%s-0.%s.%s.svc.%s", bootStrapName, bootStrapName, nsName, clusterName)
	}
	ownerDef := marklogicClusterAsOwner(cr)
	MarkLogicGroupDef := &databasev1alpha1.MarklogicGroup{
		TypeMeta:   generateTypeMeta("MarklogicGroup", "operator.marklogic.com/v1alpha1"),
		ObjectMeta: objectMeta,
		Spec: databasev1alpha1.MarklogicGroupSpec{
			Replicas:                      params.Replicas,
			Name:                          params.Name,
			GroupConfig:                   params.GroupConfig,
			Auth:                          params.Auth,
			Image:                         params.Image,
			ImagePullSecrets:              params.ImagePullSecrets,
			License:                       params.License,
			TerminationGracePeriodSeconds: params.TerminationGracePeriodSeconds,
			BootstrapHost:                 bootStrapHostName,
			Resources:                     params.Resources,
			EnableConverters:              params.EnableConverters,
			PriorityClassName:             params.PriorityClassName,
			ClusterDomain:                 params.ClusterDomain,
			UpdateStrategy:                params.UpdateStrategy,
			Affinity:                      params.Affinity,
			NodeSelector:                  params.NodeSelector,
			Storage:                       params.Storage,
			Service:                       params.Service,
			LogCollection:                 params.LogCollection,
			TopologySpreadConstraints:     params.TopologySpreadConstraints,
			PodSecurityContext:            params.PodSecurityContext,
			ContainerSecurityContext:      params.ContainerSecurityContext,
			PathBasedRouting:              params.PathBasedRouting,
			Tls:                           params.Tls,
			AdditionalVolumes:             params.AdditionalVolumes,
			AdditionalVolumeMounts:        params.AdditionalVolumeMounts,
		},
	}
	AddOwnerRefToObject(MarkLogicGroupDef, ownerDef)
	return MarkLogicGroupDef
}

func (cc *ClusterContext) ReconsileMarklogicCluster() (reconcile.Result, error) {
	operatorCR := cc.GetMarkLogicCluster()
	logger := cc.ReqLogger
	ctx := cc.Ctx
	total := len(operatorCR.Spec.MarkLogicGroups)
	logger.Info("===== Total Count ==== ", "Count:", total)
	cr := cc.MarklogicCluster

	for i := 0; i < total; i++ {
		logger.Info("ReconcileCluster", "Count", i)
		currentMlg := &databasev1alpha1.MarklogicGroup{}
		namespace := cr.Namespace
		name := cr.Spec.MarkLogicGroups[i].Name
		namespacedName := types.NamespacedName{Name: name, Namespace: namespace}
		clusterParams := generateMarkLogicClusterParams(cr)
		params := generateMarkLogicGroupParams(cr, i, clusterParams)
		logger.Info("!!! ReconcileCluster MarkLogicGroup", "MarkLogicGroupParams", params)
		markLogicGroupDef := GenerateMarkLogicGroupDef(operatorCR, i, params)
		err := cc.Client.Get(cc.Ctx, namespacedName, currentMlg)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("MarkLogicGroup resource not found. Creating a new one")

				err = cc.Client.Create(ctx, markLogicGroupDef)
				if err != nil {
					logger.Error(err, "Failed to create markLogicCluster")
				}

				logger.Info("Created new MarkLogic Server resource")
				result.Done().Output()
			} else {
				logger.Error(err, "Failed to get MarkLogicGroup resource")
			}
		} else {
			patchDiff, err := patch.DefaultPatchMaker.Calculate(currentMlg, markLogicGroupDef,
				patch.IgnoreStatusFields(),
				patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
				patch.IgnoreField("kind"))

			if err != nil {
				logger.Error(err, "Error calculating patch")
				return result.Error(err).Output()
			}
			if !patchDiff.IsEmpty() {
				logger.Info("MarkLogic statefulSet spec is different from the MarkLogicGroup spec, updating the statefulSet")
				logger.Info(patchDiff.String())
				currentMlg.Spec = markLogicGroupDef.Spec
				err := cc.Client.Update(cc.Ctx, currentMlg)
				if err != nil {
					logger.Error(err, "Error updating MakrLogicGroup")
					return result.Error(err).Output()
				}
			} else {
				logger.Info("MarkLogic statefulSet spec is the same as the MarkLogicGroup spec")
			}
		}

	}
	return result.Done().Output()
}

func generateMarkLogicClusterParams(cr *databasev1alpha1.MarklogicCluster) *MarkLogicClusterParameters {
	markLogicClusterParameters := &MarkLogicClusterParameters{
		Name:                          cr.ObjectMeta.Name,
		Image:                         cr.Spec.Image,
		ImagePullPolicy:               cr.Spec.ImagePullPolicy,
		ImagePullSecrets:              cr.Spec.ImagePullSecrets,
		ClusterDomain:                 cr.Spec.ClusterDomain,
		Storage:                       cr.Spec.Storage,
		Affinity:                      cr.Spec.Affinity,
		NodeSelector:                  cr.Spec.NodeSelector,
		TopologySpreadConstraints:     cr.Spec.TopologySpreadConstraints,
		PriorityClassName:             cr.Spec.PriorityClassName,
		License:                       cr.Spec.License,
		EnableConverters:              cr.Spec.EnableConverters,
		Resources:                     cr.Spec.Resources,
		HugePages:                     cr.Spec.HugePages,
		LogCollection:                 cr.Spec.LogCollection,
		Auth:                          cr.Spec.Auth,
		PodSecurityContext:            cr.Spec.PodSecurityContext,
		ContainerSecurityContext:      cr.Spec.ContainerSecurityContext,
		Tls:                           cr.Spec.Tls,
		TerminationGracePeriodSeconds: cr.Spec.TerminationGracePeriodSeconds,
		AdditionalVolumes:             cr.Spec.AdditionalVolumes,
		AdditionalVolumeMounts:        cr.Spec.AdditionalVolumeMounts,
	}
	if cr.Spec.HAProxy == nil || cr.Spec.HAProxy.PathBasedRouting == nil || !cr.Spec.HAProxy.Enabled || !*cr.Spec.HAProxy.PathBasedRouting {
		markLogicClusterParameters.PathBasedRouting = false
	} else {
		markLogicClusterParameters.PathBasedRouting = true
	}

	return markLogicClusterParameters
}

func generateMarkLogicGroupParams(cr *databasev1alpha1.MarklogicCluster, index int, clusterParams *MarkLogicClusterParameters) *MarkLogicGroupParameters {
	MarkLogicGroupParameters := &MarkLogicGroupParameters{
		Replicas:                      cr.Spec.MarkLogicGroups[index].Replicas,
		Name:                          cr.Spec.MarkLogicGroups[index].Name,
		GroupConfig:                   cr.Spec.MarkLogicGroups[index].GroupConfig,
		Service:                       cr.Spec.MarkLogicGroups[index].Service,
		Image:                         clusterParams.Image,
		ImagePullPolicy:               clusterParams.ImagePullPolicy,
		ImagePullSecrets:              clusterParams.ImagePullSecrets,
		Auth:                          clusterParams.Auth,
		License:                       clusterParams.License,
		Storage:                       clusterParams.Storage,
		TerminationGracePeriodSeconds: clusterParams.TerminationGracePeriodSeconds,
		Resources:                     clusterParams.Resources,
		EnableConverters:              clusterParams.EnableConverters,
		PriorityClassName:             clusterParams.PriorityClassName,
		ClusterDomain:                 clusterParams.ClusterDomain,
		Affinity:                      clusterParams.Affinity,
		NodeSelector:                  clusterParams.NodeSelector,
		TopologySpreadConstraints:     clusterParams.TopologySpreadConstraints,
		HugePages:                     clusterParams.HugePages,
		PodSecurityContext:            clusterParams.PodSecurityContext,
		ContainerSecurityContext:      clusterParams.ContainerSecurityContext,
		IsBootstrap:                   cr.Spec.MarkLogicGroups[index].IsBootstrap,
		LogCollection:                 clusterParams.LogCollection,
		PathBasedRouting:              clusterParams.PathBasedRouting,
		Tls:                           clusterParams.Tls,
		AdditionalVolumeMounts:        clusterParams.AdditionalVolumeMounts,
		AdditionalVolumes:             clusterParams.AdditionalVolumes,
	}

	if cr.Spec.MarkLogicGroups[index].HAProxy != nil && cr.Spec.MarkLogicGroups[index].HAProxy.PathBasedRouting != nil {
		MarkLogicGroupParameters.PathBasedRouting = *cr.Spec.MarkLogicGroups[index].HAProxy.PathBasedRouting
	}
	if cr.Spec.MarkLogicGroups[index].Image != "" {
		MarkLogicGroupParameters.Image = cr.Spec.MarkLogicGroups[index].Image
	}
	if cr.Spec.MarkLogicGroups[index].ImagePullPolicy != "" {
		MarkLogicGroupParameters.ImagePullPolicy = cr.Spec.MarkLogicGroups[index].ImagePullPolicy
	}
	if cr.Spec.MarkLogicGroups[index].ImagePullSecrets != nil {
		MarkLogicGroupParameters.ImagePullSecrets = cr.Spec.MarkLogicGroups[index].ImagePullSecrets
	}
	if cr.Spec.MarkLogicGroups[index].Storage != nil {
		MarkLogicGroupParameters.Storage = cr.Spec.MarkLogicGroups[index].Storage
	}
	if cr.Spec.MarkLogicGroups[index].Resources != nil {
		MarkLogicGroupParameters.Resources = cr.Spec.MarkLogicGroups[index].Resources
	}
	if cr.Spec.MarkLogicGroups[index].Affinity != nil {
		MarkLogicGroupParameters.Affinity = cr.Spec.MarkLogicGroups[index].Affinity
	}
	if cr.Spec.MarkLogicGroups[index].NodeSelector != nil {
		MarkLogicGroupParameters.NodeSelector = cr.Spec.MarkLogicGroups[index].NodeSelector
	}
	if cr.Spec.MarkLogicGroups[index].TopologySpreadConstraints != nil {
		MarkLogicGroupParameters.TopologySpreadConstraints = cr.Spec.MarkLogicGroups[index].TopologySpreadConstraints
	}
	if cr.Spec.MarkLogicGroups[index].PriorityClassName != "" {
		MarkLogicGroupParameters.PriorityClassName = cr.Spec.MarkLogicGroups[index].PriorityClassName
	}
	if cr.Spec.MarkLogicGroups[index].HugePages != nil {
		MarkLogicGroupParameters.HugePages = cr.Spec.MarkLogicGroups[index].HugePages
	}
	if cr.Spec.MarkLogicGroups[index].LogCollection != nil {
		MarkLogicGroupParameters.LogCollection = cr.Spec.MarkLogicGroups[index].LogCollection
	}
	if cr.Spec.MarkLogicGroups[index].Tls != nil {
		MarkLogicGroupParameters.Tls = cr.Spec.MarkLogicGroups[index].Tls
	}
	if cr.Spec.MarkLogicGroups[index].AdditionalVolumes != nil {
		MarkLogicGroupParameters.AdditionalVolumes = cr.Spec.MarkLogicGroups[index].AdditionalVolumes
	}
	if cr.Spec.MarkLogicGroups[index].AdditionalVolumeMounts != nil {
		MarkLogicGroupParameters.AdditionalVolumeMounts = cr.Spec.MarkLogicGroups[index].AdditionalVolumeMounts
	}
	return MarkLogicGroupParameters
}
