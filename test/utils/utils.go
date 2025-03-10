/*
Copyright 2024.

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

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
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
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
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

func WaitForPod(ctx context.Context, t *testing.T, client klient.Client, namespace, podName string, timeout time.Duration) error {
	start := time.Now()
	pod := &corev1.Pod{}
	p := utils.RunCommand(`kubectl get ns`)
	t.Logf("Kubernetes namespace: %s", p.Result())
	for {
		t.Logf("Waiting for pod %s in namespace %s ", podName, namespace)
		p := utils.RunCommand("kubectl get pods --namespace " + namespace)
		t.Logf("Kubernetes Pods: %s", p.Result())
		err := client.Resources(namespace).Get(ctx, podName, namespace, pod)
		t.Logf("Pod %s is in phase %s", pod.Name, pod.Status.Phase)
		if err == nil {
			if pod.Status.Phase == "Running" {
				return nil
			}
		} else if !errors.IsNotFound(err) {
			t.Logf("Failed to get pod %s: %v", podName, err)
			continue
		}

		if time.Since(start) > timeout {
			return fmt.Errorf("timed out waiting for pod %s to be created", podName)
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
		return "", fmt.Errorf("failed to execute command: %v, stderr: %v", err, stderr.String())
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
