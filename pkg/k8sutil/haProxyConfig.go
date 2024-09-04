package k8sutil

import (
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
	// err := initHAProxyClient("/etc/haproxy/haproxy.cfg")
	err := client.Get(oc.Ctx, nsName, configmap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("HAProxy ConfigMap is not found, creating a new one")
			hAProxyDef := generateHAProxyDef(objectMeta, marklogicServerAsOwner(cr))
			logger.Info("===== Reconciling HA Proxy ==== ", "hAProxyDef:", hAProxyDef)
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
			//=====under development=====//
			/*
				cfg, err := oc.configureHAProxyClient("/etc/haproxy/haproxy.cfg")
				if err != nil {
					logger.Error(err, "HAProxy Client configuration failed")
					return result.Error(err)
				}
				logger.Info("HAProxy Client configuration is successful")
				err = oc.getHAProxyTest(cfg)
				if err != nil {
					logger.Error(err, "HAProxy Test failed")
					return result.Error(err)
				}
				logger.Info("HAProxy Test is successful")
			*/
		} else {
			logger.Error(err, "HAProxy configmap creation is failed")
			return result.Error(err)
		}
	}
	return result.Continue()
}

// generateHAProxyDef generates the HAProxy ConfigMap
func generateHAProxyDef(objectMeta metav1.ObjectMeta, owner metav1.OwnerReference) *corev1.ConfigMap {
	labels := map[string]string{
		"app.kubernetes.io/component": "haproxy",
	}
	annotations := map[string]string{}
	data := getHAProxyData()
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

// func createInt64(x int64) *int64 {
// 	return &x
// }
// func createInt32(x int32) *int32 {
// 	return &x
// }

func getHAProxyData() map[string]string {
	// HAProxy Config with global and defaults for POC purpose
	haProxyData := make(map[string]string)
	haProxyData["haproxy.cfg"] = `
global
	log stdout format raw local0
	maxconn 1024

defaults
	log global
	option forwardfor
	timeout client 600s
	timeout connect 600s
	timeout server 600s

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
	logger.Info("===== HAProxy Deployment ==== ", "deployment:", deployment)
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
					Name:       "dhf-jobs",
					Port:       8010,
					TargetPort: intstr.FromInt(int(8010)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	logger.Info("===== HAProxy Service ==== ", "service:", service)
	err := client.Create(oc.Ctx, &service)
	if err != nil {
		logger.Error(err, "HAProxy Service creation failed")
		return err
	}
	logger.Info("HAProxy Service creation is successful")
	return nil
}

//========Below code is under development========//
/*
// configureHAProxyClient configures the HAProxy client
func (oc *OperatorContext) configureHAProxyClient(haProxyCfgPath string) (cfg configuration.Configuration, err error) {
	logger := oc.ReqLogger
	logger.Info("===== configureHAProxyClient ==== ")

	entries, err := os.ReadDir("./")
	if err != nil {
		logger.Error(err, "Error reading directory")
	}

	for _, e := range entries {
		logger.Info("===== configureHAProxyClient ==== ", "entry:", e.Name())
	}

	confClient, err := configuration.New(oc.Ctx,
		cfg_opt.ConfigurationFile(haProxyCfgPath),
		cfg_opt.HAProxyBin("/usr/local/sbin/haproxy"),
		cfg_opt.UsePersistentTransactions,
		cfg_opt.TransactionsDir("/tmp/haproxy-test"),
	)
	if err != nil {
		logger.Error(err, "Error creating configuration client")
		return nil, err
	}

	opt := []options.Option{
		options.Configuration(confClient),
	}

	cnHAProxyClient, err := client_native.New(context.Background(), opt...)
	if err != nil {
		logger.Error(err, "Error creating client_native client")
		return nil, err
	}

	cfg, err = cnHAProxyClient.Configuration()
	if err != nil {
		logger.Error(err, "Error creating configuration client")
		return nil, err
	}
	ver, err := cfg.GetConfigurationVersion("haproxyv1")
	if err != nil {
		logger.Error(err, "Error getting configuration version")
		return nil, err
	}
	logger.Info("===== configureHAProxyClient ==== ", "ver:", ver)
	return cfg, nil
}

// getHAProxyTest tests the HAProxy configuration
func (oc *OperatorContext) getHAProxyTest(cfg configuration.Configuration) error {
	logger := oc.ReqLogger
	logger.Info("===== getHAProxyTest ==== ")
	intVal, global, err := cfg.GetGlobalConfiguration("haproxyv1")
	if err != nil {
		logger.Error(err, "Error getting global configuration")
		return err
	}
	logger.Info("===== getHAProxyTest ==== ", "global:", global)
	logger.Info("===== getHAProxyTest ==== ", "intVal:", intVal)
	return nil
}
*/
