package k8sutil

import (
	"context"
	"fmt"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strconv"
)

type statefulSetParameters struct {
	Replicas                      *int32
	Name                          string
	Metadata                      metav1.ObjectMeta
	PersistentVolumeClaim         corev1.PersistentVolumeClaim
	TerminationGracePeriodSeconds *int64
}

type containerParameters struct {
	Name               string
	Namespace          string
	ClusterDomain      string
	Image              string
	ImagePullPolicy    corev1.PullPolicy
	Resources          *corev1.ResourceRequirements
	PersistenceEnabled *bool
	Volumes            []corev1.Volume
	MountPaths         []corev1.VolumeMount
	LicenseKey         string
	Licensee           string
	BootstrapHost      string
	LivenessProbe      databasev1alpha1.ContainerProbe
	ReadinessProbe     databasev1alpha1.ContainerProbe
	GroupConfig        databasev1alpha1.GroupConfig
}

func (oc *OperatorContext) ReconcileStatefulset() (reconcile.Result, error) {
	cr := oc.GetMarkLogicServer()
	logger := oc.ReqLogger
	labels := getMarkLogicLabels(cr.Spec.Name)
	annotations := map[string]string{}
	objectMeta := generateObjectMeta(cr.Spec.Name, cr.Namespace, labels, annotations)
	sts, err := oc.GetStatefulSet(cr.Namespace, objectMeta.Name)
	containerParams := generateContainerParams(cr)
	statefulSetParams := generateStatefulSetsParams(cr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			statefulSetDef := generateStatefulSetsDef(objectMeta, statefulSetParams, marklogicServerAsOwner(cr), containerParams)
			oc.createStatefulSet(cr.Namespace, statefulSetDef, cr)
			oc.Recorder.Event(oc.MarklogicGroup, "Normal", "StatefulSetCreated", "MarkLogic statefulSet created successfully")
			return result.RequeueSoon(10).Output()
		}
		result.Error(err).Output()
	}
	if err != nil {
		logger.Error(err, "Cannot create standalone statefulSet for MarkLogic")
		return result.Error(err).Output()
	}
	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	updated := false
	if sts.Status.ReadyReplicas == 0 || sts.Status.ReadyReplicas != sts.Status.Replicas {
		logger.Info("MarkLogic statefulSet is not ready, setting condition and requeue")
		condition := metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "MarkLogicGroupStatefulSetNotReady",
			Message: "MarkLogicGroup statefulSet is not ready",
		}
		updated = oc.setCondition(&condition)
		if updated {
			err := oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch)
			if err != nil {
				oc.ReqLogger.Error(err, "error updating the MarkLogic Operator Internal status")
			}
		}
		return result.RequeueSoon(10).Output()
	} else {
		logger.Info("MarkLogic statefulSet is ready, setting condition")
		condition := metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "MarkLogicGroupStatefulSetReady",
			Message: "MarkLogicGroup statefulSet is ready",
		}
		updated = oc.setCondition(&condition)
	}
	if updated {
		err := oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch)
		if err != nil {
			oc.ReqLogger.Error(err, "error updating the MarkLogic Operator Internal status")
		}
	}
	logger.Info("Operator Status:", "Stage", cr.Status.Stage)
	if cr.Status.Stage == "STS_CREATED" {
		logger.Info("MarkLogic statefulSet created successfully, waiting for pods to be ready")
		pods, err := GetPodsForStatefulSet(cr.Namespace, cr.Spec.Name)
		if err != nil {
			logger.Error(err, "Error getting pods for statefulset")
		}
		logger.Info("Pods in statefulSet: ", "Pods", pods)
	}

	return result.Done().Output()
}

func (oc *OperatorContext) setCondition(condition *metav1.Condition) bool {
	group := oc.MarklogicGroup
	if group.Status.GetConditionStatus(condition.Type) != condition.Status {
		// We are changing the status, so record the transition time
		condition.LastTransitionTime = metav1.Now()
		group.SetCondition(*condition)
		return true
	}
	return false
}

