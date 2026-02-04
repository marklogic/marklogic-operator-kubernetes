package e2e
// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	istioNamespace   = "istio-test"
	istioClusterName = "istio-ml-cluster"
)

func TestIstioAmbientMode(t *testing.T) {
	var (
		adminUsername = "admin"
		adminPassword = "admin"
		groupName     = "istio-group"
		replicas      = int32(2)
	)

	mlcluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      istioClusterName,
			Namespace: istioNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        groupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
		},
	}

	feature := features.New("ISTIO Ambient Mode Test").WithLabel("type", "istio-ambient")

	// Setup: Create namespace with ISTIO ambient label
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Creating namespace with ISTIO ambient mode enabled")
		client := c.Client()

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: istioNamespace,
				Labels: map[string]string{
					"istio.io/dataplane-mode": "ambient",
				},
			},
		}
		if err := client.Resources().Create(ctx, ns); err != nil {
			t.Fatalf("Failed to create namespace: %s", err)
		}

		// Verify ISTIO components are running
		t.Log("Verifying ISTIO components are ready")
		if err := utils.WaitForPod(ctx, t, client, "istio-system", "istiod", 120*time.Second); err != nil {
			t.Fatalf("istiod not ready: %v", err)
		}

		return ctx
	})

	// Setup: Create MarklogicCluster with ISTIO annotations
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Creating MarklogicCluster in ISTIO ambient mesh")
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(istioNamespace).GetScheme())

		if err := client.Resources(istioNamespace).Create(ctx, mlcluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlcluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Assess: Verify ztunnel pods are running in istio-system
	feature.Assess("ISTIO ztunnel pods are running", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		podList := &corev1.PodList{}
		if err := client.Resources().List(ctx, podList, func(lo *metav1.ListOptions) {
			lo.LabelSelector = "app=ztunnel"
			lo.FieldSelector = "metadata.namespace=istio-system"
		}); err != nil {
			t.Fatal(err)
		}

		if len(podList.Items) == 0 {
			t.Fatal("No ztunnel pods found in istio-system namespace")
		}

		for _, pod := range podList.Items {
			if pod.Status.Phase != corev1.PodRunning {
				t.Fatalf("ztunnel pod %s is not running: %s", pod.Name, pod.Status.Phase)
			}
			t.Logf("ztunnel pod %s is running", pod.Name)
		}

		return ctx
	})

	// Assess: Verify pods created and running
	feature.Assess("MarklogicCluster pods are running with ISTIO", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		for i := 0; i < int(replicas); i++ {
			podName := fmt.Sprintf("node-%d", i)
			t.Logf("Waiting for pod %s to be ready...", podName)
			
			err := utils.WaitForPod(ctx, t, client, istioNamespace, podName, 300*time.Second)
			if err != nil {
				t.Fatalf("Failed to wait for pod %s: %v", podName, err)
			}

			t.Logf("Pod %s is ready", podName)
		}

		return ctx
	})

	// Assess: Verify wrapper script phases in pod logs
	feature.Assess("Wrapper script completed all ISTIO phases", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "node-0"
		containerName := "marklogic"

		t.Logf("Checking wrapper script logs in pod %s", podName)

		// Wait a bit for logs to accumulate
		time.Sleep(10 * time.Second)

		cmd := "cat /var/opt/MarkLogic/Logs/wrapper.log || tail -100 /proc/1/fd/1"
		output, err := utils.ExecCmdInPod(podName, istioNamespace, containerName, cmd)
		if err != nil {
			t.Logf("Warning: Could not read wrapper log directly, checking pod logs")
			// Fallback to kubectl logs
			output, err = utils.GetPodLogs(istioNamespace, podName, containerName)
			if err != nil {
				t.Fatalf("Failed to get pod logs: %v", err)
			}
		}

		t.Logf("Pod logs (first 2000 chars):\n%s", output[:min(len(output), 2000)])

		// Verify key phases completed
		expectedPhases := []string{
			"Phase 1: Starting vendor script in background",
			"Phase 2: Capturing MarkLogic PID",
			"Phase 3: Waiting for localhost:8001",
			"Phase 4: ISTIO ambient network gatekeeper",
			"Phase 5: Cluster initialization via cluster-config.sh",
			"Phase 6: Signal readiness",
			"Phase 7: Monitoring processes",
		}

		for _, phase := range expectedPhases {
			if !strings.Contains(output, phase) {
				t.Errorf("Expected phase not found in logs: %s", phase)
			} else {
				t.Logf("✓ Found: %s", phase)
			}
		}

		// Verify ISTIO mesh connectivity check passed
		if !strings.Contains(output, "Mesh connectivity check passed") {
			t.Error("ISTIO mesh connectivity check did not pass")
		} else {
			t.Log("✓ ISTIO mesh connectivity check passed")
		}

		return ctx
	})

	// Assess: Verify PID refresh after config
	feature.Assess("PID refresh logic handles restarts correctly", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "node-0"
		containerName := "marklogic"

		cmd := "cat /var/opt/MarkLogic/Logs/wrapper.log 2>/dev/null || tail -200 /proc/1/fd/1"
		output, err := utils.ExecCmdInPod(podName, istioNamespace, containerName, cmd)
		if err != nil {
			output, err = utils.GetPodLogs(istioNamespace, podName, containerName)
			if err != nil {
				t.Fatalf("Failed to get pod logs: %v", err)
			}
		}

		// Check for PID refresh logic
		if strings.Contains(output, "Phase 5.5: PID refresh") {
			t.Log("✓ PID refresh phase found")
			
			// Verify PID was actually refreshed if MarkLogic restarted
			if strings.Contains(output, "MarkLogic PID changed") {
				t.Log("✓ PID change detected and handled correctly")
			} else if strings.Contains(output, "PID unchanged") {
				t.Log("✓ PID remained stable (no restart during config)")
			} else {
				t.Error("PID refresh logic did not report status")
			}
		}

		return ctx
	})

	// Assess: Test pod restart detection
	feature.Assess("Crash detection works correctly", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podName := "node-1"
		
		// Get initial restart count
		var pod corev1.Pod
		if err := client.Resources().Get(ctx, podName, istioNamespace, &pod); err != nil {
			t.Fatalf("Failed to get pod %s: %v", podName, err)
		}
		
		initialRestartCount := int32(0)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "marklogic" {
				initialRestartCount = cs.RestartCount
				break
			}
		}

		t.Logf("Initial restart count for %s: %d", podName, initialRestartCount)

		// Simulate crash by killing MarkLogic process
		t.Log("Simulating MarkLogic crash...")
		killCmd := "kill -9 $(cat /var/run/MarkLogic.pid)"
		_, err := utils.ExecCmdInPod(podName, istioNamespace, "marklogic", killCmd)
		if err != nil {
			t.Logf("Kill command failed (expected if wrapper already detected crash): %v", err)
		}

		// Wait and verify pod restarted
		time.Sleep(30 * time.Second)

		if err := client.Resources().Get(ctx, podName, istioNamespace, &pod); err != nil {
			t.Fatalf("Failed to get pod %s after crash: %v", podName, err)
		}

		newRestartCount := int32(0)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "marklogic" {
				newRestartCount = cs.RestartCount
				break
			}
		}

		if newRestartCount > initialRestartCount {
			t.Logf("✓ Pod restarted correctly after crash (restarts: %d -> %d)", initialRestartCount, newRestartCount)
		} else {
			t.Errorf("Pod did not restart after crash (restarts: %d)", newRestartCount)
		}

		return ctx
	})

	// Assess: Verify MarkLogic cluster is healthy
	feature.Assess("MarkLogic cluster is operational", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		podName := "node-0"
		containerName := "marklogic"

		// Check if MarkLogic is responding
		cmd := "curl -s -u admin:admin http://localhost:8001/admin/v1/timestamp"
		output, err := utils.ExecCmdInPod(podName, istioNamespace, containerName, cmd)
		if err != nil {
			t.Fatalf("Failed to query MarkLogic: %v", err)
		}

		if !strings.Contains(output, "timestamp") {
			t.Errorf("Unexpected response from MarkLogic: %s", output)
		} else {
			t.Log("✓ MarkLogic is responding to queries")
		}

		return ctx
	})

	// Teardown: Clean up resources
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Cleaning up ISTIO test resources")
		client := c.Client()

		if err := client.Resources(istioNamespace).Delete(ctx, mlcluster); err != nil {
			t.Logf("Failed to delete MarklogicCluster: %s", err)
		}

		// Wait for pods to terminate
		time.Sleep(30 * time.Second)

		if err := client.Resources().Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: istioNamespace},
		}); err != nil {
			t.Logf("Failed to delete namespace: %s", err)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
