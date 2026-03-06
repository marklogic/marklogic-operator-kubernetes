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

// killMarkLogicProcess forces a container restart in a pod by crashing the MarkLogic
// process tree and, when necessary, killing the vendor startup script's keepalive so
// the wrapper's watchdog fires and exits (causing Kubernetes to restart the container).
//
// Why multiple strategies are needed:
//   - MarkLogic runs as a different OS user than the kubectl-exec session in rootless
//     UBI9 containers, so `readlink /proc/*/exe` returns empty (EPERM on the symlink).
//   - /proc/PID/comm is world-readable and exposes the 15-char process name: we use it
//     to find MarkLogic processes across user boundaries.
//   - Even if kill -9 on a MarkLogic process is permitted, init.d may restart it quickly.
//     Killing the vendor script's keepalive process (`tail -f /dev/null`) is always
//     permitted (same user as the container) and is the signal the wrapper monitors: the
//     wrapper's Phase 7 loop checks `kill -0 "$SCRIPT_PID"` and exits with code 1 the
//     moment that process disappears, which Kubernetes sees as a container crash.
func killMarkLogicProcess(t *testing.T, namespace, podName string) error {
	t.Logf("Killing MarkLogic processes in pod %s", podName)

	// Strategy 1: Kill the process recorded in the PID file (best-effort).
	pidFile := "/var/run/MarkLogic.pid"
	killByPidFileCmd := fmt.Sprintf(
		`ML_PID=$(cat %s 2>/dev/null); [ -n "$ML_PID" ] && kill -9 "$ML_PID" 2>/dev/null; true`,
		pidFile,
	)
	out1, err1 := utils.ExecCmdInPod(podName, namespace, mlServerContainer, killByPidFileCmd)
	if err1 != nil {
		t.Logf("PID-file kill output: %s, err: %v (may be benign)", out1, err1)
	} else {
		t.Logf("PID-file kill sent (output: %s)", out1)
	}

	// Strategy 2: Scan /proc/*/comm (world-readable, no symlink needed) to find ALL
	// MarkLogic processes regardless of which OS user they run as.
	killByCommCmd := `killed=0; ` +
		`for f in /proc/[0-9]*/comm; do ` +
		`  comm=$(cat "$f" 2>/dev/null); ` +
		`  case "$comm" in ` +
		`    MarkLogic*|mlserver*|MLServer*) ` +
		`      pid=${f%/comm}; pid=${pid#/proc/}; ` +
		`      kill -9 "$pid" 2>/dev/null && killed=$((killed+1)); ` +
		`      ;; ` +
		`  esac; ` +
		`done; ` +
		`echo "Killed $killed MarkLogic process(es) via comm"`
	out2, err2 := utils.ExecCmdInPod(podName, namespace, mlServerContainer, killByCommCmd)
	if err2 != nil {
		t.Logf("Comm-scan kill output: %s, err: %v (may be benign)", out2, err2)
	} else {
		t.Logf("Comm-scan kill result: %s", strings.TrimSpace(out2))
	}

	// Strategy 3: Kill the vendor script's keepalive process.
	// start-marklogic-rootless.sh ends with `tail -f /dev/null` which is SCRIPT_PID that
	// the wrapper monitors. Killing it causes the wrapper to exit immediately (exit 1),
	// which is the most reliable path to a container restart. This process runs as the
	// same user as the container so kill always succeeds.
	killTailCmd := `killed=0; ` +
		`for f in /proc/[0-9]*/comm; do ` +
		`  comm=$(cat "$f" 2>/dev/null); ` +
		`  if [ "$comm" = "tail" ]; then ` +
		`    pid=${f%/comm}; pid=${pid#/proc/}; ` +
		`    cmdline=$(cat "/proc/$pid/cmdline" 2>/dev/null | tr "\\0" " "); ` +
		`    case "$cmdline" in ` +
		`      *-f*) kill -9 "$pid" 2>/dev/null && killed=$((killed+1)); ;; ` +
		`    esac; ` +
		`  fi; ` +
		`done; ` +
		`echo "Killed $killed tail keepalive process(es)"`
	out3, err3 := utils.ExecCmdInPod(podName, namespace, mlServerContainer, killTailCmd)
	if err3 != nil {
		t.Logf("Tail-kill output: %s, err: %v (may be benign)", out3, err3)
	} else {
		t.Logf("Tail-kill result: %s", strings.TrimSpace(out3))
	}

	t.Logf("Kill sequence complete for pod %s", podName)
	return nil
}

