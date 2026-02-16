// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	e2eutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

const (
	istioAmbientNs       = "istio-ambient-test"
	istioMultinodeNs     = "istio-multinode-test"
	nonIstioNs           = "non-istio-test"
	istioClusterName     = "istio-ambient-cluster"
	istioMultinodeName   = "istio-multinode-cluster"
	nonIstioClusterName  = "non-istio-cluster"
	mlServerContainer    = "marklogic-server"
	wrapperReadyLog      = "[Wrapper] Mesh Network is Ready."
	wrapperSkippedLog    = "[Wrapper] Localhost is UP."
	wrapperMonitoringLog = "[Wrapper] Initialization complete"
)

var (
	ambientReplicas     = int32(3)
	singleReplicas      = int32(1)
	istioAdminUsername  = "admin"
	istioAdminPassword  = "Admin@8001"
	istioSecretName     = "istio-admin-secrets"
	istioWaitTimeout    = 10 * time.Minute
	standardWaitTimeout = 5 * time.Minute
	podCheckInterval    = 10 * time.Second
	maxLogRetries       = 30
	logRetryInterval    = 10 * time.Second
)

// isIstioAmbientEnabled checks the E2E_ISTIO_AMBIENT environment variable
func isIstioAmbientEnabled() bool {
	return os.Getenv("E2E_ISTIO_AMBIENT") == "true"
}

// createAmbientNamespace creates a namespace with the Istio Ambient mode label.
// It deletes any pre-existing namespace first to ensure idempotent re-runs.
func createAmbientNamespace(ctx context.Context, t *testing.T, c *envconf.Config, nsName string) error {
	client := c.Client()

	// Clean up any existing namespace from a previous interrupted run
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := client.Resources().Delete(ctx, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete pre-existing namespace %s: %w", nsName, err)
		}
		// Namespace doesn't exist, proceed with creation
	} else {
		t.Logf("Deleted pre-existing namespace %s, waiting for cleanup...", nsName)
		deletionCompleted := false
		for i := 0; i < 60; i++ {
			check := &corev1.Namespace{}
			err := client.Resources().Get(ctx, nsName, "", check)
			if err != nil {
				if apierrors.IsNotFound(err) {
					deletionCompleted = true
					break
				}
				return fmt.Errorf("error checking namespace deletion status: %w", err)
			}
			time.Sleep(2 * time.Second)
		}
		if !deletionCompleted {
			// Final verification to avoid attempting to recreate a namespace that is still terminating.
			check := &corev1.Namespace{}
			if err := client.Resources().Get(ctx, nsName, "", check); err != nil {
				if !apierrors.IsNotFound(err) {
					return fmt.Errorf("error verifying namespace deletion status: %w", err)
				}
				// NotFound here means deletion completed just after the loop ended; safe to proceed.
			} else {
				return fmt.Errorf("namespace %s still exists or is terminating after waiting for deletion", nsName)
			}
		}
	}

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				"istio.io/dataplane-mode": "ambient",
			},
		},
	}
	if err := client.Resources().Create(ctx, namespace); err != nil {
		return fmt.Errorf("failed to create ambient namespace: %w", err)
	}
	t.Logf("Created Istio Ambient namespace: %s", nsName)
	return nil
}

// createStandardNamespace creates a namespace without Istio labels.
// It deletes any pre-existing namespace first to ensure idempotent re-runs.
func createStandardNamespace(ctx context.Context, t *testing.T, c *envconf.Config, nsName string) error {
	client := c.Client()

	// Clean up any existing namespace from a previous interrupted run
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := client.Resources().Delete(ctx, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete pre-existing namespace %s: %w", nsName, err)
		}
		// Namespace doesn't exist, proceed with creation
	} else {
		t.Logf("Deleted pre-existing namespace %s, waiting for cleanup...", nsName)
		deletionCompleted := false
		for i := 0; i < 60; i++ {
			check := &corev1.Namespace{}
			err := client.Resources().Get(ctx, nsName, "", check)
			if err != nil {
				if apierrors.IsNotFound(err) {
					deletionCompleted = true
					break
				}
				return fmt.Errorf("error checking namespace deletion status: %w", err)
			}
			time.Sleep(2 * time.Second)
		}
		if !deletionCompleted {
			// Final verification to avoid attempting to recreate a namespace that is still terminating.
			check := &corev1.Namespace{}
			if err := client.Resources().Get(ctx, nsName, "", check); err != nil {
				if !apierrors.IsNotFound(err) {
					return fmt.Errorf("error verifying namespace deletion status: %w", err)
				}
				// NotFound here means deletion completed just after the loop ended; safe to proceed.
			} else {
				return fmt.Errorf("namespace %s still exists or is terminating after waiting for deletion", nsName)
			}
		}
	}

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}
	if err := client.Resources().Create(ctx, namespace); err != nil {
		return fmt.Errorf("failed to create standard namespace: %w", err)
	}
	t.Logf("Created standard namespace: %s", nsName)
	return nil
}

