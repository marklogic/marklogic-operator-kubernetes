// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	sessionAffinityTestEnvironment = "MARKLOGIC_HAPROXY_SESSION_AFFINITY"
	sessionAffinityNamespace       = "ml-haproxy-session-affinity"
	sessionAffinityClientName      = "session-affinity-client"
	sessionAffinityProxyName       = "session-affinity-haproxy"
	sessionAffinityBackendAName    = "session-affinity-backend-a"
	sessionAffinityBackendBName    = "session-affinity-backend-b"
	sessionAffinityPort            = 8080

	nginxImage   = "nginx:1.27.5-alpine"
	haproxyImage = "haproxytech/haproxy-alpine:3.4.3"
	curlImage    = "curlimages/curl:8.12.1"
)

func TestHAProxySessionIDAffinityContract(t *testing.T) {
	if !sessionAffinityTestEnabledFromEnvironment() {
		t.Skipf("set %s=true to run the HAProxy SessionID affinity contract test", sessionAffinityTestEnvironment)
	}

	t.Cleanup(func() {
		if t.Failed() {
			testutil.CollectKubernetesDiagnostics(t, sessionAffinityNamespace)
		}
		testutil.DeleteNamespace(t, sessionAffinityNamespace)
	})

	testutil.EnsureNamespace(t, sessionAffinityNamespace)
	testutil.ApplyObjects(t, sessionAffinityObjects(sessionAffinityNamespace)...)
	for _, name := range []string{sessionAffinityBackendAName, sessionAffinityBackendBName, sessionAffinityProxyName} {
		testutil.WaitForDeploymentAvailable(t, sessionAffinityNamespace, name, 2*time.Minute)
	}
	testutil.WaitForPodReady(t, sessionAffinityNamespace, sessionAffinityClientName, time.Minute)

	initial := sessionAffinityRequest(t, "curl -sS --retry 12 --retry-delay 1 --retry-connrefused -D /tmp/headers -o /tmp/body -c /tmp/cookies -w '%{http_code}' http://"+sessionAffinityProxyName+":8080/start")
	if initial.status != "302" {
		t.Fatalf("initial response status = %q, want 302; headers: %s", initial.status, initial.headers)
	}
	if !strings.Contains(initial.headers, "SessionID=") {
		t.Fatalf("initial response does not set SessionID: %s", initial.headers)
	}
	if initial.backend == "" {
		t.Fatalf("initial response does not identify a backend: %s", initial.headers)
	}

	affine := sessionAffinityRequest(t, "curl -sS --retry 12 --retry-delay 1 --retry-connrefused -D /tmp/headers -o /tmp/body -b /tmp/cookies -w '%{http_code}' http://"+sessionAffinityProxyName+":8080/callback")
	if affine.status != "200" {
		t.Fatalf("cookie replay status = %q, want 200; headers: %s", affine.status, affine.headers)
	}
	if affine.backend != initial.backend {
		t.Fatalf("SessionID replay backend = %q, want initiating backend %q", affine.backend, initial.backend)
	}

	backends := make(map[string]bool)
	for request := 0; request < 8; request++ {
		response := sessionAffinityRequest(t, "curl -sS --retry 12 --retry-delay 1 --retry-connrefused -D /tmp/headers -o /tmp/body -w '%{http_code}' http://"+sessionAffinityProxyName+":8080/callback")
		if response.status != "200" {
			t.Fatalf("cookie-free request %d status = %q, want 200; headers: %s", request, response.status, response.headers)
		}
		backends[response.backend] = true
	}
	if len(backends) < 2 {
		t.Fatalf("cookie-free requests reached backends %v, want both test backends", backends)
	}
}

type sessionAffinityResponse struct {
	status  string
	headers string
	backend string
}

