// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (cc *ClusterContext) ReconcileHAProxy() result.ReconcileResult {
	logger := cc.ReqLogger
	client := cc.Client
	cr := cc.MarklogicCluster

	logger.Info("Reconciling HAProxy Config")

	labels := cc.GetHAProxyLabels(cr.GetObjectMeta().GetName())
	annotations := cc.GetClusterAnnotations()
	configMapName := "marklogic-haproxy"
	objectMeta := generateObjectMeta(configMapName, cr.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	svcName := types.NamespacedName{Name: "marklogic-haproxy", Namespace: cr.Namespace}
	configmap := &corev1.ConfigMap{}
	haproxyService := &corev1.Service{}
	err := client.Get(cc.Ctx, nsName, configmap)
	data := generateHAProxyConfigMapData(cc.Ctx, cc.MarklogicCluster)
	configMapDef := generateHAProxyConfigMap(objectMeta, marklogicClusterAsOwner(cr), data)
	haproxyDeploymentDef := cc.createHAProxyDeploymentDef(objectMeta)
	haproxyServiceDef := cc.generateHaproxyServiceDef(objectMeta)
	configmapHash := calculateHash(configMapDef.Data)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("HAProxy ConfigMap is not found, creating a new one")
			if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(configMapDef); err != nil {
				logger.Error(err, "Failed to set last applied annotation for HAProxy ConfigMap")
			}
			err = cc.createConfigMapForCC(configMapDef)
			if err != nil {
				logger.Info("HAProxy configmap creation is failed")
				return result.Error(err)
			}
			logger.Info("HAProxy configmap creation is successful")
			err = cc.createHAProxyDeployment(haproxyDeploymentDef)
			if err != nil {
				logger.Info("HAProxy Deployment creation is failed")
				return result.Error(err)
			}
			if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(haproxyServiceDef); err != nil {
				logger.Error(err, "Failed to set last applied annotation for HAProxy Service")
			}
			err = cc.createHAProxyService(haproxyServiceDef)
			if err != nil {
				logger.Info("HAProxy Service creation is failed")
				return result.Error(err)
			}
			logger.Info("HAProxy Deployed is successful")
			return result.Continue()
		} else {
			logger.Error(err, "HAProxy configmap creation is failed")
			return result.Error(err)
		}
	}
	logger.Info("HAProxy ConfigMap is found", "configmap:", configmap)
	patchDiff, err := patch.DefaultPatchMaker.Calculate(configmap, configMapDef,
		patch.IgnoreStatusFields(),
		patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
		patch.IgnoreField("kind"))
	if err != nil {
		logger.Error(err, "Error calculating patch for HAProxy configmap")
		return result.Error(err)
	}
	if !patchDiff.IsEmpty() {
		logger.Info("MarkLogic HAProxy Config spec is different from previous spec, updating the HAProxy ConfigMap")
		configmap.Data = configMapDef.Data
		if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(configmap); err != nil {
			logger.Error(err, "Failed to set last applied annotation for HAProxy ConfigMap")
		}
		err := cc.Client.Update(cc.Ctx, configmap)
		if err != nil {
			logger.Error(err, "Error updating MarkLogic HAProxy ConfigMap")
			return result.Error(err)
		}
	}
	err = client.Get(cc.Ctx, svcName, haproxyService)
	if err != nil {
		logger.Error(err, "Failed to get HAProxy service")
		return result.Error(err)
	}
	patchDiff, err = patch.DefaultPatchMaker.Calculate(haproxyService, haproxyServiceDef,
		patch.IgnoreStatusFields(),
		patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
		patch.IgnoreField("kind"))
	if err != nil {
		logger.Error(err, "Error calculating patch for HAProxy service")
		return result.Error(err)
	}
	if !patchDiff.IsEmpty() {
		logger.Info("HAProxy spec is different from the previous spec, updating the haproxy service")
		haproxyService.Spec = haproxyServiceDef.Spec
		haproxyService.ObjectMeta.Labels = haproxyServiceDef.ObjectMeta.Labels
		haproxyService.ObjectMeta.Annotations = haproxyServiceDef.ObjectMeta.Annotations
		err := cc.Client.Update(cc.Ctx, haproxyService)
		if err != nil {
			logger.Error(err, "Error updating HAProxy service")
			return result.Error(err)
		}
	}

	haproxyDeployment := &appsv1.Deployment{}
	deployName := types.NamespacedName{Name: "marklogic-haproxy", Namespace: cr.Namespace}
	err = client.Get(cc.Ctx, deployName, haproxyDeployment)
	if err != nil {
		logger.Error(err, "Failed to get HAProxy Deployment")
		return result.Error(err)
	}
	patchDiff, err = patch.DefaultPatchMaker.Calculate(haproxyDeployment, haproxyDeploymentDef,
		patch.IgnoreStatusFields(),
		patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
		patch.IgnoreField("kind"))
	if err != nil {
		logger.Error(err, "Failed to calculate HAProxy Deployment patch")
		return result.Error(err)
	}
	if haproxyDeploymentDef.Spec.Template.Annotations == nil {
		haproxyDeploymentDef.Spec.Template.Annotations = make(map[string]string)
	}
	if haproxyDeployment.Spec.Template.Annotations["configmap-hash"] != configmapHash || !patchDiff.IsEmpty() {
		logger.Info("HAProxy Deployment is different from the HAProxy ConfigMap, updating the Deployment")
		haproxyDeploymentDef.Spec.Template.Annotations["configmap-hash"] = configmapHash
		err := client.Update(cc.Ctx, haproxyDeploymentDef)
		if err != nil {
			logger.Error(err, "Error updating HAProxy Deployment")
			return result.Error(err)
		}
	}
	return result.Continue()
}