func (oc *OperatorContext) GetStatefulSet(namespace string, stateful string) (*appsv1.StatefulSet, error) {
	logger := oc.ReqLogger
	statefulInfo := &appsv1.StatefulSet{}
	err := oc.Client.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: stateful}, statefulInfo)
	if err != nil {
		logger.Info("MarkLogic statefulSet get action failed")
		return nil, err
	}
	logger.Info("MarkLogic statefulSet get action was successful")
	return statefulInfo, nil
}

func (oc *OperatorContext) createStatefulSet(namespace string, stateful *appsv1.StatefulSet, cr *databasev1alpha1.MarklogicGroup) error {
	logger := oc.ReqLogger
	err := oc.Client.Create(context.TODO(), stateful)
	// _, err := GenerateK8sClient().AppsV1().StatefulSets(namespace).Create(context.TODO(), stateful, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "MarkLogic stateful creation failed")
		return err
	}
	cr.Status.Stage = "STS_CREATED"
	logger.Info("MarkLogic stateful successfully created")
	return nil
}

func generateStatefulSetsDef(stsMeta metav1.ObjectMeta, params statefulSetParameters, ownerDef metav1.OwnerReference, containerParams containerParameters) *appsv1.StatefulSet {
	statefulSet := &appsv1.StatefulSet{
		TypeMeta:   generateTypeMeta("StatefulSet", "apps/v1"),
		ObjectMeta: stsMeta,
		Spec: appsv1.StatefulSetSpec{
			Selector:            LabelSelectors(stsMeta.GetLabels()),
			ServiceName:         stsMeta.Name,
			Replicas:            params.Replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: stsMeta.GetLabels(),
				},
				Spec: corev1.PodSpec{
					Containers:                    generateContainerDef(stsMeta.GetName(), containerParams),
					TerminationGracePeriodSeconds: params.TerminationGracePeriodSeconds,
					Volumes:                       generateVolumes(stsMeta.Name),
				},
			},
		},
	}
	// add EmptyDir volume if storage is not provided
	if containerParams.PersistenceEnabled == nil || !*containerParams.PersistenceEnabled {
		emptyDir := corev1.Volume{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, emptyDir)
	} else {
		statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, params.PersistentVolumeClaim)
	}
	AddOwnerRefToObject(statefulSet, ownerDef)
	return statefulSet
}

func GetPodsForStatefulSet(namespace, name string) ([]corev1.Pod, error) {
	selector := fmt.Sprintf("app.kubernetes.io/name=marklogic,app.kubernetes.io/instance=%s", name)
	// List Pods with the label selector
	listOptions := metav1.ListOptions{LabelSelector: selector}
	pods, err := GenerateK8sClient().CoreV1().Pods(namespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}

	return pods.Items, nil
}

func generateContainerDef(name string, containerParams containerParameters) []corev1.Container {
	containerDef := []corev1.Container{
		{
			Name:            name,
			Image:           containerParams.Image,
			ImagePullPolicy: containerParams.ImagePullPolicy,
			Env:             getEnvironmentVariables(containerParams),
			Lifecycle:       getLifeCycle(),
			VolumeMounts:    getVolumeMount(),
		},
	}
	if containerParams.Resources != nil {
		containerDef[0].Resources = *containerParams.Resources
	}
	if containerParams.LivenessProbe.Enabled == true {
		containerDef[0].LivenessProbe = getLivenessProbe(containerParams.LivenessProbe)
	}

	if containerParams.ReadinessProbe.Enabled == true {
		containerDef[0].ReadinessProbe = getReadinessProbe(containerParams.ReadinessProbe)
	}

	return containerDef
}

func generateStatefulSetsParams(cr *databasev1alpha1.MarklogicGroup) statefulSetParameters {
	params := statefulSetParameters{
		Replicas:                      cr.Spec.Replicas,
		Name:                          cr.Spec.Name,
		TerminationGracePeriodSeconds: cr.Spec.TerminationGracePeriodSeconds,
	}
	if cr.Spec.Storage != nil {
		params.PersistentVolumeClaim = generatePVCTemplate(cr.Spec.Storage.Size)
	}
	return params
}