func sessionAffinityRequest(t *testing.T, request string) sessionAffinityResponse {
	t.Helper()
	testutil.ExecuteInPod(t, sessionAffinityNamespace, sessionAffinityClientName, "curl", "sh", "-ec", request+" > /tmp/status")
	result := testutil.ExecuteInPod(t, sessionAffinityNamespace, sessionAffinityClientName, "curl", "sh", "-ec", `status=$(tail -c 3 /tmp/status 2>/dev/null || true); printf '%s\n---HEADERS---\n' "$status"; cat /tmp/headers`)
	parts := strings.SplitN(result, "\n---HEADERS---\n", 2)
	if len(parts) != 2 {
		t.Fatalf("parse response metadata: %q", result)
	}
	return sessionAffinityResponse{
		status:  strings.TrimSpace(parts[0]),
		headers: parts[1],
		backend: headerValue(parts[1], "X-Test-Backend"),
	}
}

func sessionAffinityTestEnabledFromEnvironment() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(sessionAffinityTestEnvironment)), "true")
}

func headerValue(headers, name string) string {
	prefix := strings.ToLower(name) + ":"
	for _, line := range strings.Split(headers, "\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func sessionAffinityObjects(namespace string) []runtime.Object {
	objects := []runtime.Object{
		sessionAffinityBackendConfig(namespace, sessionAffinityBackendAName),
		sessionAffinityBackendConfig(namespace, sessionAffinityBackendBName),
		sessionAffinityHAProxyConfig(namespace),
	}
	for _, name := range []string{sessionAffinityBackendAName, sessionAffinityBackendBName} {
		objects = append(objects, sessionAffinityBackendDeployment(namespace, name), sessionAffinityService(namespace, name))
	}
	objects = append(objects, sessionAffinityHAProxyDeployment(namespace), sessionAffinityService(namespace, sessionAffinityProxyName), sessionAffinityClient(namespace))
	return objects
}

func sessionAffinityBackendConfig(namespace, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-config", Namespace: namespace},
		Data: map[string]string{"default.conf": fmt.Sprintf(`server {
  listen 8080;
  location = /start {
    add_header Set-Cookie "SessionID=%[1]s; Path=/; HttpOnly" always;
    add_header X-Test-Backend %[1]s always;
    return 302 /callback;
  }
  location = /callback {
    add_header X-Test-Backend %[1]s always;
    return 200 "%[1]s\n";
  }
}`+"\n", name)},
	}
}

func sessionAffinityBackendDeployment(namespace, name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: name, Image: nginxImage, Ports: []corev1.ContainerPort{{ContainerPort: sessionAffinityPort}},
				VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/nginx/conf.d", ReadOnly: true}},
			}}, Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name + "-config"}}}}}}},
		},
	}
}

func sessionAffinityHAProxyConfig(namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sessionAffinityProxyName + "-config", Namespace: namespace},
		Data: map[string]string{"haproxy.cfg": fmt.Sprintf(`global
  log stdout format raw local0
defaults
  mode http
  timeout connect 5s
  timeout client 30s
  timeout server 30s
frontend session-affinity
  bind :8080
	default_backend session-affinity-backend
backend session-affinity-backend
  balance roundrobin
  stick-table type string len 64 size 10k expire 5m
  stick store-response res.cook(SessionID)
  stick match req.cook(SessionID)
  server node-a %[1]s:8080 check
  server node-b %[2]s:8080 check
`, sessionAffinityBackendAName, sessionAffinityBackendBName)},
	}
}

func sessionAffinityHAProxyDeployment(namespace string) *appsv1.Deployment {
	labels := map[string]string{"app": sessionAffinityProxyName}
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: sessionAffinityProxyName, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "haproxy", Image: haproxyImage, Ports: []corev1.ContainerPort{{ContainerPort: sessionAffinityPort}},
				VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/usr/local/etc/haproxy/haproxy.cfg", SubPath: "haproxy.cfg", ReadOnly: true}},
			}}, Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: sessionAffinityProxyName + "-config"}}}}}}},
		},
	}
}

func sessionAffinityService(namespace, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": name}, Ports: []corev1.ServicePort{{Port: sessionAffinityPort, TargetPort: intstr.FromInt(sessionAffinityPort)}}},
	}
}

func sessionAffinityClient(namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sessionAffinityClientName, Namespace: namespace},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "curl", Image: curlImage, Command: []string{"sleep", "infinity"}}}},
	}
}
