package k8sutil

import (
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	// "sigs.k8s.io/controller-runtime/pkg/client/patch"
	"context"
	"fmt"
)

func (cc *ClusterContext) ReconcileDynamicHost() result.ReconcileResult {
	cc.ReqLogger.Info("handler::ReconcileDynamicHost")
	if cc.MarklogicCluster.Spec.DynamicHost != nil && cc.MarklogicCluster.Spec.DynamicHost.Enabled {
		cc.ReconcileStatefulsetForDynamicHost()
		cc.ReqLogger.Info("Dynamic Host is enabled; reconciling related resources")
	}
	return result.Continue()
}

func (cc *ClusterContext) ReconcileStatefulsetForDynamicHost() (result.ReconcileResult, error) {
	cr := cc.GetMarkLogicCluster()
	logger := cc.ReqLogger
	logger.Info("Reconciling StatefulSet for Dynamic Host")
	groupLabels := cr.Labels
	dynamicHostName := cr.GetObjectMeta().GetName() + "-dynamic"
	if groupLabels == nil {
		groupLabels = getSelectorLabels(dynamicHostName)
	}
	groupLabels["app.kubernetes.io/instance"] = dynamicHostName
	groupAnnotations := cr.GetAnnotations()
	delete(groupAnnotations, "banzaicloud.com/last-applied")
	objectMeta := generateObjectMeta(dynamicHostName, cr.Namespace, groupLabels, groupAnnotations)
	currentSts, err := cc.GetStatefulSet(cr.Namespace, dynamicHostName)
	if currentSts != nil {
		logger.Info("Fetched current StatefulSet", "StatefulSet.Name", currentSts.Name)
	} else {
		logger.Info("No existing StatefulSet found for Dynamic Host", "StatefulSet.Name", dynamicHostName)
	}
	containerParams := generateContainerParamsForDynamicHost(cr)
	statefulSetParams := generateStatefulSetsParamsForDynamicHost(cr)
	statefulSetDef := generateStatefulSetsDefForDynamicHost(objectMeta, statefulSetParams, marklogicClusterAsOwner(cr), containerParams, cr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			cc.createStatefulSet(statefulSetDef)
			return result.RequeueSoon(10), nil
		}
		result.Error(err).Output()
	}
	return result.Continue(), nil
}

func generateStatefulSetsDefForDynamicHost(stsMeta metav1.ObjectMeta, params statefulSetParameters, ownerDef metav1.OwnerReference, containerParams containerParameters, cr *marklogicv1.MarklogicCluster) *appsv1.StatefulSet {
	statefulSet := &appsv1.StatefulSet{
		TypeMeta:   generateTypeMeta("StatefulSet", "apps/v1"),
		ObjectMeta: stsMeta,
		Spec: appsv1.StatefulSetSpec{
			Selector:            LabelSelectors(getSelectorLabels(params.Name)),
			ServiceName:         stsMeta.Name,
			Replicas:            params.Replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: params.UpdateStrategy},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      stsMeta.GetLabels(),
					Annotations: stsMeta.GetAnnotations(),
				},
				Spec: corev1.PodSpec{
					InitContainers:                generateInitContainersForDynamicHost(containerParams),
					Containers:                    generateContainerDefForDynamicHost(containerParams),
					TerminationGracePeriodSeconds: params.TerminationGracePeriodSeconds,
					SecurityContext:               containerParams.PodSecurityContext,
					Volumes:                       generateVolumesForDynamicHost(cr, containerParams),
					NodeSelector:                  params.NodeSelector,
					Affinity:                      params.Affinity,
					TopologySpreadConstraints:     params.TopologySpreadConstraints,
					PriorityClassName:             params.PriorityClassName,
					ImagePullSecrets:              params.ImagePullSecrets,
				},
			},
		},
	}
	// add EmptyDir volume if persistence is not provided
	if containerParams.Persistence == nil || !containerParams.Persistence.Enabled {
		emptyDir := corev1.Volume{
			Name:         "datadir",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, emptyDir)
	} else {
		statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, params.PersistentVolumeClaim)
	}
	if params.AdditionalVolumeClaimTemplates != nil {
		statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, *params.AdditionalVolumeClaimTemplates...)
	}
	if params.ServiceAccountName != "" {
		statefulSet.Spec.Template.Spec.ServiceAccountName = params.ServiceAccountName
	}

	AddOwnerRefToObject(statefulSet, ownerDef)
	return statefulSet
}