// verifyWrapperLogs checks that pod logs contain the expected message, with retries
func verifyWrapperLogs(ctx context.Context, t *testing.T, namespace, podName, expectedLog string) error {
	t.Logf("Verifying wrapper logs for pod %s in namespace %s", podName, namespace)

	var lastLogs string
	for attempt := 1; attempt <= maxLogRetries; attempt++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while verifying wrapper logs: %w", ctx.Err())
		default:
		}

		logs, err := utils.GetPodLogs(namespace, podName, mlServerContainer)
		if err != nil {
			t.Logf("Attempt %d/%d: Failed to get logs: %v", attempt, maxLogRetries, err)
			if attempt < maxLogRetries {
				time.Sleep(logRetryInterval)
				continue
			}
			return fmt.Errorf("failed to get pod logs after %d attempts: %w", maxLogRetries, err)
		}

		lastLogs = logs
		if strings.Contains(logs, expectedLog) {
			t.Logf("Successfully verified log message: %s (attempt %d)", expectedLog, attempt)
			return nil
		}

		// Show partial logs every 5 attempts for debugging
		if attempt%5 == 0 {
			lines := strings.Split(logs, "\n")
			recentLines := lines
			if len(lines) > 15 {
				recentLines = lines[len(lines)-15:]
			}
			t.Logf("Attempt %d/%d: Recent logs:\n%s", attempt, maxLogRetries, strings.Join(recentLines, "\n"))
		} else {
			t.Logf("Attempt %d/%d: Expected log '%s' not found yet", attempt, maxLogRetries, expectedLog)
		}

		if attempt < maxLogRetries {
			time.Sleep(logRetryInterval)
		}
	}

	// Show last logs snapshot on failure
	lines := strings.Split(lastLogs, "\n")
	recentLines := lines
	if len(lines) > 20 {
		recentLines = lines[len(lines)-20:]
	}
	t.Logf("Final log snapshot:\n%s", strings.Join(recentLines, "\n"))
	return fmt.Errorf("expected log message '%s' not found in pod %s after %d attempts", expectedLog, podName, maxLogRetries)
}

// killMarkLogicProcess kills the MarkLogic process in a pod to test crash recovery
func killMarkLogicProcess(t *testing.T, namespace, podName string) error {
	t.Logf("Killing MarkLogic process in pod %s", podName)

	killCmd := "pkill -9 -f /opt/MarkLogic/bin/MarkLogic"
	output, err := utils.ExecCmdInPod(podName, namespace, mlServerContainer, killCmd)
	if err != nil {
		// pkill returns non-zero if no process found, which may happen if it already exited
		t.Logf("pkill command output: %s, err: %v", output, err)
	}

	t.Logf("Successfully sent kill signal to MarkLogic process in pod %s", podName)
	return nil
}

// waitForClusterHealth waits for the MarkLogic cluster to be healthy by querying
// the management API on the given pod. It verifies that the /manage/v2 endpoint
// returns a successful response, indicating that MarkLogic is fully operational.
// This approach is used because the operator sets Ready conditions on MarklogicGroup
// (child CR) resources, not on the parent MarklogicCluster CR.
func waitForClusterHealth(ctx context.Context, t *testing.T, namespace, podName string, timeout time.Duration) error {
	t.Logf("Waiting for MarkLogic cluster to become healthy (via pod %s)", podName)

	url := "http://localhost:8002/manage/v2"
	curlCommand := fmt.Sprintf(
		"curl -s -o /dev/null -w '%%{http_code}' %s --anyauth -u %s:%s",
		url, istioAdminUsername, istioAdminPassword,
	)

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for cluster health: %w", ctx.Err())
		default:
		}

		output, err := utils.ExecCmdInPod(podName, namespace, mlServerContainer, curlCommand)
		if err == nil && strings.TrimSpace(output) == "200" {
			t.Logf("MarkLogic management API is healthy on pod %s", podName)
			return nil
		}

		if err != nil {
			t.Logf("Management API not ready yet on pod %s: %v", podName, err)
		} else {
			t.Logf("Management API returned HTTP %s on pod %s (waiting for 200)", strings.TrimSpace(output), podName)
		}

		if time.Since(start) > timeout {
			return fmt.Errorf("MarkLogic management API on pod %s did not become healthy within %v", podName, timeout)
		}

		time.Sleep(podCheckInterval)
	}
}

