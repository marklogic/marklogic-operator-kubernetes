// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	logCollectionNamespace = "log-test"
	logGroupName           = "lognode"
)

// TestLogCollectionDisabled tests that fluent-bit is NOT created when LogCollection.Enabled is false
func TestLogCollectionDisabled(t *testing.T) {
	feature := features.New("Log Collection Disabled Test").WithLabel("type", "log-collection-disabled")

	replicas := int32(1)
	adminUser := "admin"
	adminPass := "Admin@8001"

	mlclusterDisabled := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-no-logs",
			Namespace: logCollectionNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUser,
				AdminPassword: &adminPass,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        logGroupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled: false, // Explicitly disabled
			},
		},
	}

	// Setup namespace and cluster
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Delete namespace if it exists and wait for it to be fully removed
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}
		if err := client.Resources().Get(ctx, logCollectionNamespace, "", ns); err == nil {
			// Namespace exists, delete it
			if err := client.Resources().Delete(ctx, ns); err != nil {
				t.Logf("Failed to delete existing namespace: %v", err)
			}
			// Wait for namespace to be fully deleted
			if err := wait.For(
				conditions.New(client.Resources()).ResourceDeleted(ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(2*time.Second),
			); err != nil {
				t.Logf("Warning: namespace deletion timeout, proceeding anyway: %v", err)
			}
		}

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: logCollectionNamespace,
			},
		}
		if err := client.Resources().Create(ctx, namespace); err != nil {
			t.Fatalf("Failed to create namespace: %s", err)
		}

		marklogicv1.AddToScheme(client.Resources(logCollectionNamespace).GetScheme())

		if err := client.Resources(logCollectionNamespace).Create(ctx, mlclusterDisabled); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlclusterDisabled, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Verify pod created
	feature.Assess("Pod created without fluent-bit", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podName := "lognode-0"
		err := utils.WaitForPod(ctx, t, client, logCollectionNamespace, podName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}

		var pod corev1.Pod
		if err := client.Resources().Get(ctx, podName, logCollectionNamespace, &pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}

		// Verify only 1 container exists (marklogic-server only)
		if len(pod.Spec.Containers) != 1 {
			t.Fatalf("Expected 1 container when log collection disabled, found %d", len(pod.Spec.Containers))
		}

		if pod.Spec.Containers[0].Name != "marklogic-server" {
			t.Fatalf("Expected only marklogic-server container, got %s", pod.Spec.Containers[0].Name)
		}

		t.Log("Verified fluent-bit container is NOT created when LogCollection is disabled")
		return ctx
	})

	// Verify ConfigMap is NOT created
	feature.Assess("Fluent-bit ConfigMap not created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		var configMap corev1.ConfigMap
		err := client.Resources().Get(ctx, "fluent-bit", logCollectionNamespace, &configMap)
		if err == nil {
			t.Fatal("Fluent-bit ConfigMap should not exist when LogCollection is disabled")
		}

		t.Log("Verified fluent-bit ConfigMap is NOT created when LogCollection is disabled")
		return ctx
	})

	// Cleanup
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(logCollectionNamespace).Delete(ctx, mlclusterDisabled); err != nil {
			t.Fatalf("Failed to delete MarklogicCluster: %s", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestLogCollectionPartialLogs tests selective log file collection
func TestLogCollectionPartialLogs(t *testing.T) {
	feature := features.New("Log Collection Partial Logs Test").WithLabel("type", "log-collection-partial")

	replicas := int32(1)
	adminUser := "admin"
	adminPass := "Admin@8001"

	mlclusterPartial := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-partial-logs",
			Namespace: logCollectionNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUser,
				AdminPassword: &adminPass,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        logGroupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled: true,
				Image:   "fluent/fluent-bit:4.1.1",
				Files: marklogicv1.LogFilesConfig{
					ErrorLogs:   true,  // Only error logs
					AccessLogs:  false, // Disabled
					RequestLogs: false, // Disabled
					CrashLogs:   false, // Disabled
					AuditLogs:   false, // Disabled
				},
				Outputs: "[OUTPUT]\n\tname stdout\n\tmatch *\n\tformat json_lines",
			},
		},
	}

	// Setup namespace and cluster
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Delete namespace if it exists and wait for it to be fully removed
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}
		if err := client.Resources().Get(ctx, logCollectionNamespace, "", ns); err == nil {
			// Namespace exists, delete it
			if err := client.Resources().Delete(ctx, ns); err != nil {
				t.Logf("Failed to delete existing namespace: %v", err)
			}
			// Wait for namespace to be fully deleted
			if err := wait.For(
				conditions.New(client.Resources()).ResourceDeleted(ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(2*time.Second),
			); err != nil {
				t.Logf("Warning: namespace deletion timeout, proceeding anyway: %v", err)
			}
		}

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: logCollectionNamespace,
			},
		}
		if err := client.Resources().Create(ctx, namespace); err != nil {
			t.Fatalf("Failed to create namespace: %s", err)
		}

		marklogicv1.AddToScheme(client.Resources(logCollectionNamespace).GetScheme())

		if err := client.Resources(logCollectionNamespace).Create(ctx, mlclusterPartial); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlclusterPartial, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Verify pod created with fluent-bit
	feature.Assess("Pod created with fluent-bit for partial logs", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podName := "lognode-0"
		err := utils.WaitForPod(ctx, t, client, logCollectionNamespace, podName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}

		var pod corev1.Pod
		if err := client.Resources().Get(ctx, podName, logCollectionNamespace, &pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}

		// Verify 2 containers exist
		if len(pod.Spec.Containers) != 2 {
			t.Fatalf("Expected 2 containers, found %d", len(pod.Spec.Containers))
		}

		t.Log("Verified pod created with fluent-bit container for partial log collection")
		return ctx
	})

	// Verify ConfigMap contains only error logs
	feature.Assess("Fluent-bit ConfigMap has only error logs", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		var configMap corev1.ConfigMap
		if err := client.Resources().Get(ctx, "fluent-bit", logCollectionNamespace, &configMap); err != nil {
			t.Fatalf("Failed to get fluent-bit ConfigMap: %v", err)
		}

		fluentBitConfig := configMap.Data["fluent-bit.yaml"]

		// Should have ErrorLog
		if !strings.Contains(fluentBitConfig, "ErrorLog.txt") {
			t.Fatal("ErrorLog.txt should be present in configuration")
		}

		// Should NOT have other logs
		if strings.Contains(fluentBitConfig, "AccessLog.txt") {
			t.Fatal("AccessLog.txt should not be present when disabled")
		}

		if strings.Contains(fluentBitConfig, "RequestLog.txt") {
			t.Fatal("RequestLog.txt should not be present when disabled")
		}

		if strings.Contains(fluentBitConfig, "CrashLog.txt") {
			t.Fatal("CrashLog.txt should not be present when disabled")
		}

		if strings.Contains(fluentBitConfig, "AuditLog.txt") {
			t.Fatal("AuditLog.txt should not be present when disabled")
		}

		t.Log("Verified only error logs are configured in fluent-bit")
		return ctx
	})

	// Cleanup
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(logCollectionNamespace).Delete(ctx, mlclusterPartial); err != nil {
			t.Fatalf("Failed to delete MarklogicCluster: %s", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestLogCollectionCustomResources tests custom resource configuration for fluent-bit
func TestLogCollectionCustomResources(t *testing.T) {
	feature := features.New("Log Collection Custom Resources Test").WithLabel("type", "log-collection-resources")

	replicas := int32(1)
	adminUser := "admin"
	adminPass := "Admin@8001"

	customResources := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("150m"),
			corev1.ResourceMemory: resource.MustParse("300Mi"),
		},
	}

	mlclusterCustom := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-custom-resources",
			Namespace: logCollectionNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUser,
				AdminPassword: &adminPass,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        logGroupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled:   true,
				Image:     "fluent/fluent-bit:4.1.1",
				Resources: customResources,
				Files: marklogicv1.LogFilesConfig{
					ErrorLogs: true,
				},
				Outputs: "[OUTPUT]\n\tname stdout\n\tmatch *",
			},
		},
	}

	// Setup namespace and cluster
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Delete namespace if it exists and wait for it to be fully removed
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}
		if err := client.Resources().Get(ctx, logCollectionNamespace, "", ns); err == nil {
			// Namespace exists, delete it
			if err := client.Resources().Delete(ctx, ns); err != nil {
				t.Logf("Failed to delete existing namespace: %v", err)
			}
			// Wait for namespace to be fully deleted
			if err := wait.For(
				conditions.New(client.Resources()).ResourceDeleted(ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(2*time.Second),
			); err != nil {
				t.Logf("Warning: namespace deletion timeout, proceeding anyway: %v", err)
			}
		}

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: logCollectionNamespace,
			},
		}
		if err := client.Resources().Create(ctx, namespace); err != nil {
			t.Fatalf("Failed to create namespace: %s", err)
		}

		marklogicv1.AddToScheme(client.Resources(logCollectionNamespace).GetScheme())

		if err := client.Resources(logCollectionNamespace).Create(ctx, mlclusterCustom); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlclusterCustom, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Verify pod created
	feature.Assess("Pod created with custom resources", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podName := "lognode-0"
		err := utils.WaitForPod(ctx, t, client, logCollectionNamespace, podName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx
	})

	// Verify custom resources are applied
	feature.Assess("Custom resources applied to fluent-bit", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podName := "lognode-0"

		var pod corev1.Pod
		if err := client.Resources().Get(ctx, podName, logCollectionNamespace, &pod); err != nil {
			t.Fatalf("Failed to get pod: %v", err)
		}

		// Find fluent-bit container
		var fluentBitContainer *corev1.Container
		for i, container := range pod.Spec.Containers {
			if container.Name == "fluent-bit" {
				fluentBitContainer = &pod.Spec.Containers[i]
				break
			}
		}

		if fluentBitContainer == nil {
			t.Fatal("Fluent-bit container not found")
		}

		// Verify custom CPU request
		cpuRequest := fluentBitContainer.Resources.Requests[corev1.ResourceCPU]
		expectedCPU := resource.MustParse("50m")
		if cpuRequest.Cmp(expectedCPU) != 0 {
			t.Fatalf("Expected custom CPU request %v, got %v", expectedCPU, cpuRequest)
		}

		// Verify custom memory request
		memRequest := fluentBitContainer.Resources.Requests[corev1.ResourceMemory]
		expectedMem := resource.MustParse("100Mi")
		if memRequest.Cmp(expectedMem) != 0 {
			t.Fatalf("Expected custom memory request %v, got %v", expectedMem, memRequest)
		}

		// Verify custom CPU limit
		cpuLimit := fluentBitContainer.Resources.Limits[corev1.ResourceCPU]
		expectedCPULimit := resource.MustParse("150m")
		if cpuLimit.Cmp(expectedCPULimit) != 0 {
			t.Fatalf("Expected custom CPU limit %v, got %v", expectedCPULimit, cpuLimit)
		}

		// Verify custom memory limit
		memLimit := fluentBitContainer.Resources.Limits[corev1.ResourceMemory]
		expectedMemLimit := resource.MustParse("300Mi")
		if memLimit.Cmp(expectedMemLimit) != 0 {
			t.Fatalf("Expected custom memory limit %v, got %v", expectedMemLimit, memLimit)
		}

		t.Log("Verified custom resources are correctly applied to fluent-bit container")
		return ctx
	})

	// Cleanup
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(logCollectionNamespace).Delete(ctx, mlclusterCustom); err != nil {
			t.Fatalf("Failed to delete MarklogicCluster: %s", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestLogCollectionCustomFilters tests custom filters configuration
func TestLogCollectionCustomFilters(t *testing.T) {
	feature := features.New("Log Collection Custom Filters Test").WithLabel("type", "log-collection-filters")

	replicas := int32(1)
	adminUser := "admin"
	adminPass := "Admin@8001"

	customFilters := `- name: grep
  match: "*"
  regex: log ERROR
- name: modify
  match: "*"
  add:
    - custom_field custom_value`

	mlclusterFilters := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-custom-filters",
			Namespace: logCollectionNamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUser,
				AdminPassword: &adminPass,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        logGroupName,
					Replicas:    &replicas,
					IsBootstrap: true,
				},
			},
			LogCollection: &marklogicv1.LogCollection{
				Enabled: true,
				Image:   "fluent/fluent-bit:4.1.1",
				Files: marklogicv1.LogFilesConfig{
					ErrorLogs: true,
				},
				Filters: customFilters,
				Outputs: "[OUTPUT]\n\tname stdout\n\tmatch *",
			},
		},
	}

	// Setup namespace and cluster
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Delete namespace if it exists and wait for it to be fully removed
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}
		if err := client.Resources().Get(ctx, logCollectionNamespace, "", ns); err == nil {
			// Namespace exists, delete it
			if err := client.Resources().Delete(ctx, ns); err != nil {
				t.Logf("Failed to delete existing namespace: %v", err)
			}
			// Wait for namespace to be fully deleted
			if err := wait.For(
				conditions.New(client.Resources()).ResourceDeleted(ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(2*time.Second),
			); err != nil {
				t.Logf("Warning: namespace deletion timeout, proceeding anyway: %v", err)
			}
		}

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: logCollectionNamespace,
			},
		}
		if err := client.Resources().Create(ctx, namespace); err != nil {
			t.Fatalf("Failed to create namespace: %s", err)
		}

		marklogicv1.AddToScheme(client.Resources(logCollectionNamespace).GetScheme())

		if err := client.Resources(logCollectionNamespace).Create(ctx, mlclusterFilters); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %s", err)
		}

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(mlclusterFilters, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	// Verify pod created
	feature.Assess("Pod created with custom filters", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podName := "lognode-0"
		err := utils.WaitForPod(ctx, t, client, logCollectionNamespace, podName, 120*time.Second)
		if err != nil {
			t.Fatalf("Failed to wait for pod creation: %v", err)
		}
		return ctx
	})

	// Verify custom filters are in ConfigMap
	feature.Assess("Custom filters in fluent-bit ConfigMap", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		var configMap corev1.ConfigMap
		if err := client.Resources().Get(ctx, "fluent-bit", logCollectionNamespace, &configMap); err != nil {
			t.Fatalf("Failed to get fluent-bit ConfigMap: %v", err)
		}

		fluentBitConfig := configMap.Data["fluent-bit.yaml"]

		// Verify custom grep filter
		if !strings.Contains(fluentBitConfig, "name: grep") {
			t.Fatal("Custom grep filter not found in configuration")
		}

		if !strings.Contains(fluentBitConfig, "regex: log ERROR") {
			t.Fatal("Custom grep regex not found in configuration")
		}

		// Verify custom modify filter
		if !strings.Contains(fluentBitConfig, "name: modify") {
			t.Fatal("Custom modify filter not found in configuration")
		}

		if !strings.Contains(fluentBitConfig, "custom_field custom_value") {
			t.Fatal("Custom field not found in modify filter")
		}

		t.Log("Verified custom filters are correctly configured in fluent-bit")
		return ctx
	})

	// Cleanup
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(logCollectionNamespace).Delete(ctx, mlclusterFilters); err != nil {
			t.Fatalf("Failed to delete MarklogicCluster: %s", err)
		}
		if err := client.Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logCollectionNamespace}}); err != nil {
			t.Fatalf("Failed to delete namespace: %s", err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
