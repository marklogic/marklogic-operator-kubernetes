package k8sutil

import (
	"fmt"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type MarkLogicGroupParameters struct {
	Replicas                       *int32
	Name                           string
	ServiceAccountName             string
	AutomountServiceAccountToken   *bool
	Labels                         map[string]string
	Annotations                    map[string]string
	GroupConfig                    *marklogicv1.GroupConfig
	Image                          string
	ImagePullPolicy                string
	ImagePullSecrets               []corev1.LocalObjectReference
	License                        *marklogicv1.License
	Service                        marklogicv1.Service
	Persistence                    *marklogicv1.Persistence
	Auth                           *marklogicv1.AdminAuth
	TerminationGracePeriodSeconds  *int64
	Resources                      *corev1.ResourceRequirements
	EnableConverters               bool
	PriorityClassName              string
	ClusterDomain                  string
	UpdateStrategy                 appsv1.StatefulSetUpdateStrategyType
	Affinity                       *corev1.Affinity
	NodeSelector                   map[string]string
	TopologySpreadConstraints      []corev1.TopologySpreadConstraint
	HugePages                      *marklogicv1.HugePages
	PodSecurityContext             *corev1.PodSecurityContext
	ContainerSecurityContext       *corev1.SecurityContext
	IsBootstrap                    bool
	LogCollection                  *marklogicv1.LogCollection
	PathBasedRouting               bool
	Tls                            *marklogicv1.Tls
	AdditionalVolumes              *[]corev1.Volume
	AdditionalVolumeMounts         *[]corev1.VolumeMount
	SecretName                     string
	AdditionalVolumeClaimTemplates *[]corev1.PersistentVolumeClaim
}

type MarkLogicClusterParameters struct {
	Auth                           *marklogicv1.AdminAuth
	Replicas                       *int32
	Name                           string
	ServiceAccountName             string
	Image                          string
	ImagePullPolicy                string
	ImagePullSecrets               []corev1.LocalObjectReference
	ClusterDomain                  string
	Persistence                    *marklogicv1.Persistence
	License                        *marklogicv1.License
	Affinity                       *corev1.Affinity
	NodeSelector                   map[string]string
	TopologySpreadConstraints      []corev1.TopologySpreadConstraint
	PriorityClassName              string
	EnableConverters               bool
	Resources                      *corev1.ResourceRequirements
	HugePages                      *marklogicv1.HugePages
	LogCollection                  *marklogicv1.LogCollection
	PodSecurityContext             *corev1.PodSecurityContext
	ContainerSecurityContext       *corev1.SecurityContext
	PathBasedRouting               bool
	Tls                            *marklogicv1.Tls
	TerminationGracePeriodSeconds  *int64
	AdditionalVolumes              *[]corev1.Volume
	AdditionalVolumeMounts         *[]corev1.VolumeMount
	AdditionalVolumeClaimTemplates *[]corev1.PersistentVolumeClaim
}

func MarkLogicGroupLogger(namespace string, name string) logr.Logger {
	var log = log.Log.WithName("controller_marlogic")
	reqLogger := log.WithValues("Request.StatefulSet.Namespace", namespace, "Request.MarkLogicGroup.Name", name)
	return reqLogger
}

func (cc *ClusterContext) GenerateMarkLogicGroupDef(cr *marklogicv1.MarklogicCluster, index int, params *MarkLogicGroupParameters) *marklogicv1.MarklogicGroup {
	logger := MarkLogicGroupLogger(cr.Namespace, cr.ObjectMeta.Name)
	logger.Info("ReconcileMarkLogicCluster")
	labels := cc.GetClusterLabels(cr.ObjectMeta.Name)
	annotations := cc.GetClusterAnnotations()
	if params.Labels != nil {
		for key, value := range params.Labels {
			labels[key] = value
		}
	}
	if params.Annotations != nil {
		for key, value := range params.Annotations {
			annotations[key] = value
		}
	}
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
	MarkLogicGroupDef := &marklogicv1.MarklogicGroup{
		TypeMeta:   generateTypeMeta("MarklogicGroup", "marklogic.progress.com/v1"),
		ObjectMeta: objectMeta,
		Spec: marklogicv1.MarklogicGroupSpec{
			Replicas:                       params.Replicas,
			Name:                           params.Name,
			GroupConfig:                    params.GroupConfig,
			Auth:                           params.Auth,
			ServiceAccountName:             params.ServiceAccountName,
			AutomountServiceAccountToken:   params.AutomountServiceAccountToken,
			Image:                          params.Image,
			Labels:                         params.Labels,
			Annotations:                    params.Annotations,
			ImagePullSecrets:               params.ImagePullSecrets,
			License:                        params.License,
			TerminationGracePeriodSeconds:  params.TerminationGracePeriodSeconds,
			BootstrapHost:                  bootStrapHostName,
			Resources:                      params.Resources,
			EnableConverters:               params.EnableConverters,
			PriorityClassName:              params.PriorityClassName,
			ClusterDomain:                  params.ClusterDomain,
			UpdateStrategy:                 params.UpdateStrategy,
			Affinity:                       params.Affinity,
			NodeSelector:                   params.NodeSelector,
			Persistence:                    params.Persistence,
			Service:                        params.Service,
			LogCollection:                  params.LogCollection,
			TopologySpreadConstraints:      params.TopologySpreadConstraints,
			PodSecurityContext:             params.PodSecurityContext,
			ContainerSecurityContext:       params.ContainerSecurityContext,
			PathBasedRouting:               params.PathBasedRouting,
			Tls:                            params.Tls,
			AdditionalVolumes:              params.AdditionalVolumes,
			AdditionalVolumeMounts:         params.AdditionalVolumeMounts,
			SecretName:                     params.SecretName,
			AdditionalVolumeClaimTemplates: params.AdditionalVolumeClaimTemplates,
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
		currentMlg := &marklogicv1.MarklogicGroup{}
		namespace := cr.Namespace
		name := cr.Spec.MarkLogicGroups[i].Name
		namespacedName := types.NamespacedName{Name: name, Namespace: namespace}
		clusterParams := generateMarkLogicClusterParams(cr)
		params := generateMarkLogicGroupParams(cr, i, clusterParams)
		markLogicGroupDef := cc.GenerateMarkLogicGroupDef(operatorCR, i, params)
		err := cc.Client.Get(cc.Ctx, namespacedName, currentMlg)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("MarkLogicGroup resource not found. Creating a new one")
				if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(markLogicGroupDef); err != nil {
					logger.Error(err, "Failed to set last applied annotation")
				}
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
				logger.Info("MarkLogicGroup spec is different from the previous spec, updating the markLogicGroup")
				// currentMlg.Spec = markLogicGroupDef.Spec
				// currentMlg.ObjectMeta.Labels = markLogicGroupDef.ObjectMeta.Labels
				// currentMlg.ObjectMeta.Annotations = markLogicGroupDef.ObjectMeta.Annotations
				markLogicGroupDef.ObjectMeta.ResourceVersion = currentMlg.ObjectMeta.ResourceVersion
				if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(markLogicGroupDef); err != nil {
					logger.Error(err, "Failed to set last applied annotation")
				}
				err := cc.Client.Update(cc.Ctx, markLogicGroupDef)
				if err != nil {
					logger.Error(err, "Error updating MarklogicGroup")
					return result.Error(err).Output()
				}
			} else {
				logger.Info("MarkLogicGroup spec is same as the current spec, no update required")
			}
		}

	}
	return result.Done().Output()
}

