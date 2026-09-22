// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// This disposable proxy tests MLE-17734, not the operator's generated config.
// Both modes use the same listener, hostname, certificate, and backend servers.
// No request URI, cookies, OAuth state, or credentials are written to proxy logs.
func authCodeProxyConfig(namespace, cluster string) string {
	config := `global
  h1-case-adjust cookie Cookie
defaults
  mode http
  timeout connect 5s
  timeout client 30s
  timeout server 30s
  retries 0
frontend oauth
  bind :8013 ssl crt /tls/tls.pem
  http-request set-var(txn.mode) req.hdr(X-Test-Affinity)
  http-request set-var(txn.session) req.cook(SessionID),sha2(256),hex
  http-request del-header X-Test-Affinity
  use_backend no-affinity if { var(txn.mode) -m str disabled }
  default_backend affinity
`
	for _, mode := range []string{"affinity", "no-affinity"} {
		config += "backend " + mode + "\n  balance roundrobin\n  option h1-case-adjust-bogus-server\n"
		if mode == "affinity" {
			// Learn the application's cookie without inserting, rewriting or truncating it.
			config += "  stick-table type binary len 32 size 10k expire 5m\n  stick store-response res.cook(SessionID),sha2(256)\n  stick match req.cook(SessionID),sha2(256)\n"
		} else {
			// Deterministic fault injection instead of hoping round robin changes nodes.
			// There are no cookie persistence rules in this backend.
			config += "  use-server node-1 if { path /oauth/callback }\n  use-server node-0 unless { path /oauth/callback }\n"
		}
		config += "  http-response set-header X-Test-Backend %[srv_name]\n"
		config += "  http-response set-header X-Test-Affinity " + mode + "\n"
		config += "  http-response set-header X-Test-Session-Hash %[res.cook(SessionID),sha2(256),hex]\n"
		config += "  http-response set-header X-Test-Received-Session-Hash %[var(txn.session)]\n"
		for node := 0; node < 2; node++ {
			hostname := marklogicServerDNSNames(cluster, namespace, node)[0]
			config += fmt.Sprintf("  server node-%d %s:8013 ssl verify required ca-file /ca/cacert.pem verifyhost %s sni str(%s) check\n", node, hostname, hostname, hostname)
		}
	}
	return config
}

func authCodeProxyObjects(namespace, cluster string) []runtime.Object {
	labels := map[string]string{"app": "authcode-test-proxy"}
	replicas := int32(1)
	return []runtime.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "authcode-proxy", Namespace: namespace}, Data: map[string]string{"haproxy.cfg": authCodeProxyConfig(namespace, cluster)}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: haproxyServiceName, Namespace: namespace}, Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "haproxy", Image: haproxyImage,
					Ports:          []corev1.ContainerPort{{ContainerPort: oauthAppServerPort}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(oauthAppServerPort)}}},
					VolumeMounts:   []corev1.VolumeMount{{Name: "config", MountPath: "/usr/local/etc/haproxy", ReadOnly: true}, {Name: "tls", MountPath: "/tls", ReadOnly: true}, {Name: "ca", MountPath: "/ca", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{
					{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "authcode-proxy"}}}},
					{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: haproxyCertificate}}},
					{Name: "ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: oauthCASecretName}}},
				},
			}},
		}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: haproxyServiceName, Namespace: namespace}, Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: oauthAppServerPort, TargetPort: intstr.FromInt(oauthAppServerPort)}}}},
	}
}
