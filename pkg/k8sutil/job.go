package k8sutil

import (
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	// "github.com/cisco-open/k8s-objectmatcher/patch"
	// databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
)

func (cc *ClusterContext) ReconcileJobs() result.ReconcileResult {
	logger := cc.ReqLogger
	logger.Info("Reconciling Jobs")
	cr := cc.MarklogicCluster
	labels := getMarkLogicLabels(cr.ObjectMeta.Name)
	annotations := map[string]string{}
	objectName := cr.ObjectMeta.Name
	volumeName := cr.ObjectMeta.Name + "-scripts"
	secretName := cr.ObjectMeta.Name + "-admin"
	jobName := objectName
	objectMeta := generateObjectMeta(jobName, cc.Request.Namespace, labels, annotations)
	currentJob, err := cc.getJob(cr.Namespace, jobName)
	clusterConfig := cc.generateClusterConfig()
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic Job is not found, creating a new one")
			jobDef := generateJobDef(objectMeta, marklogicClusterAsOwner(cc.MarklogicCluster), volumeName, secretName, clusterConfig)
			err = cc.createJob(jobDef)
			if err != nil {
				logger.Info("MarkLogic Job creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic Job creation is successful")
			// result.Continue()
		} else {
			logger.Error(err, "MarkLogic Job creation is failed")
			return result.Error(err)
		}
	} else {
		jobDef := generateJobDef(objectMeta, marklogicClusterAsOwner(cc.MarklogicCluster), volumeName, secretName, clusterConfig)
		err := cc.Client.Update(cc.Ctx, jobDef)
		if err != nil {
			logger.Error(err, "MarkLogic Job update is failed")
			return result.Error(err)
		}
	}
	logger.Info("Current Job", "Job", currentJob)

	return result.Continue()
}

func (cc *ClusterContext) getJob(namespace string, name string) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := cc.Client.Get(cc.Ctx, types.NamespacedName{Name: name, Namespace: namespace}, job)
	return job, err
}

func (oc *ClusterContext) createJob(job *batchv1.Job) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, job)
	if err != nil {
		logger.Error(err, "MarkLogic cluster Job creation is failed")
		return err
	}
	logger.Info("MarkLogic cluster Job creation is successful")
	return nil
}

func generateJobDef(meta metav1.ObjectMeta, ownerRef metav1.OwnerReference, volumeName string, secretName string, clusterConfig string) *batchv1.Job {
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Job",
			APIVersion: "v1",
		},
		ObjectMeta: meta,
		Spec: batchv1.JobSpec{
			BackoffLimit:            func(i int32) *int32 { return &i }(1),
			TTLSecondsAfterFinished: func(i int32) *int32 { return &i }(100),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "marklogic",
							Image:   "dwdraju/alpine-curl-jq:latest",
							Command: []string{"/bin/bash", "/tmp/job-scripts/job.sh"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "job-scripts",
									MountPath: "/tmp/job-scripts/",
								},
								{
									Name:      "mladmin-secrets",
									MountPath: "/run/secrets/ml-secrets",
									ReadOnly:  true,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "MARKLOGIC_ADMIN_USERNAME_FILE",
									Value: "ml-secrets/username",
								}, {
									Name:  "MARKLOGIC_ADMIN_PASSWORD_FILE",
									Value: "ml-secrets/password",
								}, {
									Name:  "MARKLOGIC_CLUSTER_CONFIG",
									Value: clusterConfig,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "job-scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: volumeName,
									},
									DefaultMode: func(i int32) *int32 { return &i }(0755),
								},
							},
						},
						{
							Name: "mladmin-secrets",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretName,
								},
							},
						},
					},
				},
			},
		},
	}
	job.SetOwnerReferences(append(job.GetOwnerReferences(), ownerRef))
	return job
}

type ClusterConfig struct {
	FqdnSuffix    string `json:"fqdnsuffix"`
	StsName       string `json:"stsname"`
	GroupName     string `json:"groupname"`
	IsBootstrap   bool   `json:"isbootstrap"`
	EnableXdqpSsl bool   `json:"enablexdqpssl"`
	Replicas      int32  `json:"replicas"`
}

func (cc *ClusterContext) generateClusterConfig() string {
	mlc := cc.MarklogicCluster
	nsName := cc.Request.Namespace
	clusters := []ClusterConfig{}
	clusterConfig := ""
	for _, group := range mlc.Spec.MarkLogicGroups {
		cluster := ClusterConfig{
			FqdnSuffix:    fmt.Sprintf("%s.%s.svc.%s", group.Name, nsName, mlc.Spec.ClusterDomain),
			StsName:       group.Name,
			GroupName:     group.GroupConfig.Name,
			IsBootstrap:   group.IsBootstrap,
			EnableXdqpSsl: group.GroupConfig.EnableXdqpSsl,
			Replicas:      *group.Replicas,
		}
		clusters = append(clusters, cluster)
	}
	jsonData, err := json.Marshal(clusters)
	if err != nil {
		cc.ReqLogger.Error(err, "Error marshalling cluster config")
	}
	clusterConfig = string(jsonData)

	return clusterConfig
}