func generateInitContainersForDynamicHost(containerParams containerParameters) []corev1.Container {
	initContainers := []corev1.Container{}
	init := corev1.Container{
		Name:            "dynamic-host-init",
		Image:           containerParams.Image,
		ImagePullPolicy: containerParams.ImagePullPolicy,
		Command:         []string{"/bin/sh", "/tmp/helm-scripts/dynamic-host-init.sh"},
		Env:             getEnvironmentVariablesForDynamicHost(containerParams),
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "helm-scripts",
				MountPath: "/tmp/helm-scripts/",
			},
			{
				Name:      "mladmin-secrets",
				MountPath: "/run/secrets/ml-secrets/",
			},
			{
				Name:      "shared-data",
				MountPath: "/var/tokens",
			},
		},
	}
	initContainers = append(initContainers, init)
	return initContainers
}

func (cc *ClusterContext) GetStatefulSet(namespace string, statefulSetName string) (*appsv1.StatefulSet, error) {
	logger := cc.ReqLogger
	statefulInfo := &appsv1.StatefulSet{}
	logger.Info("Getting MarkLogic Dynamic Host statefulSet", "StatefulSet.Name", statefulSetName)
	err := cc.Client.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: statefulSetName}, statefulInfo)
	if err != nil {
		logger.Info("Getting MarkLogic Dynamic Host statefulSet failed")
		return nil, err
	}
	logger.Info("MarkLogic Dynamic Host statefulSet get action was successful")
	return statefulInfo, nil
}

func generateContainerParamsForDynamicHost(cr *marklogicv1.MarklogicCluster) containerParameters {
	containerParams := containerParameters{
		Image:                  cr.Spec.Image,
		Resources:              cr.Spec.Resources,
		Name:                   cr.GetObjectMeta().GetName() + "-dynamic",
		Namespace:              cr.Namespace,
		DynamicHost:            cr.Spec.DynamicHost,
		ClusterDomain:          cr.Spec.ClusterDomain,
		EnableConverters:       cr.Spec.EnableConverters,
		PodSecurityContext:     cr.Spec.PodSecurityContext,
		SecurityContext:        cr.Spec.ContainerSecurityContext,
		LogCollection:          cr.Spec.LogCollection,
		Tls:                    cr.Spec.Tls,
		AdditionalVolumes:      cr.Spec.AdditionalVolumes,
		AdditionalVolumeMounts: cr.Spec.AdditionalVolumeMounts,
		Persistence:            cr.Spec.Persistence,
	}
	if cr.Spec.Auth != nil && cr.Spec.Auth.SecretName != nil && *cr.Spec.Auth.SecretName != "" {
		containerParams.SecretName = *cr.Spec.Auth.SecretName
	} else {
		containerParams.SecretName = fmt.Sprintf("%s-admin", cr.ObjectMeta.Name)
	}
	bootStrapName := ""
	for _, group := range cr.Spec.MarkLogicGroups {
		if group.IsBootstrap {
			bootStrapName = group.Name
		}
	}
	nsName := cr.ObjectMeta.Namespace
	clusterName := cr.Spec.ClusterDomain
	bootStrapHostName := fmt.Sprintf("%s-0.%s.%s.svc.%s", bootStrapName, bootStrapName, nsName, clusterName)
	containerParams.BootstrapHost = bootStrapHostName
	return containerParams
}

func (cc *ClusterContext) createStatefulSet(statefulset *appsv1.StatefulSet) error {
	logger := cc.ReqLogger
	err := cc.Client.Create(context.TODO(), statefulset)
	if err != nil {
		logger.Error(err, "MarkLogic stateful creation failed")
		return err
	}
	logger.Info("MarkLogic stateful for dynamic hosts successfully created")
	return nil
}