// generateHAProxyData generates the HAProxy Config Data
func generateHAProxyConfigMapData(ctx context.Context, cr *marklogicv1.MarklogicCluster) map[string]string {
	var result string
	// HAProxy Config Data
	haProxyData := make(map[string]string)
	haProxyData["haproxy.cfg"] = `
global
  log stdout format raw local0
  maxconn 1024
`
	baseConfig := `
defaults
  log global
  option forwardfor
  timeout client {{ $.ClientTimeout}}s
  timeout connect {{ $.ConnectTimeout}}s
  timeout server {{ $.ServerTimeout}}s

resolvers dns
  # add nameserver from /etc/resolv.conf
  parse-resolv-conf

  hold valid    10s

  # Maximum size of a DNS answer allowed, in bytes
  accepted_payload_size 8192

  # How long to "hold" a backend server's up/down status depending on the name resolution status.
  # For example, if an NXDOMAIN response is returned, keep the backend server in its current state (up) for
  # at least another 30 seconds before marking it as down due to DNS not having a record for it.
  hold valid    10s
  hold other    30s
  hold refused  30s
  hold nx       30s
  hold timeout  30s
  hold obsolete 30s

  # How many times to retry a query
  resolve_retries 3

  # How long to wait between retries when no valid response has been received
  timeout retry 5s

  # How long to wait for a successful resolution
  timeout resolve 5s
`
	data := map[string]interface{}{
		"ClientTimeout":  cr.Spec.HAProxy.Timeout.Client,
		"ConnectTimeout": cr.Spec.HAProxy.Timeout.Connect,
		"ServerTimeout":  cr.Spec.HAProxy.Timeout.Server,
	}
	result += parseTemplateToString(baseConfig, data) + "\n"
	haProxyData["haproxy.cfg"] += result + "\n"

	haproxyConfig := generateHAProxyConfig(ctx, cr)

	haProxyData["haproxy.cfg"] += generateFrontendConfig(cr, haproxyConfig) + "\n"
	haProxyData["haproxy.cfg"] += generateBackendConfig(cr, haproxyConfig) + "\n"

	if cr.Spec.HAProxy.Stats.Enabled {
		haProxyData["haproxy.cfg"] += generateStatsConfig(cr)
	}

	haProxyData["haproxy.cfg"] += generateTcpConfig(cr, haproxyConfig) + "\n"

	return haProxyData
}

func (cc *ClusterContext) createHAProxyDeploymentDef(meta metav1.ObjectMeta) *appsv1.Deployment {
	cr := cc.MarklogicCluster
	selectorLabels := getHAProxySelectorLabels(cr.GetObjectMeta().GetName())
	ownerDef := marklogicClusterAsOwner(cr)
	defaultMode := int32(420)
	deploymentDef := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "marklogic-haproxy",
			Namespace:   cc.Request.Namespace,
			Labels:      meta.Labels,
			Annotations: meta.Annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &cr.Spec.HAProxy.ReplicaCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: meta.Labels,
					Annotations: map[string]string{
						"configmap-hash": calculateHash(generateHAProxyConfigMapData(cc.Ctx, cr)),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "haproxy",
							Image: cr.Spec.HAProxy.Image,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									"cpu":    resource.MustParse("250m"),
									"memory": resource.MustParse("128Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "haproxy-config",
									MountPath: "/usr/local/etc/haproxy/haproxy.cfg",
									SubPath:   "haproxy.cfg",
								},
							},
						},
					},
					ImagePullSecrets: cr.Spec.HAProxy.ImagePullSecrets,
					Volumes: []corev1.Volume{
						{
							Name: "haproxy-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "marklogic-haproxy",
									},
									DefaultMode: &defaultMode,
								},
							},
						},
					},
				},
			},
		},
	}
	if cr.Spec.HAProxy.Affinity != nil {
		deploymentDef.Spec.Template.Spec.Affinity = cr.Spec.HAProxy.Affinity
	}
	if cr.Spec.HAProxy.NodeSelector != nil {
		deploymentDef.Spec.Template.Spec.NodeSelector = cr.Spec.HAProxy.NodeSelector
	}
	if cr.Spec.HAProxy.Tls != nil && cr.Spec.HAProxy.Tls.Enabled && cr.Spec.HAProxy.Tls.SecretName != "" {
		deploymentDef.Spec.Template.Spec.Volumes = append(deploymentDef.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "ssl-certificate",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cr.Spec.HAProxy.Tls.SecretName,
				},
			},
		})
	}
	AddOwnerRefToObject(deploymentDef, ownerDef)
	return deploymentDef
}