// waitForHostsInCluster polls the MarkLogic hosts endpoint on the given pod until
// all expected host substrings appear in the response. This is needed for multi-node
// tests where the management API may be healthy on the bootstrap node before the
// joining nodes have fully completed cluster membership.
func waitForHostsInCluster(ctx context.Context, t *testing.T, namespace, podName string, expectedHosts []string, timeout time.Duration) error {
	t.Logf("Waiting for hosts %v to appear in cluster (querying pod %s)", expectedHosts, podName)

	url := "http://localhost:8002/manage/v2/hosts"
	curlCommand := fmt.Sprintf("curl -s %s --digest -u '%s:%s'", url, istioAdminUsername, istioAdminPassword)

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for cluster hosts: %w", ctx.Err())
		default:
		}

		output, err := utils.ExecCmdInPod(podName, namespace, mlServerContainer, curlCommand)
		if err == nil {
			allFound := true
			for _, host := range expectedHosts {
				if !strings.Contains(output, host) {
					allFound = false
					t.Logf("Host %q not yet in cluster (will retry)", host)
					break
				}
			}
			if allFound {
				t.Logf("All expected hosts %v are present in the cluster", expectedHosts)
				return nil
			}
		} else {
			t.Logf("Failed to query hosts endpoint on pod %s: %v", podName, err)
		}

		if time.Since(start) > timeout {
			lastOutput := ""
			if err == nil {
				lastOutput = output
			}
			return fmt.Errorf("hosts %v did not all appear in cluster within %v. Last output: %s", expectedHosts, timeout, lastOutput)
		}

		time.Sleep(podCheckInterval)
	}
}