func generateStatefulSetsParamsForDynamicHost(cr *marklogicv1.MarklogicCluster) statefulSetParameters {
	params := statefulSetParameters{
		Replicas:                       &cr.Spec.DynamicHost.Size,
		Name:                           cr.GetObjectMeta().GetName() + "-dynamic",
		ServiceAccountName:             cr.Spec.ServiceAccountName,
		TerminationGracePeriodSeconds:  cr.Spec.TerminationGracePeriodSeconds,
		UpdateStrategy:                 cr.Spec.UpdateStrategy,
		NodeSelector:                   cr.Spec.NodeSelector,
		Affinity:                       cr.Spec.Affinity,
		TopologySpreadConstraints:      cr.Spec.TopologySpreadConstraints,
		PriorityClassName:              cr.Spec.PriorityClassName,
		ImagePullSecrets:               cr.Spec.ImagePullSecrets,
		AdditionalVolumeClaimTemplates: cr.Spec.AdditionalVolumeClaimTemplates,
	}
	if cr.Spec.Persistence != nil && cr.Spec.Persistence.Enabled {
		params.PersistentVolumeClaim = generatePVCTemplate(cr.Spec.Persistence)
	}
	return params
}

func generateContainerDefForDynamicHost(containerParams containerParameters) []corev1.Container {
	containerDef := []corev1.Container{
		{
			Name:            "marklogic-dynamic-host",
			Image:           containerParams.Image,
			ImagePullPolicy: containerParams.ImagePullPolicy,
			Env:             getEnvironmentVariablesForDynamicHost(containerParams),
			Lifecycle:       getLifeCycleForDynamicHost(),
			SecurityContext: containerParams.SecurityContext,
			VolumeMounts:    getVolumeMountForDynamicHost(containerParams),
		},
	}
	if containerParams.Resources != nil {
		containerDef[0].Resources = *containerParams.Resources
	}

	if containerParams.LivenessProbe.Enabled {
		containerDef[0].LivenessProbe = getLivenessProbe(containerParams.LivenessProbe)
	}

	if containerParams.ReadinessProbe.Enabled {
		containerDef[0].ReadinessProbe = getReadinessProbe(containerParams.ReadinessProbe)
	}

	return containerDef
}

func getVolumeMountForDynamicHost(containerParams containerParameters) []corev1.VolumeMount {
	var VolumeMounts []corev1.VolumeMount

	VolumeMounts = append(VolumeMounts,
		corev1.VolumeMount{
			Name:      "datadir",
			MountPath: "/var/opt/MarkLogic",
		},
		corev1.VolumeMount{
			Name:      "helm-scripts",
			MountPath: "/tmp/helm-scripts",
		},
		corev1.VolumeMount{
			Name:      "mladmin-secrets",
			MountPath: "/run/secrets/ml-secrets",
			ReadOnly:  true,
		},
		corev1.VolumeMount{
			Name:      "shared-data",
			MountPath: "/var/tokens",
		},
	)
	if containerParams.HugePages != nil && containerParams.HugePages.Enabled {
		VolumeMounts = append(VolumeMounts,
			corev1.VolumeMount{
				Name:      "huge-pages",
				MountPath: containerParams.HugePages.MountPath,
			},
		)
	}
	if containerParams.Tls != nil && containerParams.Tls.EnableOnDefaultAppServers {
		VolumeMounts = append(VolumeMounts,
			corev1.VolumeMount{
				Name:      "certs",
				MountPath: "/run/secrets/marklogic-certs/",
			})
	}
	if containerParams.AdditionalVolumeMounts != nil {
		VolumeMounts = append(VolumeMounts, *containerParams.AdditionalVolumeMounts...)
	}
	return VolumeMounts
}

func getLifeCycleForDynamicHost() *corev1.Lifecycle {
	return &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "/tmp/helm-scripts/dynamic-host-poststart.sh"},
			},
		},
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "/tmp/helm-scripts/prestop-hook.sh"},
			},
		},
	}
}

