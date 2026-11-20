// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

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
	haProxyData["haproxy.cfg"] += "  stats socket " + haproxyreload.RuntimeDir + "/worker.sock mode 600 level admin expose-fd listeners\n"
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
				},
				Spec: corev1.PodSpec{
					SecurityContext: getHAProxyPodSecurityContextOrDefault(cr.Spec.HAProxy.PodSecurityContext),
					Containers: []corev1.Container{
						{
							Name:            "haproxy",
							Image:           cr.Spec.HAProxy.Image,
							SecurityContext: getHAProxyContainerSecurityContextOrDefault(cr.Spec.HAProxy.ContainerSecurityContext),
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
					Items: []corev1.KeyToPath{{
						Key:  cr.Spec.HAProxy.Tls.CertFileName,
						Path: cr.Spec.HAProxy.Tls.SecretName + "/" + cr.Spec.HAProxy.Tls.CertFileName,
					}},
				},
			},
		})
		deploymentDef.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deploymentDef.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      "ssl-certificate",
				MountPath: "/usr/local/etc/ssl",
				ReadOnly:  true,
			},
		)
	}
	if cr.Spec.HAProxy.Tolerations != nil && len(cr.Spec.HAProxy.Tolerations) > 0 {
		deploymentDef.Spec.Template.Spec.Tolerations = cr.Spec.HAProxy.Tolerations
	}
	AddOwnerRefToObject(deploymentDef, ownerDef)
	return deploymentDef
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