func generateMarkLogicClusterParams(cr *marklogicv1.MarklogicCluster) *MarkLogicClusterParameters {
	markLogicClusterParameters := &MarkLogicClusterParameters{
		Name:                           cr.ObjectMeta.Name,
		Image:                          cr.Spec.Image,
		ImagePullPolicy:                cr.Spec.ImagePullPolicy,
		ImagePullSecrets:               cr.Spec.ImagePullSecrets,
		ServiceAccountName:             cr.Spec.ServiceAccountName,
		ClusterDomain:                  cr.Spec.ClusterDomain,
		Persistence:                    cr.Spec.Persistence,
		Affinity:                       cr.Spec.Affinity,
		NodeSelector:                   cr.Spec.NodeSelector,
		TopologySpreadConstraints:      cr.Spec.TopologySpreadConstraints,
		PriorityClassName:              cr.Spec.PriorityClassName,
		License:                        cr.Spec.License,
		EnableConverters:               cr.Spec.EnableConverters,
		Resources:                      cr.Spec.Resources,
		HugePages:                      cr.Spec.HugePages,
		LogCollection:                  cr.Spec.LogCollection,
		Auth:                           cr.Spec.Auth,
		PodSecurityContext:             cr.Spec.PodSecurityContext,
		ContainerSecurityContext:       cr.Spec.ContainerSecurityContext,
		Tls:                            cr.Spec.Tls,
		TerminationGracePeriodSeconds:  cr.Spec.TerminationGracePeriodSeconds,
		AdditionalVolumes:              cr.Spec.AdditionalVolumes,
		AdditionalVolumeMounts:         cr.Spec.AdditionalVolumeMounts,
		AdditionalVolumeClaimTemplates: cr.Spec.AdditionalVolumeClaimTemplates,
	}

	if cr.Spec.HAProxy == nil || cr.Spec.HAProxy.PathBasedRouting == nil || !cr.Spec.HAProxy.Enabled || !*cr.Spec.HAProxy.PathBasedRouting {
		markLogicClusterParameters.PathBasedRouting = false
	} else {
		markLogicClusterParameters.PathBasedRouting = true
	}

	return markLogicClusterParameters
}

