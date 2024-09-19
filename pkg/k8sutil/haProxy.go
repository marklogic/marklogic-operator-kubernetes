package k8sutil

import (
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (oc *OperatorContext) ReconcileHAProxy() result.ReconcileResult {
	logger := oc.ReqLogger
	client := oc.Client
	cr := oc.MarklogicGroup

	logger.Info("Reconciling HAProxy Config")
	labels := map[string]string{
		"app.kubernetes.io/instance": "marklogic",
		"app.kubernetes.io/name":     "haproxy",
	}
	annotations := map[string]string{}
	configMapName := "marklogic-haproxy"
	objectMeta := generateObjectMeta(configMapName, cr.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	configmap := &corev1.ConfigMap{}
	err := client.Get(oc.Ctx, nsName, configmap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("HAProxy ConfigMap is not found, creating a new one")
			hAProxyDef := oc.generateHAProxyDef(objectMeta, marklogicServerAsOwner(cr), cr)
			err = oc.createConfigMap(hAProxyDef)
			if err != nil {
				logger.Info("HAProxy configmap creation is failed")
				return result.Error(err)
			}
			logger.Info("HAProxy configmap creation is successful")
			err = oc.createHAProxyDeployment()
			if err != nil {
				logger.Info("HAProxy Deployment creation is failed")
				return result.Error(err)
			}
			// createHAProxyService(service corev1.Service)
			err = oc.createHAProxyService()
			if err != nil {
				logger.Info("HAProxy Service creation is failed")
				return result.Error(err)
			}
			logger.Info("HAProxy Test is successful")
		} else {
			logger.Error(err, "HAProxy configmap creation is failed")
			return result.Error(err)
		}
	}
	return result.Continue()
}

// generateHAProxyDef generates the HAProxy ConfigMap
func (oc *OperatorContext) generateHAProxyDef(objectMeta metav1.ObjectMeta, owner metav1.OwnerReference, cr *databasev1alpha1.MarklogicGroup) *corev1.ConfigMap {
	labels := map[string]string{
		"app.kubernetes.io/component": "haproxy",
	}
	annotations := map[string]string{}
	data := oc.generateHAProxyData(cr)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        objectMeta.Name,
			Namespace:   objectMeta.Namespace,
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{
				owner,
			},
		},
		Data: data,
	}
}

// generateHAProxyData generates the HAProxy Config Data
func (oc *OperatorContext) generateHAProxyData(cr *databasev1alpha1.MarklogicGroup) map[string]string {

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
		"ClientTimeout":  cr.Spec.HAProxyConfig.Timeout.Client,
		"ConnectTimeout": cr.Spec.HAProxyConfig.Timeout.Connect,
		"ServerTimeout":  cr.Spec.HAProxyConfig.Timeout.Server,
	}
	haProxyData["haproxy.cfg"] += parseConfigDef(baseConfig, data) + "\n"

	frontend := generateFrontendConfig(cr)
	backend := generateBackendConfig(cr)
	haProxyData["haproxy.cfg"] += frontend
	haProxyData["haproxy.cfg"] += backend

	if cr.Spec.HAProxyConfig.Stats.Enabled {
		haProxyData["haproxy.cfg"] += generateStatsConfig(cr)
	}

	if cr.Spec.HAProxyConfig.TcpPorts.Enabled {
		haProxyData["haproxy.cfg"] += generateTcpConfig(cr)
	}

	return haProxyData
}

// createHAproxy Deployment
func (oc *OperatorContext) createHAProxyDeployment() error {
	logger := oc.ReqLogger
	logger.Info("Creating HAProxy Deployment")
	labels := map[string]string{
		"app.kubernetes.io/instance": "marklogic",
		"app.kubernetes.io/name":     "haproxy",
	}
	client := oc.Client
	replica := int32(1)
	defaultMode := int32(420)
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogic-haproxy",
			Namespace: "default",
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replica,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance": "marklogic",
					"app.kubernetes.io/name":     "haproxy",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/instance": "marklogic",
						"app.kubernetes.io/name":     "haproxy",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "haproxy",
							Image: "haproxytech/haproxy-alpine:2.9.4",
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
	err := client.Create(oc.Ctx, &deployment)
	if err != nil {
		logger.Error(err, "HAProxy Deployment creation failed")
		return err
	}
	logger.Info("HAProxy Deployment creation is successful")
	return nil

}

// createHAProxyService creates the HAProxy Service
func (oc *OperatorContext) createHAProxyService() error {
	logger := oc.ReqLogger
	logger.Info("Creating HAProxy Service")
	client := oc.Client
	service := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogic-haproxy",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/instance": "marklogic",
				"app.kubernetes.io/name":     "haproxy",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/instance": "marklogic",
				"app.kubernetes.io/name":     "haproxy",
			},
			Ports: []corev1.ServicePort{
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
				{
					Name:       "frontendport",
					Port:       80,
					TargetPort: intstr.FromInt(int(80)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	err := client.Create(oc.Ctx, &service)
	if err != nil {
		logger.Error(err, "HAProxy Service creation failed")
		return err
	}
	logger.Info("HAProxy Service creation is successful")
	return nil
}