func generateContainerParams(cr *databasev1alpha1.MarklogicGroup) containerParameters {
	trueProperty := true
	containerParams := containerParameters{
		Image:          cr.Spec.Image,
		Resources:      cr.Spec.Resources,
		Name:           cr.Spec.Name,
		Namespace:      cr.Namespace,
		ClusterDomain:  cr.Spec.ClusterDomain,
		BootstrapHost:  cr.Spec.BootstrapHost,
		LivenessProbe:  cr.Spec.LivenessProbe,
		ReadinessProbe: cr.Spec.ReadinessProbe,
		GroupConfig:    cr.Spec.GroupConfig,
	}

	if cr.Spec.Storage != nil {
		containerParams.Volumes = cr.Spec.Storage.VolumeMount.Volume
		containerParams.MountPaths = cr.Spec.Storage.VolumeMount.MountPath
	}

	if cr.Spec.Storage != nil {
		containerParams.PersistenceEnabled = &trueProperty
	}
	if cr.Spec.License != nil {
		containerParams.LicenseKey = cr.Spec.License.Key
		containerParams.Licensee = cr.Spec.License.Licensee
	}

	return containerParams
}

func getLifeCycle() *corev1.Lifecycle {
	return &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "/tmp/helm-scripts/poststart-hook.sh"},
			},
		},
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "/tmp/helm-scripts/prestop-hook.sh"},
			},
		},
	}
}

func generateVolumes(stsName string) []corev1.Volume {
	volumes := []corev1.Volume{}
	volumes = append(volumes, corev1.Volume{
		Name: "helm-scripts",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: fmt.Sprintf("%s-scripts", stsName),
				},
				DefaultMode: func(i int32) *int32 { return &i }(0755),
			},
		},
	}, corev1.Volume{
		Name: "mladmin-secrets",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: fmt.Sprintf("%s-admin", stsName),
			},
		},
	})
	return volumes
}

func generatePVCTemplate(storageSize string) corev1.PersistentVolumeClaim {
	pvcTemplate := corev1.PersistentVolumeClaim{}
	pvcTemplate.CreationTimestamp = metav1.Time{}
	pvcTemplate.Name = "data"
	pvcTemplate.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	pvcTemplate.Spec.Resources.Requests.Storage().Add(resource.MustParse(storageSize))
	pvcTemplate.Spec.Resources.Requests = corev1.ResourceList{
		corev1.ResourceStorage: resource.MustParse(storageSize),
	}
	return pvcTemplate
}

func getEnvironmentVariables(containerParams containerParameters) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}
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
		Value: containerParams.GroupConfig.Name,
	}, corev1.EnvVar{
		Name:  "XDQP_SSL_ENABLED",
		Value: strconv.FormatBool(containerParams.GroupConfig.EnableXdqpSsl),
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

	if containerParams.BootstrapHost != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_BOOTSTRAP_HOST",
			Value: containerParams.BootstrapHost,
		})
	} else {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MARKLOGIC_BOOTSTRAP_HOST",
			Value: fmt.Sprintf("%s-0.%s.%s.svc.%s", containerParams.Name, containerParams.Name, containerParams.Namespace, containerParams.ClusterDomain),
		})
	}

	return envVars
}

func getVolumeMount() []corev1.VolumeMount {
	var VolumeMounts []corev1.VolumeMount

	// if persistenceEnabled != nil && *persistenceEnabled {
	VolumeMounts = append(VolumeMounts,
		corev1.VolumeMount{
			Name:      "data",
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
	)
	return VolumeMounts
}

func getLivenessProbe(probe databasev1alpha1.ContainerProbe) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: probe.InitialDelaySeconds,
		PeriodSeconds:       probe.PeriodSeconds,
		FailureThreshold:    probe.FailureThreshold,
		TimeoutSeconds:      probe.TimeoutSeconds,
		SuccessThreshold:    probe.SuccessThreshold,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "/tmp/helm-scripts/liveness-probe.sh"},
			},
		},
	}
}

func getReadinessProbe(probe databasev1alpha1.ContainerProbe) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: probe.InitialDelaySeconds,
		PeriodSeconds:       probe.PeriodSeconds,
		FailureThreshold:    probe.FailureThreshold,
		TimeoutSeconds:      probe.TimeoutSeconds,
		SuccessThreshold:    probe.SuccessThreshold,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: 7997,
				},
			},
		},
	}
}