// createHAproxy Deployment
func (cc *ClusterContext) createHAProxyDeployment(deploymentDef *appsv1.Deployment) error {
	logger := cc.ReqLogger
	logger.Info("Creating HAProxy Deployment")
	client := cc.Client
	err := client.Create(cc.Ctx, deploymentDef)
	if err != nil {
		logger.Error(err, "HAProxy Deployment creation failed")
		return err
	}
	logger.Info("HAProxy Deployment creation is successful")
	return nil
}

func (cc *ClusterContext) generateHaproxyServiceDef(meta metav1.ObjectMeta) *corev1.Service {
	cr := cc.MarklogicCluster
	defaultPort := []corev1.ServicePort{
		{
			Name:       "qconsole",
			Port:       8000,
			TargetPort: intstr.FromInt(int(8000)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "admin",
			Port:       8001,
			TargetPort: intstr.FromInt(int(8001)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "manage",
			Port:       8002,
			TargetPort: intstr.FromInt(int(8002)),
			Protocol:   corev1.ProtocolTCP,
		},
	}
	servicePort := []corev1.ServicePort{}

	if *cr.Spec.HAProxy.PathBasedRouting {
		servicePort = []corev1.ServicePort{
			{
				Name:       "frontend",
				Port:       cr.Spec.HAProxy.FrontendPort,
				TargetPort: intstr.FromInt(int(cr.Spec.HAProxy.FrontendPort)),
				Protocol:   corev1.ProtocolTCP,
			},
		}
	} else {
		if len(cr.Spec.HAProxy.AppServers) == 0 {
			servicePort = append(servicePort, defaultPort...)
		} else {
			for _, appServer := range cr.Spec.HAProxy.AppServers {
				port := corev1.ServicePort{
					Name: appServer.Name,
					Port: appServer.Port,
				}
				if appServer.TargetPort != 0 {
					port.TargetPort = intstr.FromInt(int(appServer.TargetPort))
				} else {
					port.TargetPort = intstr.FromInt(int(appServer.Port))
				}
				servicePort = append(servicePort, port)
			}
		}
	}
	if cr.Spec.HAProxy.Stats.Enabled {
		servicePort = append(servicePort, corev1.ServicePort{
			Name: "stats",
			Port: cr.Spec.HAProxy.Stats.Port,
		})
	}
	selectorLabels := getHAProxySelectorLabels(cr.GetObjectMeta().GetName())
	serviceDef := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "marklogic-haproxy",
			Namespace:   cc.Request.Namespace,
			Labels:      meta.Labels,
			Annotations: meta.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels,
			Ports:    servicePort,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
	return serviceDef
}

func (cc *ClusterContext) createHAProxyService(serviceDef *corev1.Service) error {
	logger := cc.ReqLogger
	logger.Info("Creating HAProxy Service")
	cr := cc.MarklogicCluster
	ownerDef := marklogicClusterAsOwner(cr)
	client := cc.Client
	AddOwnerRefToObject(serviceDef, ownerDef)
	err := client.Create(cc.Ctx, serviceDef)
	if err != nil {
		logger.Error(err, "HAProxy Service creation failed")
		return err
	}
	logger.Info("HAProxy Service creation is successful")
	return nil
}

func generateHAProxyConfigMap(objectMeta metav1.ObjectMeta, owner metav1.OwnerReference, data map[string]string) *corev1.ConfigMap {

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        objectMeta.Name,
			Namespace:   objectMeta.Namespace,
			Labels:      objectMeta.Labels,
			Annotations: objectMeta.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				owner,
			},
		},
		Data: data,
	}
}

func calculateHash(data map[string]string) string {
	// Create a slice to hold the sorted keys
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}

	// Sort the keys to ensure consistent ordering
	sort.Strings(keys)

	// Create a SHA256 hash
	hash := sha256.New()

	// Iterate over the sorted keys and write key-value pairs to the hash
	for _, k := range keys {
		hash.Write([]byte(k))
		hash.Write([]byte(data[k]))
	}

	// Get the final hash and convert to hexadecimal string
	return hex.EncodeToString(hash.Sum(nil))
}
