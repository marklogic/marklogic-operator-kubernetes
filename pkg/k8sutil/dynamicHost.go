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
	cc.ReqLogger.Info("Dynamic Host is disabled; ensured related resources are deleted and status cleared")
	return result.Continue()
}

func (cc *ClusterContext) ReconcileStatefulsetForDynamicHost() (result.ReconcileResult, error) {
	cr := cc.GetMarkLogicCluster()
	logger := cc.ReqLogger
	logger.Info("Reconciling StatefulSet for Dynamic Host")
	groupLabels := cr.Labels
	dynamicHostName := cr.GetObjectMeta().GetName() + "-dynamic-host"
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
	if containerParams.Tls != nil && containerParams.Tls.EnableOnDefaultAppServers {
		copyCertsVM := []corev1.VolumeMount{
			{
				Name:      "certs",
				MountPath: "/run/secrets/marklogic-certs/",
			},
			{
				Name:      "mladmin-secrets",
				MountPath: "/run/secrets/ml-secrets/",
			},
			{
				Name:      "helm-scripts",
				MountPath: "/tmp/helm-scripts/",
			},
		}
		if containerParams.Tls.CertSecretNames != nil {
			copyCertsVM = append(copyCertsVM, corev1.VolumeMount{
				Name:      "ca-cert-secret",
				MountPath: "/tmp/ca-cert-secret/",
			}, corev1.VolumeMount{
				Name:      "server-cert-secrets",
				MountPath: "/tmp/server-cert-secrets/",
			})
		}
		statefulSet.Spec.Template.Spec.InitContainers = []corev1.Container{
			{
				Name:            "copy-certs",
				Image:           "redhat/ubi9:9.4",
				ImagePullPolicy: "IfNotPresent",
				Command:         []string{"/bin/sh", "/tmp/helm-scripts/copy-certs.sh"},
				VolumeMounts:    copyCertsVM,
				Env: []corev1.EnvVar{
					{
						Name:      "POD_NAME",
						ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
					},
					{
						Name:  "MARKLOGIC_ADMIN_USERNAME_FILE",
						Value: "ml-secrets/username",
					},
					{
						Name:  "MARKLOGIC_ADMIN_PASSWORD_FILE",
						Value: "ml-secrets/password",
					},
					{
						Name:  "MARKLOGIC_FQDN_SUFFIX",
						Value: fmt.Sprintf("%s.%s.svc.%s", containerParams.Name, containerParams.Namespace, containerParams.ClusterDomain),
					},
				},
			},
		}
	}

	AddOwnerRefToObject(statefulSet, ownerDef)
	return statefulSet
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
		Name:                   cr.GetObjectMeta().GetName() + "-dynamic-host",
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
		Name:                           cr.GetObjectMeta().GetName() + "-dynamic-host",
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
			Lifecycle:       getLifeCycle(),
			SecurityContext: containerParams.SecurityContext,
			VolumeMounts:    getVolumeMount(containerParams),
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