func generateMarkLogicGroupParams(cr *marklogicv1.MarklogicCluster, index int, clusterParams *MarkLogicClusterParameters) *MarkLogicGroupParameters {
	// Always enforce automountServiceAccountToken to false for security
	falseValue := false

	markLogicGroupParameters := &MarkLogicGroupParameters{
		Replicas:                       cr.Spec.MarkLogicGroups[index].Replicas,
		Name:                           cr.Spec.MarkLogicGroups[index].Name,
		Labels:                         cr.Spec.MarkLogicGroups[index].Labels,
		Annotations:                    cr.Spec.MarkLogicGroups[index].Annotations,
		GroupConfig:                    cr.Spec.MarkLogicGroups[index].GroupConfig,
		Service:                        cr.Spec.MarkLogicGroups[index].Service,
		Image:                          clusterParams.Image,
		ImagePullPolicy:                clusterParams.ImagePullPolicy,
		ImagePullSecrets:               clusterParams.ImagePullSecrets,
		Auth:                           clusterParams.Auth,
		ServiceAccountName:             clusterParams.ServiceAccountName,
		AutomountServiceAccountToken:   &falseValue, // Always false for security
		License:                        clusterParams.License,
		Persistence:                    clusterParams.Persistence,
		TerminationGracePeriodSeconds:  clusterParams.TerminationGracePeriodSeconds,
		Resources:                      clusterParams.Resources,
		EnableConverters:               clusterParams.EnableConverters,
		PriorityClassName:              clusterParams.PriorityClassName,
		ClusterDomain:                  clusterParams.ClusterDomain,
		Affinity:                       clusterParams.Affinity,
		NodeSelector:                   clusterParams.NodeSelector,
		TopologySpreadConstraints:      clusterParams.TopologySpreadConstraints,
		HugePages:                      clusterParams.HugePages,
		PodSecurityContext:             clusterParams.PodSecurityContext,
		ContainerSecurityContext:       clusterParams.ContainerSecurityContext,
		IsBootstrap:                    cr.Spec.MarkLogicGroups[index].IsBootstrap,
		LogCollection:                  clusterParams.LogCollection,
		PathBasedRouting:               clusterParams.PathBasedRouting,
		Tls:                            clusterParams.Tls,
		AdditionalVolumeMounts:         clusterParams.AdditionalVolumeMounts,
		AdditionalVolumes:              clusterParams.AdditionalVolumes,
		AdditionalVolumeClaimTemplates: clusterParams.AdditionalVolumeClaimTemplates,
	}

	if cr.Spec.MarkLogicGroups[index].AdditionalVolumeClaimTemplates != nil {
		markLogicGroupParameters.AdditionalVolumeClaimTemplates = cr.Spec.MarkLogicGroups[index].AdditionalVolumeClaimTemplates
	}

	if cr.Spec.Auth != nil && cr.Spec.Auth.SecretName != nil && *cr.Spec.Auth.SecretName != "" {
		markLogicGroupParameters.SecretName = *cr.Spec.Auth.SecretName
	} else {
		markLogicGroupParameters.SecretName = fmt.Sprintf("%s-admin", cr.ObjectMeta.Name)
	}
	if cr.Spec.MarkLogicGroups[index].HAProxy != nil && cr.Spec.MarkLogicGroups[index].HAProxy.PathBasedRouting != nil {
		markLogicGroupParameters.PathBasedRouting = *cr.Spec.MarkLogicGroups[index].HAProxy.PathBasedRouting
	}
	if cr.Spec.MarkLogicGroups[index].Image != "" {
		markLogicGroupParameters.Image = cr.Spec.MarkLogicGroups[index].Image
	}
	if cr.Spec.MarkLogicGroups[index].ImagePullPolicy != "" {
		markLogicGroupParameters.ImagePullPolicy = cr.Spec.MarkLogicGroups[index].ImagePullPolicy
	}
	if cr.Spec.MarkLogicGroups[index].ImagePullSecrets != nil {
		markLogicGroupParameters.ImagePullSecrets = cr.Spec.MarkLogicGroups[index].ImagePullSecrets
	}
	if cr.Spec.MarkLogicGroups[index].Persistence != nil {
		markLogicGroupParameters.Persistence = cr.Spec.MarkLogicGroups[index].Persistence
	}
	if cr.Spec.MarkLogicGroups[index].Resources != nil {
		markLogicGroupParameters.Resources = cr.Spec.MarkLogicGroups[index].Resources
	}
	if cr.Spec.MarkLogicGroups[index].Affinity != nil {
		markLogicGroupParameters.Affinity = cr.Spec.MarkLogicGroups[index].Affinity
	}
	if cr.Spec.MarkLogicGroups[index].NodeSelector != nil {
		markLogicGroupParameters.NodeSelector = cr.Spec.MarkLogicGroups[index].NodeSelector
	}
	if cr.Spec.MarkLogicGroups[index].TopologySpreadConstraints != nil {
		markLogicGroupParameters.TopologySpreadConstraints = cr.Spec.MarkLogicGroups[index].TopologySpreadConstraints
	}
	if cr.Spec.MarkLogicGroups[index].PriorityClassName != "" {
		markLogicGroupParameters.PriorityClassName = cr.Spec.MarkLogicGroups[index].PriorityClassName
	}
	if cr.Spec.MarkLogicGroups[index].HugePages != nil {
		markLogicGroupParameters.HugePages = cr.Spec.MarkLogicGroups[index].HugePages
	}
	if cr.Spec.MarkLogicGroups[index].LogCollection != nil {
		markLogicGroupParameters.LogCollection = cr.Spec.MarkLogicGroups[index].LogCollection
	}
	if cr.Spec.MarkLogicGroups[index].Tls != nil {
		markLogicGroupParameters.Tls = cr.Spec.MarkLogicGroups[index].Tls
	}
	if cr.Spec.MarkLogicGroups[index].AdditionalVolumes != nil {
		markLogicGroupParameters.AdditionalVolumes = cr.Spec.MarkLogicGroups[index].AdditionalVolumes
	}
	if cr.Spec.MarkLogicGroups[index].AdditionalVolumeMounts != nil {
		markLogicGroupParameters.AdditionalVolumeMounts = cr.Spec.MarkLogicGroups[index].AdditionalVolumeMounts
	}
	return markLogicGroupParameters
}