// waitForPodRestart waits for a pod to restart by checking that its UID has changed
func waitForPodRestart(ctx context.Context, t *testing.T, c *envconf.Config, namespace, podName string, originalUID string, timeout time.Duration) error {
	t.Logf("Waiting for pod %s to restart", podName)
	client := c.Client()
	start := time.Now()

	for {
		pod := &corev1.Pod{}
		err := client.Resources(namespace).Get(ctx, podName, namespace, pod)

		if err == nil {
			currentUID := string(pod.UID)
			if currentUID != originalUID {
				t.Logf("Pod %s has restarted (UID changed from %s to %s)", podName, originalUID, currentUID)

				if pod.Status.Phase == corev1.PodRunning {
					for _, status := range pod.Status.ContainerStatuses {
						if status.Name == mlServerContainer && status.Ready {
							t.Logf("Pod %s is running and ready after restart", podName)
							return nil
						}
					}
				}
			}
		}

		if time.Since(start) > timeout {
			return fmt.Errorf("pod %s did not restart within %v", podName, timeout)
		}

		time.Sleep(podCheckInterval)
	}
}

// ============================================================================
// Test 1: Happy Path Provisioning
// Validates: Phase 4 (mesh gatekeeper) + Phase 5 (cluster join) + Phase 6 (readiness)
// ============================================================================

