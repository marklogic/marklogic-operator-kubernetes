// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	e2eutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

const (
	logNS        = "ml-ns-log" // must be in watchedNamespaces
	logGroupName = "lognode"
)

// cleanupLogNS deletes all MarklogicCluster CRs in logNS and waits for the
// operator to remove their StatefulSets. The namespace itself (and the
// Helm-installed Role/RoleBinding) is intentionally preserved so the operator
// retains RBAC for subsequent tests. Deleting and recreating the namespace
// would destroy the Helm-created RBAC, leaving the operator forbidden from
// creating resources in the fresh namespace.
func cleanupLogNS(ctx context.Context, t *testing.T, _ *envconf.Config) {
	t.Helper()
	// Delete all MarklogicCluster CRs in the namespace.
	if p := e2eutils.RunCommand(fmt.Sprintf(
		"kubectl delete marklogicclusters --all -n %s --ignore-not-found=true", logNS,
	)); p.Err() != nil {
		t.Logf("Warning: could not delete MarklogicCluster CRs in %s: %s", logNS, p.Result())
	}
	// Wait for the operator to cascade-delete all StatefulSets.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		p := e2eutils.RunCommand(fmt.Sprintf(
			"kubectl get statefulsets -n %s -o name 2>/dev/null", logNS,
		))
		if strings.TrimSpace(p.Result()) == "" {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("Warning: StatefulSets in %s may not be fully cleaned up before next test", logNS)
}

