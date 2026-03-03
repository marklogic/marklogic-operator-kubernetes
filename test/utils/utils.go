/*
Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/utils"
)

const (
	prometheusOperatorVersion = "v0.68.0"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/" +
		"releases/download/%s/bundle.yaml"

	certmanagerVersion = "v1.5.3"
	certmanagerURLTmpl = "https://github.com/jetstack/cert-manager/releases/download/%s/cert-manager.yaml"
)

func warnError(err error) {
	fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// validateURL performs basic validation on URLs to prevent command injection
func validateURL(url string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("URL must use HTTPS: %s", url)
	}
	// Basic validation to ensure URL doesn't contain shell metacharacters
	if strings.ContainsAny(url, ";|&$`(){}[]<>") {
		return fmt.Errorf("URL contains invalid characters: %s", url)
	}
	return nil
}

// validateKindClusterName validates cluster names for kind
func validateKindClusterName(name string) error {
	// Kind cluster names should only contain alphanumeric characters and hyphens
	if !regexp.MustCompile(`^[a-zA-Z0-9-]+$`).MatchString(name) {
		return fmt.Errorf("invalid cluster name: %s", name)
	}
	return nil
}

// validateImageName validates Docker image names
func validateImageName(name string) error {
	// Basic validation for Docker image names
	if strings.ContainsAny(name, ";|&$`(){}[]<>") {
		return fmt.Errorf("image name contains invalid characters: %s", name)
	}
	return nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	if err := validateURL(url); err != nil {
		return fmt.Errorf("invalid prometheus operator URL: %w", err)
	}
	// #nosec G204 - URL is validated and constructed from trusted constants
	cmd := exec.Command("kubectl", "create", "-f", url)
	_, err := Run(cmd)
	return err
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) ([]byte, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		fmt.Fprintf(GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return output, nil
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	if err := validateURL(url); err != nil {
		warnError(fmt.Errorf("invalid prometheus operator URL: %w", err))
		return
	}
	// #nosec G204 - URL is validated and constructed from trusted constants
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	if err := validateURL(url); err != nil {
		warnError(fmt.Errorf("invalid cert manager URL: %w", err))
		return
	}
	// #nosec G204 - URL is validated and constructed from trusted constants
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	if err := validateURL(url); err != nil {
		return fmt.Errorf("invalid cert manager URL: %w", err)
	}
	// #nosec G204 - URL is validated and constructed from trusted constants
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// LoadImageToKindCluster loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}

	// Validate inputs to prevent command injection
	if err := validateImageName(name); err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}
	if err := validateKindClusterName(cluster); err != nil {
		return fmt.Errorf("invalid cluster name: %w", err)
	}

	// #nosec G204 - Inputs are validated for safety
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command("kind", kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.Replace(wd, "/test/e2e", "", -1)
	return wd, nil
}

// WaitForPod waits for a pod to be in Running phase, or optionally for Running + Ready condition.
// By default, it only checks for Running phase. Pass true for checkReady to also wait for Ready condition.
// Usage:
//   - WaitForPod(ctx, t, client, ns, name, timeout) // waits for Running only
//   - WaitForPod(ctx, t, client, ns, name, timeout, true) // waits for Running + Ready
func WaitForPod(ctx context.Context, t *testing.T, client klient.Client, namespace, podName string, timeout time.Duration, checkReady ...bool) error {
	// Default to checking only Running phase
	waitForReady := false
	if len(checkReady) > 0 {
		waitForReady = checkReady[0]
	}

	start := time.Now()
	pod := &corev1.Pod{}
	p := utils.RunCommand(`kubectl get ns`)
	t.Logf("Kubernetes namespace: %s", p.Result())
	p = utils.RunCommand("kubectl get pods --namespace " + "marklogic-operator-system" + " -o wide")
	t.Logf("Kubernetes Operator Running Status: %s", p.Result())

	statusMsg := "Running"
	if waitForReady {
		statusMsg = "Ready"
	}

	for {
		t.Logf("Waiting for pod %s in namespace %s to be %s", podName, namespace, statusMsg)
		p := utils.RunCommand("kubectl get pods --namespace " + namespace)
		t.Logf("Kubernetes Pods: %s", p.Result())
		err := client.Resources(namespace).Get(ctx, podName, namespace, pod)

		if err == nil {
			t.Logf("Pod %s is in phase %s", pod.Name, pod.Status.Phase)

			// Check for Running state
			if pod.Status.Phase == corev1.PodRunning {
				if waitForReady {
					// Check if pod is actually Ready
					for _, cond := range pod.Status.Conditions {
						if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
							t.Logf("Pod %s is Ready", pod.Name)
							return nil
						}
					}
					t.Logf("Pod %s is Running but not yet Ready", pod.Name)
				} else {
					// Just checking for Running is sufficient
					t.Logf("Pod %s is Running", pod.Name)
					return nil
				}
			}

			// Diagnostic: Check for failed state
			if pod.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("pod %s entered Failed state: %s", podName, pod.Status.Message)
			}

			// Diagnostic: Check init containers
			for i, initStatus := range pod.Status.InitContainerStatuses {
				if initStatus.State.Waiting != nil {
					t.Logf("Init container [%d] %s is waiting: %s - %s",
						i, initStatus.Name, initStatus.State.Waiting.Reason, initStatus.State.Waiting.Message)
				}
				if initStatus.State.Terminated != nil && initStatus.State.Terminated.ExitCode != 0 {
					t.Errorf("Init container [%d] %s failed with exit code %d: %s",
						i, initStatus.Name, initStatus.State.Terminated.ExitCode, initStatus.State.Terminated.Reason)
					// Get logs from failed init container
					logsCmd := fmt.Sprintf("kubectl logs %s -n %s -c %s --tail=50", podName, namespace, initStatus.Name)
					logResult := utils.RunCommand(logsCmd)
					t.Logf("Failed init container logs:\n%s", logResult.Result())
					return fmt.Errorf("init container %s failed", initStatus.Name)
				}
			}

			// Diagnostic: Check main containers
			for _, containerStatus := range pod.Status.ContainerStatuses {
				if containerStatus.State.Waiting != nil {
					t.Logf("Container %s is waiting: %s - %s",
						containerStatus.Name, containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message)
					if containerStatus.State.Waiting.Reason == "ImagePullBackOff" ||
						containerStatus.State.Waiting.Reason == "ErrImagePull" {
						return fmt.Errorf("image pull failed for container %s: %s",
							containerStatus.Name, containerStatus.State.Waiting.Message)
					}
					// Fail fast on CrashLoopBackOff after multiple restarts
					if containerStatus.State.Waiting.Reason == "CrashLoopBackOff" && containerStatus.RestartCount >= 3 {
						// Get logs from the failing container
						logsCmd := fmt.Sprintf("kubectl logs %s -n %s -c %s --tail=100", podName, namespace, containerStatus.Name)
						logResult := utils.RunCommand(logsCmd)
						t.Logf("Container %s logs:\n%s", containerStatus.Name, logResult.Result())

						// Also try to get previous logs if available
						prevLogsCmd := fmt.Sprintf("kubectl logs %s -n %s -c %s --previous --tail=100", podName, namespace, containerStatus.Name)
						prevLogResult := utils.RunCommand(prevLogsCmd)
						if prevLogResult.Result() != "" {
							t.Logf("Container %s previous logs:\n%s", containerStatus.Name, prevLogResult.Result())
						}

						return fmt.Errorf("container %s is in CrashLoopBackOff with %d restarts: %s",
							containerStatus.Name, containerStatus.RestartCount, containerStatus.State.Waiting.Message)
					}
				}
				// Check for terminated state with non-zero exit code
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
					t.Errorf("Container %s terminated with exit code %d: %s",
						containerStatus.Name, containerStatus.State.Terminated.ExitCode, containerStatus.State.Terminated.Reason)
					// Get logs from terminated container
					logsCmd := fmt.Sprintf("kubectl logs %s -n %s -c %s --tail=100", podName, namespace, containerStatus.Name)
					logResult := utils.RunCommand(logsCmd)
					t.Logf("Terminated container logs:\n%s", logResult.Result())
					return fmt.Errorf("container %s terminated with exit code %d: %s",
						containerStatus.Name, containerStatus.State.Terminated.ExitCode, containerStatus.State.Terminated.Reason)
				}
			}

			// Diagnostic: Check pod conditions
			for _, cond := range pod.Status.Conditions {
				if cond.Status == "False" && cond.Type == "PodScheduled" {
					// Unschedulable due to unbound PVCs is transient; the PVC provisioner
					// may still be creating the volume. Log and continue waiting instead of
					// failing immediately.
					if cond.Reason == "Unschedulable" && strings.Contains(cond.Message, "PersistentVolumeClaim") {
						t.Logf("Pod %s is Unschedulable due to unbound PVCs, waiting for PVC provisioning: %s", podName, cond.Message)
						break
					}
					return fmt.Errorf("pod scheduling failed: %s - %s", cond.Reason, cond.Message)
				}
			}

		} else if !errors.IsNotFound(err) {
			t.Logf("Failed to get pod %s: %v", podName, err)
			continue
		}

		if time.Since(start) > timeout {
			// Enhanced timeout error with pod describe
			describeCmd := fmt.Sprintf("kubectl describe pod %s -n %s", podName, namespace)
			describeResult := utils.RunCommand(describeCmd)
			t.Logf("Pod description:\n%s", describeResult.Result())

			return fmt.Errorf("timed out after %v waiting for pod %s to be %s (current phase: %s)",
				timeout, podName, statusMsg, pod.Status.Phase)
		}

		time.Sleep(5 * time.Second)
	}
}

// util function to get secret data
func GetSecretData(ctx context.Context, client klient.Client, namespace, secretName, username, password string) (string, string, error) {
	secret := &corev1.Secret{}
	err := client.Resources(namespace).Get(ctx, secretName, namespace, secret)
	if err != nil {
		return "", "", fmt.Errorf("Failed to get secret: %s", err)
	}
	usernameSecret, ok := secret.Data[username]
	if !ok {
		return "", "", fmt.Errorf("username not found in secret data")
	}
	passwordSecret, ok := secret.Data[password]
	if !ok {
		return "", "", fmt.Errorf("password not found in secret data")
	}
	return string(usernameSecret), string(passwordSecret), nil
}

func ExecCmdInPod(podName, namespace, containerName, command string) (string, error) {
	cmd := exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", containerName, "--", "sh", "-c", command)

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to execute command: %v, stderr: %s", err, stderr.String())
	}
	return out.String(), nil
}

// GetPodLogs retrieves logs from a specific container in a pod
func GetPodLogs(namespace, podName, containerName string) (string, error) {
	cmd := exec.Command("kubectl", "logs", podName, "-n", namespace, "-c", containerName, "--tail=500")

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to get pod logs: %v, stderr: %s", err, stderr.String())
	}
	return out.String(), nil
}

func AddHelmRepo(chartName, url string) error {
	cmd := exec.Command("helm", "repo", "add", chartName, url)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to add Helm repo: %w", err)
	}
	fmt.Printf("%s helm repo added successfully \n", chartName)
	return nil
}

func InstallHelmChart(releaseName string, chartName string, namespace string, version string, valuesFile ...string) error {
	cmd := exec.Command("helm", "install", releaseName, chartName, "--namespace", namespace, "--create-namespace", "--version", version)
	if valuesFile != nil {
		valuesFilePath := filepath.Join("test", "e2e", "data", valuesFile[0])

		if _, err := os.Stat(valuesFilePath); os.IsNotExist(err) {
			return fmt.Errorf("values file %s does not exist", valuesFilePath)
		}
		cmd.Args = append(cmd.Args, "-f", valuesFilePath)
	}

	fmt.Print(cmd.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to install Helm chart: %w", err)
	}
	fmt.Printf("%s Helm chart installed successfully", chartName)
	return nil
}

func DeleteNS(ctx context.Context, cfg *envconf.Config, nsName string) error {
	nsObj := corev1.Namespace{}
	nsObj.Name = nsName
	err := cfg.Client().Resources().Delete(ctx, &nsObj)
	return err
}