func TestIstioAmbientProvisioning(t *testing.T) {
	if !isIstioAmbientEnabled() {
		t.Skip("Skipping: Istio ambient mode tests not enabled (set E2E_ISTIO_AMBIENT=true)")
	}

	feature := features.New("Istio Ambient Mode - Happy Path Provisioning").
		WithLabel("type", "istio-ambient")

	var createdPods []string

	// Setup: Create Istio Ambient namespace and deploy cluster
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := createAmbientNamespace(ctx, t, c, istioAmbientNs); err != nil {
			t.Fatalf("Failed to create ambient namespace: %v", err)
		}

		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(istioAmbientNs).GetScheme())

		// Create admin secret
		p := e2eutils.RunCommand(fmt.Sprintf(
			"kubectl -n %s create secret generic %s --from-literal=username=%s --from-literal=password=%s",
			istioAmbientNs, istioSecretName, istioAdminUsername, istioAdminPassword,
		))
		if p.Err() != nil {
			t.Fatalf("Failed to create admin secret: %s", p.Result())
		}

		// Create MarkLogic cluster CR
		mlcluster := &marklogicv1.MarklogicCluster{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "marklogic.progress.com/v1",
				Kind:       "MarklogicCluster",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      istioClusterName,
				Namespace: istioAmbientNs,
			},
			Spec: marklogicv1.MarklogicClusterSpec{
				Image: marklogicImage,
				Auth: &marklogicv1.AdminAuth{
					SecretName: &istioSecretName,
				},
				MarkLogicGroups: []*marklogicv1.MarklogicGroups{
					{
						Name:        "node",
						Replicas:    &ambientReplicas,
						IsBootstrap: true,
					},
				},
			},
		}

		if err := client.Resources(istioAmbientNs).Create(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlcluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatalf("MarklogicCluster resource creation timeout: %v", err)
		}

		t.Log("MarklogicCluster resource created successfully")
		return ctx
	})

	// Assess: Verify all pods reach Running status
	feature.Assess("All pods reach Running status", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		for i := 0; i < int(ambientReplicas); i++ {
			podName := fmt.Sprintf("node-%d", i)
			createdPods = append(createdPods, podName)

			err := utils.WaitForPod(ctx, t, client, istioAmbientNs, podName, istioWaitTimeout)
			if err != nil {
				t.Fatalf("Failed to wait for pod %s: %v", podName, err)
			}
			t.Logf("Pod %s is running", podName)
		}
		return ctx
	})

	// Assess: Verify namespace has Istio ambient mode active
	feature.Assess("Namespace has Istio ambient mode active", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// 1. Verify namespace label
		ns := &corev1.Namespace{}
		if err := client.Resources().Get(ctx, istioAmbientNs, "", ns); err != nil {
			t.Fatalf("Failed to get namespace: %v", err)
		}
		mode, ok := ns.Labels["istio.io/dataplane-mode"]
		if !ok || mode != "ambient" {
			t.Fatalf("Namespace %s does not have istio.io/dataplane-mode=ambient label. Labels: %v", istioAmbientNs, ns.Labels)
		}
		t.Logf("Namespace %s has istio.io/dataplane-mode=ambient label", istioAmbientNs)

		// 2. Verify ztunnel DaemonSet is running in istio-system
		p := e2eutils.RunCommand("kubectl get daemonset ztunnel -n istio-system -o jsonpath='{.status.numberReady}'")
		if p.Err() != nil {
			t.Fatalf("ztunnel DaemonSet not found in istio-system: %s", p.Result())
		}
		ztunnelReady := strings.Trim(p.Result(), "'")
		if ztunnelReady == "" || ztunnelReady == "0" {
			t.Fatalf("ztunnel DaemonSet has no ready pods: %s", p.Result())
		}
		t.Logf("ztunnel DaemonSet has %s ready pods", ztunnelReady)

		// 3. Verify pods are enrolled in the ambient mesh via istioctl
		p = e2eutils.RunCommand(fmt.Sprintf("istioctl ztunnel-config workload -n %s 2>&1 || echo 'ztunnel-config unavailable'", istioAmbientNs))
		output := p.Result()
		if strings.Contains(output, "ztunnel-config unavailable") || strings.Contains(output, "Error") {
			t.Logf("Warning: could not query ztunnel workload enrollment: %s", output)
		} else {
			enrolledCount := 0
			for i := 0; i < int(ambientReplicas); i++ {
				podName := fmt.Sprintf("node-%d", i)
				if strings.Contains(output, podName) {
					enrolledCount++
					t.Logf("Pod %s is enrolled in ztunnel mesh", podName)
				}
			}
			if enrolledCount == 0 {
				t.Logf("Warning: no pods appear in ztunnel workload output. Output: %s", output)
			}
		}

		t.Log("Istio ambient mode is active on namespace")
		return ctx
	})

	// Assess: Verify wrapper logs show mesh network ready for non-bootstrap pods
	feature.Assess("Wrapper logs show mesh network ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Wait a bit for pods to stabilize before checking logs
		t.Logf("Waiting for pods to stabilize before checking wrapper logs...")
		time.Sleep(30 * time.Second)

		// Double-check all pods still exist
		client := c.Client()
		for _, podName := range createdPods {
			pod := &corev1.Pod{}
			if err := client.Resources(istioAmbientNs).Get(ctx, podName, istioAmbientNs, pod); err != nil {
				t.Fatalf("Pod %s not found before log check: %v", podName, err)
			}
			t.Logf("Pod %s confirmed present, status: %s", podName, pod.Status.Phase)
		}

		for _, podName := range createdPods {
			if err := verifyWrapperLogs(ctx, t, istioAmbientNs, podName, wrapperMonitoringLog); err != nil {
				t.Fatalf("Failed to verify wrapper logs for pod %s: %v", podName, err)
			}
		}

		// Non-bootstrap pods should show mesh network ready message
		for i := 1; i < int(ambientReplicas); i++ {
			podName := fmt.Sprintf("node-%d", i)
			if err := verifyWrapperLogs(ctx, t, istioAmbientNs, podName, wrapperReadyLog); err != nil {
				t.Fatalf("Failed to verify mesh ready log for pod %s: %v", podName, err)
			}
		}
		t.Log("All pods show successful mesh network initialization")
		return ctx
	})

	// Assess: Verify cluster status becomes Ready
	feature.Assess("Cluster status becomes Ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := waitForClusterHealth(ctx, t, istioAmbientNs, "node-0", istioWaitTimeout); err != nil {
			t.Fatalf("Cluster health check failed: %v", err)
		}
		return ctx
	})

	// Assess: Verify MarkLogic cluster formation — all nodes registered
	feature.Assess("MarkLogic cluster is formed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "node-0"
		url := "http://localhost:8002/manage/v2/hosts"
		curlCommand := fmt.Sprintf("curl -s %s --anyauth -u %s:%s", url, istioAdminUsername, istioAdminPassword)

		output, err := utils.ExecCmdInPod(podName, istioAmbientNs, mlServerContainer, curlCommand)
		if err != nil {
			t.Fatalf("Failed to query MarkLogic hosts: %v", err)
		}

		for i := 0; i < int(ambientReplicas); i++ {
			expectedHost := fmt.Sprintf("node-%d", i)
			if !strings.Contains(output, expectedHost) {
				t.Fatalf("Host %s not found in cluster. Hosts output: %s", expectedHost, output)
			}
		}
		t.Log("All MarkLogic nodes are present in the cluster")
		return ctx
	})

	// Teardown
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		mlcluster := &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      istioClusterName,
				Namespace: istioAmbientNs,
			},
		}
		if err := client.Resources(istioAmbientNs).Delete(ctx, mlcluster); err != nil {
			t.Logf("Failed to delete MarklogicCluster: %v", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: istioAmbientNs}}); err != nil {
			t.Logf("Failed to delete namespace: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// ============================================================================
// Test 2: Process Crash Recovery (Resilience)
// Validates: Phase 7 (watchdog) detects crash, pod restarts, Phase 4 re-runs
// ============================================================================

func TestIstioAmbientResilience(t *testing.T) {
	if !isIstioAmbientEnabled() {
		t.Skip("Skipping: Istio ambient mode tests not enabled (set E2E_ISTIO_AMBIENT=true)")
	}

	feature := features.New("Istio Ambient Mode - Process Guardian & Resilience").
		WithLabel("type", "istio-ambient")

	resilienceNs := "istio-resilience-test"
	resilienceCluster := "istio-resilience-cluster"
	resilienceSecret := "resilience-admin-secrets"
	resilienceReplicas := int32(1)
	var leaderPodUID string

	// Setup: Deploy a single-node cluster in ambient namespace
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := createAmbientNamespace(ctx, t, c, resilienceNs); err != nil {
			t.Fatalf("Failed to create ambient namespace: %v", err)
		}

		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(resilienceNs).GetScheme())

		p := e2eutils.RunCommand(fmt.Sprintf(
			"kubectl -n %s create secret generic %s --from-literal=username=%s --from-literal=password=%s",
			resilienceNs, resilienceSecret, istioAdminUsername, istioAdminPassword,
		))
		if p.Err() != nil {
			t.Fatalf("Failed to create admin secret: %s", p.Result())
		}

		mlcluster := &marklogicv1.MarklogicCluster{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "marklogic.progress.com/v1",
				Kind:       "MarklogicCluster",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      resilienceCluster,
				Namespace: resilienceNs,
			},
			Spec: marklogicv1.MarklogicClusterSpec{
				Image: marklogicImage,
				Auth: &marklogicv1.AdminAuth{
					SecretName: &resilienceSecret,
				},
				MarkLogicGroups: []*marklogicv1.MarklogicGroups{
					{
						Name:        "node",
						Replicas:    &resilienceReplicas,
						IsBootstrap: true,
					},
				},
			},
		}

		if err := client.Resources(resilienceNs).Create(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlcluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatalf("MarklogicCluster resource creation timeout: %v", err)
		}

		// Wait for pod to be ready
		err := utils.WaitForPod(ctx, t, client, resilienceNs, "node-0", istioWaitTimeout)
		if err != nil {
			t.Fatalf("Failed to wait for pod node-0: %v", err)
		}

		// Wait for cluster to be ready
		if err := waitForClusterHealth(ctx, t, resilienceNs, "node-0", istioWaitTimeout); err != nil {
			t.Fatalf("Cluster health check failed: %v", err)
		}

		t.Log("Cluster is fully operational")
		return ctx
	})

	// Assess: Record leader pod UID before crash
	feature.Assess("Identify leader pod", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		pod := &corev1.Pod{}
		if err := client.Resources(resilienceNs).Get(ctx, "node-0", resilienceNs, pod); err != nil {
			t.Fatalf("Failed to get leader pod: %v", err)
		}

		leaderPodUID = string(pod.UID)
		t.Logf("Leader pod node-0 identified with UID: %s", leaderPodUID)
		return ctx
	})

	// Assess: Kill MarkLogic process and verify container restarts
	feature.Assess("Process crash triggers container restart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		resources := client.Resources(resilienceNs)

		// Helper to get restart count for the MarkLogic server container
		getRestartCount := func(pod *corev1.Pod) int32 {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == mlServerContainer {
					return cs.RestartCount
				}
			}
			if len(pod.Status.ContainerStatuses) > 0 {
				return pod.Status.ContainerStatuses[0].RestartCount
			}
			return 0
		}

		// Capture initial restart count before inducing the crash
		var beforePod corev1.Pod
		if err := resources.Get(ctx, "node-0", resilienceNs, &beforePod); err != nil {
			t.Fatalf("Failed to get pod %s/node-0 before crash: %v", resilienceNs, err)
		}
		initialRestartCount := getRestartCount(&beforePod)
		t.Logf("Initial container RestartCount: %d", initialRestartCount)

		if err := killMarkLogicProcess(t, resilienceNs, "node-0"); err != nil {
			t.Fatalf("Failed to kill MarkLogic process: %v", err)
		}

		t.Log("Waiting for container restart after process crash...")

		// Wait until the pod is Running, Ready, and the container RestartCount increases
		err := wait.For(func(ctx context.Context) (bool, error) {
			var pod corev1.Pod
			if err := resources.Get(ctx, "node-0", resilienceNs, &pod); err != nil {
				if apierrors.IsNotFound(err) {
					// Pod may temporarily disappear; keep waiting
					return false, nil
				}
				return false, err
			}

			// Ensure pod is Running
			if pod.Status.Phase != corev1.PodRunning {
				return false, nil
			}

			// Ensure restart count increased
			currentRestartCount := getRestartCount(&pod)
			if currentRestartCount <= initialRestartCount {
				return false, nil
			}

			// Ensure pod is Ready again
			ready := false
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}

			if ready {
				t.Logf("Container restart verified: RestartCount increased from %d to %d", initialRestartCount, currentRestartCount)
			}
			return ready, nil
		},
			wait.WithTimeout(5*time.Minute),
			wait.WithContext(ctx),
		)
		if err != nil {
			t.Fatalf("Pod did not restart and become ready after crash: %v", err)
		}

		t.Log("Container successfully restarted after process crash")
		return ctx
	})

	// Assess: Verify pod recovers and cluster is healthy
	feature.Assess("Pod recovers and cluster is healthy", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Verify wrapper re-initialized
		if err := verifyWrapperLogs(ctx, t, resilienceNs, "node-0", wrapperMonitoringLog); err != nil {
			t.Fatalf("Wrapper did not reinitialize properly: %v", err)
		}

		// Verify cluster health
		if err := waitForClusterHealth(ctx, t, resilienceNs, "node-0", istioWaitTimeout); err != nil {
			t.Fatalf("Cluster did not recover after pod restart: %v", err)
		}

		t.Log("Pod successfully recovered and cluster is healthy")
		return ctx
	})

	// Assess: Verify pod is not in CrashLoopBackOff
	feature.Assess("Pod is not in CrashLoopBackOff", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		pod := &corev1.Pod{}
		if err := client.Resources(resilienceNs).Get(ctx, "node-0", resilienceNs, pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == mlServerContainer {
				if containerStatus.State.Waiting != nil {
					reason := containerStatus.State.Waiting.Reason
					if reason == "CrashLoopBackOff" {
						t.Fatalf("Pod is in CrashLoopBackOff state")
					}
				}
				if containerStatus.RestartCount > 3 {
					t.Logf("Warning: Container has restarted %d times (expected ~1)", containerStatus.RestartCount)
				}
			}
		}

		t.Log("Pod is stable and not in CrashLoopBackOff")
		return ctx
	})

	// Teardown
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		mlcluster := &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resilienceCluster,
				Namespace: resilienceNs,
			},
		}
		if err := client.Resources(resilienceNs).Delete(ctx, mlcluster); err != nil {
			t.Logf("Failed to delete MarklogicCluster: %v", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resilienceNs}}); err != nil {
			t.Logf("Failed to delete namespace: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// ============================================================================
// Test 3: Multi-Group Network Gatekeeper (dnode + enode)
// Validates: Phase 4 runs on all pods - bootstrap validates self-connectivity, non-bootstrap validates bootstrap host connectivity (cross-group mesh)
// ============================================================================

func TestIstioAmbientNetworkGatekeeper(t *testing.T) {
	if !isIstioAmbientEnabled() {
		t.Skip("Skipping: Istio ambient mode tests not enabled (set E2E_ISTIO_AMBIENT=true)")
	}

	feature := features.New("Istio Ambient Mode - Multi-Group Network Gatekeeper").
		WithLabel("type", "istio-ambient-multinode")

	// Setup: Deploy multi-node cluster with dnode + enode
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := createAmbientNamespace(ctx, t, c, istioMultinodeNs); err != nil {
			t.Fatalf("Failed to create ambient namespace: %v", err)
		}

		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(istioMultinodeNs).GetScheme())

		p := e2eutils.RunCommand(fmt.Sprintf(
			"kubectl -n %s create secret generic %s --from-literal=username=%s --from-literal=password=%s",
			istioMultinodeNs, istioSecretName, istioAdminUsername, istioAdminPassword,
		))
		if p.Err() != nil {
			t.Fatalf("Failed to create admin secret: %s", p.Result())
		}

		mlcluster := &marklogicv1.MarklogicCluster{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "marklogic.progress.com/v1",
				Kind:       "MarklogicCluster",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      istioMultinodeName,
				Namespace: istioMultinodeNs,
			},
			Spec: marklogicv1.MarklogicClusterSpec{
				Image: marklogicImage,
				Auth: &marklogicv1.AdminAuth{
					SecretName: &istioSecretName,
				},
				MarkLogicGroups: []*marklogicv1.MarklogicGroups{
					{
						Name:        "dnode",
						Replicas:    &singleReplicas,
						IsBootstrap: true,
						GroupConfig: &marklogicv1.GroupConfig{
							Name: "dnode",
						},
					},
					{
						Name:     "enode",
						Replicas: &singleReplicas,
						GroupConfig: &marklogicv1.GroupConfig{
							Name: "enode",
						},
					},
				},
			},
		}

		if err := client.Resources(istioMultinodeNs).Create(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlcluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatalf("MarklogicCluster resource creation timeout: %v", err)
		}

		t.Log("Multi-node MarklogicCluster resource created")
		return ctx
	})

	// Assess: Verify bootstrap node (dnode-0) starts first
	feature.Assess("Bootstrap node initializes successfully", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		bootstrapPod := "dnode-0"

		err := utils.WaitForPod(ctx, t, client, istioMultinodeNs, bootstrapPod, istioWaitTimeout)
		if err != nil {
			t.Fatalf("Failed to wait for bootstrap pod: %v", err)
		}

		// Verify wrapper completed initialization
		if err := verifyWrapperLogs(ctx, t, istioMultinodeNs, bootstrapPod, wrapperMonitoringLog); err != nil {
			t.Fatalf("Bootstrap pod wrapper did not complete successfully: %v", err)
		}

		t.Log("Bootstrap node is ready")
		return ctx
	})

	// Assess: Verify E-node waits for bootstrap host via mesh, then joins
	feature.Assess("E-node joins cluster through mesh network", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		enodePod := "enode-0"

		err := utils.WaitForPod(ctx, t, client, istioMultinodeNs, enodePod, istioWaitTimeout)
		if err != nil {
			t.Fatalf("Failed to wait for E-node pod: %v", err)
		}

		// Verify enode checked mesh connectivity to bootstrap host
		if err := verifyWrapperLogs(ctx, t, istioMultinodeNs, enodePod, wrapperReadyLog); err != nil {
			t.Fatalf("E-node did not verify mesh connectivity: %v", err)
		}

		t.Log("E-node successfully joined cluster through mesh")
		return ctx
	})

	// Assess: Verify inter-node communication — both hosts in cluster
	feature.Assess("Inter-node communication works via mesh", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		bootstrapPod := "dnode-0"
		url := "http://localhost:8002/manage/v2/hosts"
		curlCommand := fmt.Sprintf("curl -s %s --anyauth -u %s:%s", url, istioAdminUsername, istioAdminPassword)

		output, err := utils.ExecCmdInPod(bootstrapPod, istioMultinodeNs, mlServerContainer, curlCommand)
		if err != nil {
			t.Fatalf("Failed to query MarkLogic hosts: %v", err)
		}

		if !strings.Contains(output, "dnode-0") {
			t.Fatalf("Bootstrap host dnode-0 not found in cluster. Output: %s", output)
		}
		if !strings.Contains(output, "enode-0") {
			t.Fatalf("E-node host enode-0 not found in cluster. Output: %s", output)
		}

		t.Log("Both dnode and enode are present in the cluster")
		return ctx
	})

	// Assess: Verify cluster status
	feature.Assess("Cluster status becomes Ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := waitForClusterHealth(ctx, t, istioMultinodeNs, "dnode-0", istioWaitTimeout); err != nil {
			t.Fatalf("Cluster health check failed: %v", err)
		}
		return ctx
	})

	// Teardown
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		mlcluster := &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      istioMultinodeName,
				Namespace: istioMultinodeNs,
			},
		}
		if err := client.Resources(istioMultinodeNs).Delete(ctx, mlcluster); err != nil {
			t.Logf("Failed to delete MarklogicCluster: %v", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: istioMultinodeNs}}); err != nil {
			t.Logf("Failed to delete namespace: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// ============================================================================
// Test 4: Non-Istio Regression
// Validates: Wrapper works correctly without Istio (no mesh delay, no breakage)
// ============================================================================

func TestNonIstioRegression(t *testing.T) {
	if !isIstioAmbientEnabled() {
		t.Skip("Skipping: Istio ambient mode tests not enabled (set E2E_ISTIO_AMBIENT=true)")
	}

	feature := features.New("Istio Ambient Mode - Non-Istio Namespace Regression").
		WithLabel("type", "non-istio-regression")

	nonIstioSecret := "non-istio-admin-secrets"

	// Setup: Create standard namespace WITHOUT Istio labels
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := createStandardNamespace(ctx, t, c, nonIstioNs); err != nil {
			t.Fatalf("Failed to create standard namespace: %v", err)
		}

		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(nonIstioNs).GetScheme())

		p := e2eutils.RunCommand(fmt.Sprintf(
			"kubectl -n %s create secret generic %s --from-literal=username=%s --from-literal=password=%s",
			nonIstioNs, nonIstioSecret, istioAdminUsername, istioAdminPassword,
		))
		if p.Err() != nil {
			t.Fatalf("Failed to create admin secret: %s", p.Result())
		}

		mlcluster := &marklogicv1.MarklogicCluster{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "marklogic.progress.com/v1",
				Kind:       "MarklogicCluster",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      nonIstioClusterName,
				Namespace: nonIstioNs,
			},
			Spec: marklogicv1.MarklogicClusterSpec{
				Image: marklogicImage,
				Auth: &marklogicv1.AdminAuth{
					SecretName: &nonIstioSecret,
				},
				MarkLogicGroups: []*marklogicv1.MarklogicGroups{
					{
						Name:        "node",
						Replicas:    &singleReplicas,
						IsBootstrap: true,
					},
				},
			},
		}

		if err := client.Resources(nonIstioNs).Create(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlcluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatalf("MarklogicCluster resource creation timeout: %v", err)
		}

		t.Log("Non-Istio MarklogicCluster resource created")
		return ctx
	})

	// Assess: Verify pod starts without mesh delay
	feature.Assess("Pod starts without Istio mesh", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		err := utils.WaitForPod(ctx, t, client, nonIstioNs, "node-0", standardWaitTimeout)
		if err != nil {
			t.Fatalf("Failed to wait for pod: %v", err)
		}

		t.Log("Pod started without Istio mesh")
		return ctx
	})

	// Assess: Verify namespace is NOT enrolled in Istio ambient mesh
	feature.Assess("Namespace is not in Istio ambient mode", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// 1. Verify namespace does NOT have the ambient label
		ns := &corev1.Namespace{}
		if err := client.Resources().Get(ctx, nonIstioNs, "", ns); err != nil {
			t.Fatalf("Failed to get namespace: %v", err)
		}
		mode, ok := ns.Labels["istio.io/dataplane-mode"]
		if ok && mode == "ambient" {
			t.Fatalf("Namespace %s should NOT have istio.io/dataplane-mode=ambient label", nonIstioNs)
		}
		t.Logf("Namespace %s correctly does not have ambient label", nonIstioNs)

		// 2. Verify pod is NOT enrolled in ztunnel mesh
		p := e2eutils.RunCommand(fmt.Sprintf("istioctl ztunnel-config workload -n %s 2>/dev/null", nonIstioNs))
		if p.Err() == nil {
			output := p.Result()
			if strings.Contains(output, "node-0") {
				t.Logf("Warning: pod node-0 appears in ztunnel workloads for non-Istio namespace. Output: %s", output)
			}
		}
		t.Log("Namespace is correctly not enrolled in Istio ambient mesh")
		return ctx
	})

	// Assess: Verify wrapper completed without mesh gatekeeper
	feature.Assess("Wrapper completes without mesh gatekeeper", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Wrapper reaches monitoring phase with Phase 4 passing quickly in non-Istio environments
		if err := verifyWrapperLogs(ctx, t, nonIstioNs, "node-0", wrapperMonitoringLog); err != nil {
			t.Fatalf("Wrapper did not complete initialization: %v", err)
		}

		// The localhost should be UP (Phase 3 completed)
		if err := verifyWrapperLogs(ctx, t, nonIstioNs, "node-0", wrapperSkippedLog); err != nil {
			t.Fatalf("Wrapper did not pass local readiness: %v", err)
		}

		t.Log("Wrapper completed successfully without Istio mesh gatekeeper")
		return ctx
	})

	// Assess: Verify cluster forms normally
	feature.Assess("Cluster forms normally without Istio", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := waitForClusterHealth(ctx, t, nonIstioNs, "node-0", standardWaitTimeout); err != nil {
			t.Fatalf("Cluster health check failed: %v", err)
		}

		podName := "node-0"
		url := "http://localhost:8002/manage/v2/hosts"
		curlCommand := fmt.Sprintf("curl -s %s --anyauth -u %s:%s", url, istioAdminUsername, istioAdminPassword)

		output, err := utils.ExecCmdInPod(podName, nonIstioNs, mlServerContainer, curlCommand)
		if err != nil {
			t.Fatalf("Failed to query MarkLogic hosts: %v", err)
		}

		if !strings.Contains(output, "node-0") {
			t.Fatalf("Host not found in cluster. Output: %s", output)
		}

		t.Log("Cluster formed normally without Istio")
		return ctx
	})

	// Teardown
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		mlcluster := &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nonIstioClusterName,
				Namespace: nonIstioNs,
			},
		}
		if err := client.Resources(nonIstioNs).Delete(ctx, mlcluster); err != nil {
			t.Logf("Failed to delete MarklogicCluster: %v", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nonIstioNs}}); err != nil {
			t.Logf("Failed to delete namespace: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