// TestLogCollectionDisabled verifies that fluent-bit is NOT created when
// LogCollection.Enabled is false.
func TestLogCollectionDisabled(t *testing.T) {
	trackTest(t)
	feature := features.New("Log Collection Disabled").WithLabel("type", "log-collection-disabled")

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "ml-no-logs", Namespace: logNS},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth:  &marklogicv1.AdminAuth{AdminUsername: &adminUsername, AdminPassword: &adminPassword},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: logGroupName, Replicas: &replicas, IsBootstrap: true},
			},
			LogCollection: &marklogicv1.LogCollection{Enabled: false},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cleanupLogNS(ctx, t, c)
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(logNS).GetScheme())
		if err := client.Resources(logNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			logDiagnostics(t, logNS)
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("Pod created without fluent-bit container", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := utils.WaitForPod(ctx, t, client, logNS, "lognode-0", 120*time.Second); err != nil {
			logDiagnostics(t, logNS)
			t.Fatalf("Failed to wait for pod: %v", err)
		}
		var pod corev1.Pod
		if err := client.Resources().Get(ctx, "lognode-0", logNS, &pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}
		if len(pod.Spec.Containers) != 1 {
			t.Fatalf("Expected 1 container when log collection disabled, found %d", len(pod.Spec.Containers))
		}
		if pod.Spec.Containers[0].Name != "marklogic-server" {
			t.Fatalf("Expected only marklogic-server container, got %s", pod.Spec.Containers[0].Name)
		}
		t.Log("Verified: fluent-bit container is NOT present when LogCollection is disabled")
		return ctx
	})

	feature.Assess("Fluent-bit ConfigMap not created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var cm corev1.ConfigMap
		if err := c.Client().Resources().Get(ctx, "fluent-bit", logNS, &cm); err == nil {
			t.Fatal("fluent-bit ConfigMap should not exist when LogCollection is disabled")
		}
		t.Log("Verified: fluent-bit ConfigMap is NOT created when LogCollection is disabled")
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := c.Client().Resources(logNS).Delete(ctx, cr); err != nil {
			t.Logf("Warning: failed to delete MarklogicCluster: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestLogCollectionPartialLogs verifies that only the requested log files are configured
// in the fluent-bit ConfigMap.
func TestLogCollectionPartialLogs(t *testing.T) {
	trackTest(t)
	feature := features.New("Log Collection Partial Logs").WithLabel("type", "log-collection-partial")

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "ml-partial-logs", Namespace: logNS},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth:  &marklogicv1.AdminAuth{AdminUsername: &adminUsername, AdminPassword: &adminPassword},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: logGroupName, Replicas: &replicas, IsBootstrap: true},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled: true,
				Image:   "fluent/fluent-bit:4.1.1",
				Files: marklogicv1.LogFilesConfig{
					ErrorLogs:   true,
					AccessLogs:  false,
					RequestLogs: false,
					CrashLogs:   false,
					AuditLogs:   false,
				},
				Outputs: "[OUTPUT]\n\tname stdout\n\tmatch *\n\tformat json_lines",
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cleanupLogNS(ctx, t, c)
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(logNS).GetScheme())
		if err := client.Resources(logNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			logDiagnostics(t, logNS)
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("Pod created with fluent-bit container", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := utils.WaitForPod(ctx, t, client, logNS, "lognode-0", 120*time.Second); err != nil {
			logDiagnostics(t, logNS)
			t.Fatalf("Failed to wait for pod: %v", err)
		}
		var pod corev1.Pod
		if err := client.Resources().Get(ctx, "lognode-0", logNS, &pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}
		if len(pod.Spec.Containers) != 2 {
			t.Fatalf("Expected 2 containers (marklogic-server + fluent-bit), found %d", len(pod.Spec.Containers))
		}
		t.Log("Verified: pod has fluent-bit container for partial log collection")
		return ctx
	})

	feature.Assess("Fluent-bit ConfigMap has only error logs", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var cm corev1.ConfigMap
		if err := c.Client().Resources().Get(ctx, "fluent-bit", logNS, &cm); err != nil {
			t.Fatalf("Failed to get fluent-bit ConfigMap: %v", err)
		}
		cfg := cm.Data["fluent-bit.yaml"]
		if !strings.Contains(cfg, "ErrorLog.txt") {
			t.Fatal("ErrorLog.txt should be present in configuration")
		}
		for _, unexpected := range []string{"AccessLog.txt", "RequestLog.txt", "CrashLog.txt", "AuditLog.txt"} {
			if strings.Contains(cfg, unexpected) {
				t.Fatalf("%s should not be present when disabled", unexpected)
			}
		}
		t.Log("Verified: only error logs are configured in fluent-bit")
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := c.Client().Resources(logNS).Delete(ctx, cr); err != nil {
			t.Logf("Warning: failed to delete MarklogicCluster: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestLogCollectionCustomResources verifies that custom resource requests/limits for the
// fluent-bit sidecar are applied correctly.
func TestLogCollectionCustomResources(t *testing.T) {
	trackTest(t)
	feature := features.New("Log Collection Custom Resources").WithLabel("type", "log-collection-resources")

	customRes := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("150m"),
			corev1.ResourceMemory: resource.MustParse("300Mi"),
		},
	}

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "ml-custom-resources", Namespace: logNS},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth:  &marklogicv1.AdminAuth{AdminUsername: &adminUsername, AdminPassword: &adminPassword},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: logGroupName, Replicas: &replicas, IsBootstrap: true},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled:   true,
				Image:     "fluent/fluent-bit:4.1.1",
				Resources: customRes,
				Files:     marklogicv1.LogFilesConfig{ErrorLogs: true},
				Outputs:   "[OUTPUT]\n\tname stdout\n\tmatch *",
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cleanupLogNS(ctx, t, c)
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(logNS).GetScheme())
		if err := client.Resources(logNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			logDiagnostics(t, logNS)
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("Pod created with custom resources", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), logNS, "lognode-0", 120*time.Second); err != nil {
			logDiagnostics(t, logNS)
			t.Fatalf("Failed to wait for pod: %v", err)
		}
		return ctx
	})

	feature.Assess("Custom resources are applied to fluent-bit container", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var pod corev1.Pod
		if err := c.Client().Resources().Get(ctx, "lognode-0", logNS, &pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}
		var fb *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "fluent-bit" {
				fb = &pod.Spec.Containers[i]
				break
			}
		}
		if fb == nil {
			t.Fatal("fluent-bit container not found in pod")
		}
		checkResource := func(name string, got, want resource.Quantity) {
			if got.Cmp(want) != 0 {
				t.Fatalf("fluent-bit %s: expected %v, got %v", name, want, got)
			}
		}
		checkResource("CPU request", fb.Resources.Requests[corev1.ResourceCPU], resource.MustParse("50m"))
		checkResource("Memory request", fb.Resources.Requests[corev1.ResourceMemory], resource.MustParse("100Mi"))
		checkResource("CPU limit", fb.Resources.Limits[corev1.ResourceCPU], resource.MustParse("150m"))
		checkResource("Memory limit", fb.Resources.Limits[corev1.ResourceMemory], resource.MustParse("300Mi"))
		t.Log("Verified: custom resources are correctly applied to fluent-bit container")
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := c.Client().Resources(logNS).Delete(ctx, cr); err != nil {
			t.Logf("Warning: failed to delete MarklogicCluster: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestLogCollectionCustomFilters verifies that custom fluent-bit filter configuration
// is correctly written to the fluent-bit ConfigMap.
func TestLogCollectionCustomFilters(t *testing.T) {
	trackTest(t)
	feature := features.New("Log Collection Custom Filters").WithLabel("type", "log-collection-filters")

	customFilters := `- name: grep
  match: "*"
  regex: log ERROR
- name: modify
  match: "*"
  add:
    - custom_field custom_value`

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "ml-custom-filters", Namespace: logNS},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth:  &marklogicv1.AdminAuth{AdminUsername: &adminUsername, AdminPassword: &adminPassword},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{Name: logGroupName, Replicas: &replicas, IsBootstrap: true},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled: true,
				Image:   "fluent/fluent-bit:4.1.1",
				Files:   marklogicv1.LogFilesConfig{ErrorLogs: true},
				Filters: customFilters,
				Outputs: "[OUTPUT]\n\tname stdout\n\tmatch *",
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		cleanupLogNS(ctx, t, c)
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(logNS).GetScheme())
		if err := client.Resources(logNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			logDiagnostics(t, logNS)
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("Pod created with custom filters", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), logNS, "lognode-0", 120*time.Second); err != nil {
			logDiagnostics(t, logNS)
			t.Fatalf("Failed to wait for pod: %v", err)
		}
		return ctx
	})

	feature.Assess("Custom filters are in fluent-bit ConfigMap", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var cm corev1.ConfigMap
		if err := c.Client().Resources().Get(ctx, "fluent-bit", logNS, &cm); err != nil {
			t.Fatalf("Failed to get fluent-bit ConfigMap: %v", err)
		}
		cfg := cm.Data["fluent-bit.yaml"]
		for _, want := range []string{"name: grep", "regex: log ERROR", "name: modify", "custom_field custom_value"} {
			if !strings.Contains(cfg, want) {
				t.Fatalf("Expected %q in fluent-bit config, not found", want)
			}
		}
		t.Log("Verified: custom filters are correctly configured in fluent-bit")
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := c.Client().Resources(logNS).Delete(ctx, cr); err != nil {
			t.Logf("Warning: failed to delete MarklogicCluster: %v", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
