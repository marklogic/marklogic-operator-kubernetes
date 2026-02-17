// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	// MarkLogicManagementPort is the default Management API port
	MarkLogicManagementPort = 8002
	// MarkLogicContainerName is the name of the MarkLogic container in pods
	MarkLogicContainerName = "marklogic-server"
)

// ErrMarkLogicNotReady indicates MarkLogic is not ready yet but may become ready soon
// This is a transient error that should trigger a retry rather than immediate failure
var ErrMarkLogicNotReady = errors.New("MarkLogic not ready")

// IsNotReadyError returns true if the error indicates MarkLogic is not ready yet
func IsNotReadyError(err error) bool {
	return errors.Is(err, ErrMarkLogicNotReady)
}

// MarkLogicHealthStatus represents the health status of a MarkLogic cluster
type MarkLogicHealthStatus struct {
	Healthy       bool     `json:"healthy"`
	HostsOnline   int      `json:"hostsOnline"`
	TotalHosts    int      `json:"totalHosts"`
	ForestsOpen   int      `json:"forestsOpen"`
	TotalForests  int      `json:"totalForests"`
	Errors        []string `json:"errors,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	DatabaseCount int      `json:"databaseCount"`
}

// HostStatusResponse represents the response from /manage/v2/hosts?view=status
type HostStatusResponse struct {
	HostStatusList struct {
		StatusListSummary struct {
			TotalHosts struct {
				Value int `json:"value"`
			} `json:"total-hosts"`
			TotalHostsOffline struct {
				Value int `json:"value"`
			} `json:"total-hosts-offline"`
		} `json:"status-list-summary"`
	} `json:"host-status-list"`
}

// ForestStatusResponse represents the response from /manage/v2/forests?view=status
type ForestStatusResponse struct {
	ForestStatusList struct {
		StatusListSummary struct {
			TotalForests struct {
				Value int `json:"value"`
			} `json:"total-forests"`
			StateNotOpen struct {
				Value int `json:"value"`
			} `json:"state-not-open"`
		} `json:"status-list-summary"`
	} `json:"forest-status-list"`
}

// DatabasesResponse represents the response from /manage/v2/databases
type DatabasesResponse struct {
	DatabaseDefaultList struct {
		ListItems struct {
			ListItem []struct {
				IDRef   string `json:"idref"`
				NameRef string `json:"nameref"`
			} `json:"list-item"`
		} `json:"list-items"`
	} `json:"database-default-list"`
}

// ExecInPod executes a command inside a pod and returns the output
// This approach matches the existing pattern used in poststart-hook.sh scripts
// where curl commands are executed inside the MarkLogic container
func (oc *OperatorContext) ExecInPod(podName, namespace, containerName string, command []string) (string, string, error) {
	// Get REST config
	cfg, err := config.GetConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to get kubernetes config: %w", err)
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", "", fmt.Errorf("failed to create clientset: %w", err)
	}

	// Create exec request
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	// Create executor
	exec, err := createExecutor(cfg, req)
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	// Execute command
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(oc.Ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return stdout.String(), stderr.String(), err
}

// createExecutor creates a SPDY executor for remote command execution
func createExecutor(cfg *rest.Config, req *rest.Request) (remotecommand.Executor, error) {
	return remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
}

// GetMarkLogicCredentials retrieves MarkLogic admin credentials from a Secret
func (oc *OperatorContext) GetMarkLogicCredentials() (username, password string, err error) {
	cr := oc.MarklogicGroup

	// Determine secret name
	secretName := cr.Spec.SecretName
	if secretName == "" {
		secretName = cr.Spec.Name
	}

	// Get the secret
	secret := &corev1.Secret{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: cr.Namespace, Name: secretName}, secret); err != nil {
		return "", "", fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	// Extract credentials
	usernameBytes, ok := secret.Data["username"]
	if !ok {
		return "", "", fmt.Errorf("secret %s missing 'username' key", secretName)
	}

	passwordBytes, ok := secret.Data["password"]
	if !ok {
		return "", "", fmt.Errorf("secret %s missing 'password' key", secretName)
	}

	return string(usernameBytes), string(passwordBytes), nil
}

// CheckMarkLogicHealth performs MarkLogic cluster health check by executing curl inside the pod
// This matches the existing pattern used in poststart-hook.sh where curl commands are
// executed inside the MarkLogic container to call the Management API on localhost
func (oc *OperatorContext) CheckMarkLogicHealth() (*MarkLogicHealthStatus, error) {
	logger := oc.ReqLogger
	cr := oc.MarklogicGroup

	status := &MarkLogicHealthStatus{
		Healthy: true,
	}

	// Get credentials
	username, password, err := oc.GetMarkLogicCredentials()
	if err != nil {
		logger.Error(err, "Failed to get MarkLogic credentials for health check")
		return nil, fmt.Errorf("failed to get MarkLogic credentials: %w", err)
	}

	// Use the first pod for health check
	podName := fmt.Sprintf("%s-0", cr.Spec.Name)

	// Check if pod exists and is ready
	pod := &corev1.Pod{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: cr.Namespace, Name: podName}, pod); err != nil {
		// Pod doesn't exist - this is a "not ready" situation, not a failure
		return nil, fmt.Errorf("%w: failed to get pod %s: %v", ErrMarkLogicNotReady, podName, err)
	}

	// Verify pod is running
	if pod.Status.Phase != corev1.PodRunning {
		// Pod not running yet - this is a "not ready" situation
		return nil, fmt.Errorf("%w: pod %s is not running (phase: %s)", ErrMarkLogicNotReady, podName, pod.Status.Phase)
	}

	// Verify the MarkLogic container is Ready (not just Running)
	// A container can be Running but not Ready if it hasn't passed readiness probes yet
	containerReady := false
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name == MarkLogicContainerName {
			if containerStatus.Ready {
				containerReady = true
			} else {
				// Container exists but not ready - MarkLogic is still starting
				// This is a "not ready" situation that should trigger retry
				return nil, fmt.Errorf("%w: MarkLogic container in pod %s is not ready yet (still initializing)", ErrMarkLogicNotReady, podName)
			}
			break
		}
	}
	if !containerReady {
		// Container not found - pod might still be starting
		return nil, fmt.Errorf("%w: MarkLogic container not found in pod %s", ErrMarkLogicNotReady, podName)
	}

	logger.Info("MarkLogic container is ready, performing health check", "pod", podName)

	// Check hosts status
	hostStatus, err := oc.checkHostsViaPodExec(podName, cr.Namespace, username, password)
	if err != nil {
		status.Healthy = false
		status.Errors = append(status.Errors, fmt.Sprintf("Host check failed: %v", err))
	} else {
		status.HostsOnline = hostStatus.online
		status.TotalHosts = hostStatus.total
		// All hosts must be online before resize
		if hostStatus.online != hostStatus.total {
			status.Healthy = false
			status.Errors = append(status.Errors, fmt.Sprintf("Not all hosts are online: %d/%d hosts online", hostStatus.online, hostStatus.total))
		}
	}

	// Check forests status
	forestStatus, err := oc.checkForestsViaPodExec(podName, cr.Namespace, username, password)
	if err != nil {
		status.Healthy = false
		status.Errors = append(status.Errors, fmt.Sprintf("Forest check failed: %v", err))
	} else {
		status.ForestsOpen = forestStatus.open
		status.TotalForests = forestStatus.total
		// All forests must be open before resize
		if forestStatus.open != forestStatus.total {
			status.Healthy = false
			status.Errors = append(status.Errors, fmt.Sprintf("Not all forests are open: %d/%d forests open", forestStatus.open, forestStatus.total))
		}
	}

	// Check database count
	dbCount, err := oc.getDatabaseCountViaPodExec(podName, cr.Namespace, username, password)
	if err != nil {
		status.Warnings = append(status.Warnings, fmt.Sprintf("Database count check failed: %v", err))
	} else {
		status.DatabaseCount = dbCount
	}

	return status, nil
}

type hostCheckResult struct {
	online int
	total  int
}

func (oc *OperatorContext) checkHostsViaPodExec(podName, namespace, username, password string) (*hostCheckResult, error) {
	// Build curl command to check hosts status
	// This matches the pattern used in poststart-hook.sh
	curlCmd := []string{
		"curl", "-s", "-m", "10",
		"--anyauth", "--user", fmt.Sprintf("%s:%s", username, password),
		"-H", "Accept: application/json",
		fmt.Sprintf("http://localhost:%d/manage/v2/hosts?view=status&format=json", MarkLogicManagementPort),
	}

	stdout, stderr, err := oc.ExecInPod(podName, namespace, MarkLogicContainerName, curlCmd)
	if err != nil {
		return nil, fmt.Errorf("curl command failed: %w, stderr: %s", err, stderr)
	}

	if stdout == "" {
		return nil, fmt.Errorf("empty response from hosts endpoint")
	}

	oc.ReqLogger.Info("Host status response received", "response", stdout)

	var hostResp HostStatusResponse
	if err := json.Unmarshal([]byte(stdout), &hostResp); err != nil {
		return nil, fmt.Errorf("failed to parse hosts response: %w, response: %s", err, stdout)
	}

	total := hostResp.HostStatusList.StatusListSummary.TotalHosts.Value
	offline := hostResp.HostStatusList.StatusListSummary.TotalHostsOffline.Value
	online := total - offline

	result := &hostCheckResult{
		total:  total,
		online: online,
	}

	oc.ReqLogger.Info("Parsed host status", "total", total, "online", online, "offline", offline)

	return result, nil
}

type forestCheckResult struct {
	open  int
	total int
}

func (oc *OperatorContext) checkForestsViaPodExec(podName, namespace, username, password string) (*forestCheckResult, error) {
	// Build curl command to check forests status
	// This matches the pattern used in poststart-hook.sh
	curlCmd := []string{
		"curl", "-s", "-m", "10",
		"--anyauth", "--user", fmt.Sprintf("%s:%s", username, password),
		"-H", "Accept: application/json",
		fmt.Sprintf("http://localhost:%d/manage/v2/forests?view=status&format=json", MarkLogicManagementPort),
	}

	stdout, stderr, err := oc.ExecInPod(podName, namespace, MarkLogicContainerName, curlCmd)
	if err != nil {
		return nil, fmt.Errorf("curl command failed: %w, stderr: %s", err, stderr)
	}

	if stdout == "" {
		return nil, fmt.Errorf("empty response from forests endpoint")
	}

	oc.ReqLogger.Info("Forest status response received", "response", stdout)

	var forestResp ForestStatusResponse
	if err := json.Unmarshal([]byte(stdout), &forestResp); err != nil {
		return nil, fmt.Errorf("failed to parse forests response: %w, response: %s", err, stdout)
	}

	total := forestResp.ForestStatusList.StatusListSummary.TotalForests.Value
	notOpen := forestResp.ForestStatusList.StatusListSummary.StateNotOpen.Value
	open := total - notOpen

	result := &forestCheckResult{
		total: total,
		open:  open,
	}

	oc.ReqLogger.Info("Parsed forest status", "total", total, "open", open, "not_open", notOpen)

	return result, nil
}

func (oc *OperatorContext) getDatabaseCountViaPodExec(podName, namespace, username, password string) (int, error) {
	// Build curl command to get databases
	// This matches the pattern used in poststart-hook.sh
	curlCmd := []string{
		"curl", "-s", "-m", "10",
		"--anyauth", "--user", fmt.Sprintf("%s:%s", username, password),
		"-H", "Accept: application/json",
		fmt.Sprintf("http://localhost:%d/manage/v2/databases?format=json", MarkLogicManagementPort),
	}

	stdout, stderr, err := oc.ExecInPod(podName, namespace, MarkLogicContainerName, curlCmd)
	if err != nil {
		return 0, fmt.Errorf("curl command failed: %w, stderr: %s", err, stderr)
	}

	if stdout == "" {
		return 0, fmt.Errorf("empty response from databases endpoint")
	}

	var dbResp DatabasesResponse
	if err := json.Unmarshal([]byte(stdout), &dbResp); err != nil {
		return 0, fmt.Errorf("failed to parse databases response: %w, response: %s", err, stdout)
	}

	return len(dbResp.DatabaseDefaultList.ListItems.ListItem), nil
}