func getEnvironmentVariablesForDynamicHost(containerParams containerParameters) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}
	groupName := "Default"
	if containerParams.GroupConfig != nil && containerParams.GroupConfig.Name != "" {
		groupName = containerParams.GroupConfig.Name
	}
	groupName = "Default"
	envVars = append(envVars, corev1.EnvVar{
		Name:  "MARKLOGIC_ADMIN_USERNAME_FILE",
		Value: "ml-secrets/username",
	}, corev1.EnvVar{
		Name:  "MARKLOGIC_ADMIN_PASSWORD_FILE",
		Value: "ml-secrets/password",
	}, corev1.EnvVar{
		Name:  "MARKLOGIC_FQDN_SUFFIX",
		Value: fmt.Sprintf("%s.%s.svc.%s", containerParams.Name, containerParams.Namespace, containerParams.ClusterDomain),
	}, corev1.EnvVar{
		Name:  "MARKLOGIC_INIT",
		Value: "false",
	}, corev1.EnvVar{
		Name:  "MARKLOGIC_JOIN_CLUSTER",
		Value: "false",
	}, corev1.EnvVar{
		Name:  "MARKLOGIC_GROUP",
		Value: groupName,
	}, corev1.EnvVar{
		Name:  "MARKLOGIC_CLUSTER_TYPE",
		Value: "bootstrap",
	},
	)

	if containerParams.LicenseKey != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LICENSE_KEY",
			Value: containerParams.LicenseKey,
		}, corev1.EnvVar{
			Name:  "LICENSEE",
			Value: containerParams.Licensee,
		})
	}

	if containerParams.DynamicHost != nil && containerParams.DynamicHost.Enabled {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_DYNAMIC_HOST",
			Value: "true",
		})
	}

	if containerParams.BootstrapHost != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_BOOTSTRAP_HOST",
			Value: containerParams.BootstrapHost,
		},
			corev1.EnvVar{
				Name:  "MARKLOGIC_CLUSTER_TYPE",
				Value: "non-bootstrap",
			})
	} else {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_BOOTSTRAP_HOST",
			Value: fmt.Sprintf("%s-0.%s.%s.svc.%s", containerParams.Name, containerParams.Name, containerParams.Namespace, containerParams.ClusterDomain),
		}, corev1.EnvVar{
			Name:  "MARKLOGIC_CLUSTER_TYPE",
			Value: "bootstrap",
		})
	}

	if containerParams.Tls != nil && containerParams.Tls.EnableOnDefaultAppServers {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_JOIN_TLS_ENABLED",
			Value: "true",
		}, corev1.EnvVar{
			Name:  "MARKLOGIC_JOIN_CACERT_FILE",
			Value: "marklogic-certs/cacert.pem",
		})
	} else {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_JOIN_TLS_ENABLED",
			Value: "false",
		})
	}

	return envVars
}

func generateVolumesForDynamicHost(cr *marklogicv1.MarklogicCluster, containerParams containerParameters) []corev1.Volume {
	volumes := []corev1.Volume{}
	otherStsName := cr.Spec.MarkLogicGroups[0].Name
	volumes = append(volumes, corev1.Volume{
		Name: "helm-scripts",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: fmt.Sprintf("%s-scripts", otherStsName),
				},
				DefaultMode: func(i int32) *int32 { return &i }(0755),
			},
		},
	}, corev1.Volume{
		Name: "mladmin-secrets",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: containerParams.SecretName,
			},
		},
	}, corev1.Volume{
		Name:         "shared-data",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	if containerParams.HugePages != nil && containerParams.HugePages.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "huge-pages",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumHugePages,
				},
			},
		})
	}
	if containerParams.LogCollection != nil && containerParams.LogCollection.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "fluent-bit",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "fluent-bit",
					},
				},
			},
		})
	}
	if containerParams.AdditionalVolumes != nil {
		volumes = append(volumes, *containerParams.AdditionalVolumes...)
	}

	return volumes
}