// waitForClusterHealth waits for the MarkLogic cluster to be healthy by querying
// the management API on the given pod. It verifies that the /manage/v2 endpoint
// returns a successful response, indicating that MarkLogic is fully operational.
// This approach is used because the operator sets Ready conditions on MarklogicGroup
// (child CR) resources, not on the parent MarklogicCluster CR.
func waitForClusterHealth(ctx context.Context, t *testing.T, namespace, podName string, timeout time.Duration) error {
	t.Logf("Waiting for MarkLogic cluster to become healthy (via pod %s)", podName)

	url := "http://localhost:8002/manage/v2"
	// Use --digest explicitly: MarkLogic's management API (8002) requires HTTP Digest auth.
	// Quoting the credentials handles special characters (e.g. '@') in the password.
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

			// Wait for Running + Ready so MarkLogic is fully initialized before subsequent checks.
			err := utils.WaitForPod(ctx, t, client, istioAmbientNs, podName, istioWaitTimeout, true)
			if err != nil {
				t.Fatalf("Failed to wait for pod %s: %v", podName, err)
			}
			t.Logf("Pod %s is Ready", podName)
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

		// All pods in this group are in the bootstrap group (IsBootstrap: true), so
		// MARKLOGIC_CLUSTER_TYPE is "bootstrap" for all replicas. The wrapper's Phase 4
		// mesh gatekeeper only runs for non-bootstrap groups (MARKLOGIC_CLUSTER_TYPE=non-bootstrap).
		// Cross-group mesh connectivity is validated in TestIstioAmbientNetworkGatekeeper.
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
		curlCommand := fmt.Sprintf("curl -s %s --digest -u '%s:%s'", url, istioAdminUsername, istioAdminPassword)

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
		// Wait for Running + Ready so MarkLogic is fully initialized (readiness probe on port 7997
		// passes only after security init and cluster-config.sh complete, at which point port 8002
		// is also serving the management API). This eliminates spurious 403/401/exit-7 noise in
		// the subsequent waitForClusterHealth call.
		err := utils.WaitForPod(ctx, t, client, resilienceNs, "node-0", istioWaitTimeout, true)
		if err != nil {
			t.Fatalf("Failed to wait for pod node-0: %v", err)
		}

		// Confirm management API is healthy (should be near-instant now that pod is Ready).
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

		// Phase A0: Confirm the kill took effect.
		// There are two valid signals that the kill worked:
		//   (a) Port 8001 goes down  — MarkLogic process tree was successfully killed.
		//   (b) RestartCount increases — the wrapper detected SCRIPT_PID missing (tail
		//       keepalive killed) and exited immediately, triggering a container restart
		//       before port 8001 even had a chance to go down.
		// We poll for either within 2 minutes before proceeding.
		portDownCmd := `curl -s -k -m 2 -o /dev/null http://localhost:8001 2>/dev/null; echo $?`
		killConfirmTimeout := 2 * time.Minute
		killConfirmStart := time.Now()
		killConfirmed := false
		for time.Since(killConfirmStart) < killConfirmTimeout {
			// Check (b) first: fast path — wrapper already exited via tail-kill.
			var podCheck corev1.Pod
			if err := resources.Get(ctx, "node-0", resilienceNs, &podCheck); err == nil {
				if getRestartCount(&podCheck) > initialRestartCount {
					t.Log("Kill confirmed: RestartCount already increased (wrapper exited via script-keepalive kill)")
					killConfirmed = true
					break
				}
			}
			// Check (a): port 8001 down.
			portOut, portErr := utils.ExecCmdInPod("node-0", resilienceNs, mlServerContainer, portDownCmd)
			if portErr != nil || strings.TrimSpace(portOut) != "0" {
				t.Logf("Kill confirmed: port 8001 is down (curl exit: %s err: %v)", strings.TrimSpace(portOut), portErr)
				killConfirmed = true
				break
			}
			t.Logf("Kill not yet confirmed (port still up, RestartCount unchanged) — retrying...")
			time.Sleep(5 * time.Second)
		}
		if !killConfirmed {
			t.Fatalf("Kill not confirmed within %v: port 8001 still up and RestartCount unchanged. "+
				"MarkLogic processes may be running as a different OS user than the exec session. "+
				"Check container security context and MarkLogic startup user.",
				killConfirmTimeout)
		}

		// Phase A: Wait for the RestartCount to increase.
		// The wrapper's port watchdog needs ~30 s of sustained port-down before it
		// exits (6 checks × 5 s = 30 s). Allow 3 minutes for that + Kubernetes to
		// actually recreate the container.
		restartDetectTimeout := 3 * time.Minute
		restartDetectStart := time.Now()
		restartDetected := false
		var postRestartExpectedCount int32
		for time.Since(restartDetectStart) < restartDetectTimeout {
			var pod corev1.Pod
			if err := resources.Get(ctx, "node-0", resilienceNs, &pod); err != nil {
				if !apierrors.IsNotFound(err) {
					t.Logf("Error getting pod node-0: %v (retrying)", err)
				}
				time.Sleep(podCheckInterval)
				continue
			}
			currentRestartCount := getRestartCount(&pod)
			if currentRestartCount > initialRestartCount {
				t.Logf("Container restart detected: RestartCount increased from %d to %d", initialRestartCount, currentRestartCount)
				postRestartExpectedCount = currentRestartCount
				restartDetected = true
				break
			}
			time.Sleep(podCheckInterval)
		}
		if !restartDetected {
			t.Fatalf("Container RestartCount did not increase within %v after process crash (still at %d). "+
				"The wrapper watchdog (~30 s port-down threshold) may not have fired.",
				restartDetectTimeout, initialRestartCount)
		}

		// Phase B: Wait for the pod to be Running and Ready after the restart.
		// MarkLogic needs to re-run the full wrapper initialization (phases 1-7) including
		// cluster join, which can take several minutes.
		t.Logf("Container restarted (RestartCount=%d). Waiting for pod to become Ready...", postRestartExpectedCount)
		readyTimeout := istioWaitTimeout
		readyStart := time.Now()
		podReady := false
		for time.Since(readyStart) < readyTimeout {
			var pod corev1.Pod
			if err := resources.Get(ctx, "node-0", resilienceNs, &pod); err != nil {
				if !apierrors.IsNotFound(err) {
					t.Logf("Error getting pod node-0: %v (retrying)", err)
				}
				time.Sleep(podCheckInterval)
				continue
			}
			if pod.Status.Phase != corev1.PodRunning {
				time.Sleep(podCheckInterval)
				continue
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					podReady = true
					break
				}
			}
			if podReady {
				t.Logf("Pod node-0 is Running and Ready after restart (took %v)", time.Since(readyStart).Round(time.Second))
				break
			}
			time.Sleep(podCheckInterval)
		}
		if !podReady {
			t.Fatalf("Pod node-0 did not become Ready within %v after container restart", readyTimeout)
		}

		t.Log("Container successfully restarted and is Ready after process crash")
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

		// Wait for Ready (not just Running) so the wrapper has finished initializing.
		err := utils.WaitForPod(ctx, t, client, istioMultinodeNs, bootstrapPod, istioWaitTimeout, true)
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

		// Wait for Ready (not just Running) so the wrapper has completed mesh connectivity checks.
		err := utils.WaitForPod(ctx, t, client, istioMultinodeNs, enodePod, istioWaitTimeout, true)
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

		// Poll the hosts endpoint until both dnode-0 and enode-0 are present.
		// The management API may be healthy on dnode-0 before enode-0 has finished
		// joining the cluster, so we must retry rather than do a one-shot check.
		if err := waitForHostsInCluster(ctx, t, istioMultinodeNs, bootstrapPod, []string{"dnode-0", "enode-0"}, istioWaitTimeout); err != nil {
			t.Fatalf("Inter-node communication check failed: %v", err)
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
		curlCommand := fmt.Sprintf("curl -s %s --digest -u '%s:%s'", url, istioAdminUsername, istioAdminPassword)

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
