// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var integrationScheme = newIntegrationScheme()

func newIntegrationScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(marklogicv1.AddToScheme(scheme))
	return scheme
}

// CollectKubernetesDiagnostics logs non-secret resources and container logs for a failed test namespace.
func CollectKubernetesDiagnostics(t *testing.T, namespace string) {
	t.Helper()
	commands := [][]string{
		{"get", "pods,services,ingresses", "-n", namespace, "-o", "wide"},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
		{"get", "pods", "-n", namespace, "-o", "yaml"},
	}
	for _, arguments := range commands {
		output, err := runKubectl(arguments...)
		if err != nil {
			t.Logf("Kubernetes diagnostics command %q failed: %v\n%s", arguments, err, output)
			continue
		}
		t.Logf("Kubernetes diagnostics command %q:\n%s", arguments, output)
	}

	pods, err := runKubectl("get", "pods", "-n", namespace, "-o", "name")
	if err != nil {
		t.Logf("Unable to list pods for diagnostics: %v\n%s", err, pods)
		return
	}
	for _, pod := range nonEmptyLines(pods) {
		output, err := runKubectl("logs", "-n", namespace, pod, "--all-containers", "--tail=500")
		if err != nil {
			t.Logf("Kubernetes diagnostics for %s failed: %v\n%s", pod, err, output)
			continue
		}
		t.Logf("Kubernetes diagnostics for %s:\n%s", pod, output)
	}
}

// DeleteNamespace requests asynchronous cleanup so failed test evidence remains available in test logs.
func DeleteNamespace(t *testing.T, namespace string) {
	t.Helper()
	output, err := runKubectl("delete", "namespace", namespace, "--ignore-not-found", "--wait=false")
	if err != nil {
		t.Logf("Namespace cleanup for %s failed: %v\n%s", namespace, err, output)
	}
}

// EnsureNamespace creates the namespace if it does not already exist.
func EnsureNamespace(t *testing.T, namespace string) {
	t.Helper()
	output, err := runKubectl("create", "namespace", namespace)
	if err != nil && !strings.Contains(output, "AlreadyExists") {
		t.Fatalf("Create namespace %s: %v\n%s", namespace, err, output)
	}
}

// ApplyObjects applies typed Kubernetes resources through kubectl.
func ApplyObjects(t *testing.T, objects ...runtime.Object) {
	t.Helper()
	var manifest bytes.Buffer
	for index, object := range objects {
		if object == nil {
			t.Fatal("Cannot apply a nil Kubernetes object")
		}
		if index > 0 {
			manifest.WriteString("\n---\n")
		}
		contents, err := marshalKubernetesObject(object)
		if err != nil {
			t.Fatalf("Marshal Kubernetes object %T: %v", object, err)
		}
		manifest.Write(contents)
	}
	output, err := runKubectlInput(manifest.Bytes(), "apply", "-f", "-")
	if err != nil {
		t.Fatalf("Apply Kubernetes resources: %v\n%s", err, output)
	}
}

func marshalKubernetesObject(object runtime.Object) ([]byte, error) {
	gvks, _, err := integrationScheme.ObjectKinds(object)
	if err != nil {
		return nil, err
	}
	if len(gvks) != 1 {
		return nil, fmt.Errorf("expected exactly one GroupVersionKind, got %d", len(gvks))
	}
	copy := object.DeepCopyObject()
	copy.GetObjectKind().SetGroupVersionKind(gvks[0])
	return json.Marshal(copy)
}

// WaitForDeploymentAvailable waits for Kubernetes to report a Deployment as available.
func WaitForDeploymentAvailable(t *testing.T, namespace, name string, timeout time.Duration) {
	t.Helper()
	waitForResourceCreation(t, namespace, "deployment", name, timeout)
	output, err := runKubectl("rollout", "status", "deployment/"+name, "-n", namespace, "--timeout="+timeout.String())
	if err != nil {
		t.Fatalf("Wait for deployment %s/%s: %v\n%s", namespace, name, err, output)
	}
}

func WaitForStatefulSetReady(t *testing.T, namespace, name string, timeout time.Duration) {
	t.Helper()
	waitForResourceCreation(t, namespace, "statefulset", name, timeout)
	replicas, err := runKubectl("get", "statefulset/"+name, "-n", namespace, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		t.Fatalf("Get StatefulSet %s/%s replicas: %v\n%s", namespace, name, err, replicas)
	}
	expectedReplicas, err := strconv.Atoi(strings.TrimSpace(replicas))
	if err != nil || expectedReplicas < 1 {
		t.Fatalf("StatefulSet %s/%s replicas = %q, want a positive integer", namespace, name, replicas)
	}
	output, err := runKubectl("wait", "--for=jsonpath={.status.readyReplicas}="+strconv.Itoa(expectedReplicas), "statefulset/"+name, "-n", namespace, "--timeout="+timeout.String())
	if err != nil {
		t.Fatalf("Wait for StatefulSet %s/%s ready replicas: %v\n%s", namespace, name, err, output)
	}
}

func WaitForPodReady(t *testing.T, namespace, name string, timeout time.Duration) {
	t.Helper()
	output, err := runKubectl("wait", "--for=condition=Ready", "pod/"+name, "-n", namespace, "--timeout="+timeout.String())
	if err != nil {
		t.Fatalf("Wait for Pod %s/%s: %v\n%s", namespace, name, err, output)
	}
}

func waitForResourceCreation(t *testing.T, namespace, resource, name string, timeout time.Duration) {
	t.Helper()
	output, err := runKubectl("wait", "--for=create", resource+"/"+name, "-n", namespace, "--timeout="+timeout.String())
	if err != nil {
		t.Fatalf("Wait for %s %s/%s creation: %v\n%s", resource, namespace, name, err, output)
	}
}

// ExecuteInPod runs a command in a ready Pod container and returns its combined output.
func ExecuteInPod(t *testing.T, namespace, name, container string, command ...string) string {
	t.Helper()
	arguments := []string{"exec", "pod/" + name, "-n", namespace}
	if container != "" {
		arguments = append(arguments, "-c", container)
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, command...)
	output, err := runKubectl(arguments...)
	if err != nil {
		t.Fatalf("Execute command in Pod %s/%s: %v\n%s", namespace, name, err, output)
	}
	return output
}

func runKubectl(arguments ...string) (string, error) {
	command := exec.Command("kubectl", arguments...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func runKubectlInput(input []byte, arguments ...string) (string, error) {
	command := exec.Command("kubectl", arguments...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	return string(output), err
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
