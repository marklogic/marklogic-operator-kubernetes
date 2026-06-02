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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	e2eutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

func cleanupHAProxyNamespaceArtifacts(ns string) {
	// Remove stale resources from interrupted runs so each test starts from a clean workload state.
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete marklogiccluster --all --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete statefulset --all --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete deployment marklogic-haproxy --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete service marklogic-haproxy ml ml-cluster --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete pod --all --ignore-not-found=true --wait=false", ns))
	e2eutils.RunCommand(fmt.Sprintf("kubectl --request-timeout=20s -n %s delete pvc --all --ignore-not-found=true --wait=false", ns))

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pods := strings.TrimSpace(e2eutils.RunCommand(fmt.Sprintf("kubectl get pods -n %s -o name --ignore-not-found=true 2>/dev/null", ns)).Result())
		sts := strings.TrimSpace(e2eutils.RunCommand(fmt.Sprintf("kubectl get statefulsets -n %s -o name --ignore-not-found=true 2>/dev/null", ns)).Result())
		if pods == "" && sts == "" {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// TestHAProxyPathBasedEnabled verifies that path-based HAProxy routing works correctly
// in a watched namespace with namespace-scoped RBAC.
func TestHAProxyPathBasedEnabled(t *testing.T) {
	trackTest(t)
	feature := features.New("HAProxy with Path-Based Routing Enabled").WithLabel("type", "haproxy-pathbased-enabled")
	haProxyPathNS := "ml-ns-haproxy-path" // must be in watchedNamespaces
	releaseName := "ml"
	haReplicas := int32(1)
	trueVal := true

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: haProxyPathNS,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        releaseName,
					Replicas:    &haReplicas,
					IsBootstrap: true,
				},
			},
			HAProxy: &marklogicv1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &trueVal,
				FrontendPort:     8080,
				AppServers: []marklogicv1.AppServers{
					{Name: "app-service", Port: 8000, Path: "/console"},
					{Name: "admin", Port: 8001, Path: "/adminUI"},
					{Name: "manage", Port: 8002, Path: "/manage"},
				},
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, haProxyPathNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", haProxyPathNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", haProxyPathNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", haProxyPathNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}
		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: haProxyPathNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", haProxyPathNS, err)
		}
		cleanupHAProxyNamespaceArtifacts(haProxyPathNS)
		marklogicv1.AddToScheme(client.Resources(haProxyPathNS).GetScheme())
		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete marklogiccluster %s --ignore-not-found=true", haProxyPathNS, cr.Name))
		if err := client.Resources(haProxyPathNS).Create(ctx, cr); err != nil {
			if apierrors.IsAlreadyExists(err) {
				e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete marklogiccluster %s --ignore-not-found=true", haProxyPathNS, cr.Name))
				time.Sleep(3 * time.Second)
				if retryErr := client.Resources(haProxyPathNS).Create(ctx, cr); retryErr != nil {
					t.Fatalf("Failed to create MarklogicCluster after replacing stale resource: %v", retryErr)
				}
			} else {
				t.Fatalf("Failed to create MarklogicCluster: %v", err)
			}
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("pod ml-0 is ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), haProxyPathNS, "ml-0", 300*time.Second, true); err != nil {
			logDiagnostics(t, haProxyPathNS)
			t.Fatalf("ml-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("marklogic-haproxy.%s.svc.cluster.local", haProxyPathNS)
		url := "http://" + fqdn + ":8080/adminUI"
		cmd := fmt.Sprintf("curl --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		time.Sleep(5 * time.Second)
		if _, err := utils.ExecCmdInPod("ml-0", haProxyPathNS, mlContainerName, cmd); err != nil {
			t.Fatalf("HAProxy path-based routing check failed: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing sets authentication to BASIC", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("ml-0.ml.%s.svc.cluster.local", haProxyPathNS)
		url := "http://" + fqdn + ":8001"
		cmd := fmt.Sprintf("curl -I %s", url)
		res, err := utils.ExecCmdInPod("ml-0", haProxyPathNS, mlContainerName, cmd)
		if err != nil {
			t.Fatalf("Failed to check authentication method: %v", err)
		}
		if !strings.Contains(res, "WWW-Authenticate: Basic") {
			t.Fatalf("Expected Basic auth header, got: %s", res)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Delete the actual MarklogicCluster created in Setup. The CR's metadata
		// name must match what was created (see cr.ObjectMeta above) — using a
		// different name results in a silent NotFound and the owned pods never
		// get garbage-collected, causing the wait below to time out.
		mlc := &marklogicv1.MarklogicCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cr.Name,
				Namespace: haProxyPathNS,
			},
		}
		if err := c.Client().Resources().Delete(ctx, mlc); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("failed to delete MarklogicCluster %s/%s: %v", haProxyPathNS, mlc.Name, err)
		}

		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			current := &marklogicv1.MarklogicCluster{}
			err := c.Client().Resources().Get(ctx, mlc.Name, haProxyPathNS, current)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				t.Fatalf("failed waiting for MarklogicCluster %s/%s deletion: %v", haProxyPathNS, mlc.Name, err)
			}
			time.Sleep(5 * time.Second)
		}

		for time.Now().Before(deadline) {
			pod := &corev1.Pod{}
			err := c.Client().Resources().Get(ctx, "ml-0", haProxyPathNS, pod)
			if apierrors.IsNotFound(err) {
				return ctx
			}
			if err != nil {
				t.Fatalf("failed waiting for owned pod ml-0 deletion in namespace %s: %v", haProxyPathNS, err)
			}
			time.Sleep(5 * time.Second)
		}

		t.Fatalf("timed out waiting for MarklogicCluster owned resources to be removed in namespace %s", haProxyPathNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestHAProxyPathBasedDisabled verifies that HAProxy with path-based routing disabled
// works correctly in a watched namespace.
func TestHAProxyPathBasedDisabled(t *testing.T) {
	trackTest(t)
	feature := features.New("HAProxy with Path-Based Routing Disabled").WithLabel("type", "haproxy-pathbased-disabled")
	haProxyNS := "ml-ns-haproxy" // must be in watchedNamespaces
	releaseName := "ml"
	haReplicas := int32(1)
	falseVal := false

	cr := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marklogicclusters",
			Namespace: haProxyNS,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        releaseName,
					Replicas:    &haReplicas,
					IsBootstrap: true,
				},
			},
			HAProxy: &marklogicv1.HAProxy{
				Enabled:          true,
				PathBasedRouting: &falseVal,
				FrontendPort:     8090,
				AppServers: []marklogicv1.AppServers{
					{Name: "app-service", Port: 8000, Path: "/console"},
					{Name: "admin", Port: 8001, Path: "/adminUI"},
					{Name: "manage", Port: 8002, Path: "/manage"},
				},
			},
		},
	}

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, haProxyNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", haProxyNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", haProxyNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", haProxyNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}
		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: haProxyNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", haProxyNS, err)
		}
		cleanupHAProxyNamespaceArtifacts(haProxyNS)
		marklogicv1.AddToScheme(client.Resources(haProxyNS).GetScheme())
		e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete marklogiccluster %s --ignore-not-found=true", haProxyNS, cr.Name))
		if err := client.Resources(haProxyNS).Create(ctx, cr); err != nil {
			if apierrors.IsAlreadyExists(err) {
				e2eutils.RunCommand(fmt.Sprintf("kubectl -n %s delete marklogiccluster %s --ignore-not-found=true", haProxyNS, cr.Name))
				time.Sleep(3 * time.Second)
				if retryErr := client.Resources(haProxyNS).Create(ctx, cr); retryErr != nil {
					t.Fatalf("Failed to create MarklogicCluster after replacing stale resource: %v", retryErr)
				}
			} else {
				t.Fatalf("Failed to create MarklogicCluster: %v", err)
			}
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(cr, func(object k8s.Object) bool { return true }),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("pod ml-0 is ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), haProxyNS, "ml-0", 300*time.Second, true); err != nil {
			logDiagnostics(t, haProxyNS)
			t.Fatalf("ml-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing disabled is working", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("marklogic-haproxy.%s.svc.cluster.local", haProxyNS)
		url := "http://" + fqdn + ":8001"
		cmd := fmt.Sprintf("curl --anyauth -u %s:%s %s", adminUsername, adminPassword, url)
		time.Sleep(5 * time.Second)
		if _, err := utils.ExecCmdInPod("ml-0", haProxyNS, mlContainerName, cmd); err != nil {
			t.Fatalf("HAProxy request failed: %v", err)
		}
		return ctx
	})

	feature.Assess("HAProxy with path-based routing disabled keeps Digest authentication", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		fqdn := fmt.Sprintf("ml-0.ml.%s.svc.cluster.local", haProxyNS)
		url := "http://" + fqdn + ":8001"
		cmd := fmt.Sprintf("curl -I %s", url)
		res, err := utils.ExecCmdInPod("ml-0", haProxyNS, mlContainerName, cmd)
		if err != nil {
			t.Fatalf("Failed to check authentication method: %v", err)
		}
		if !strings.Contains(res, "WWW-Authenticate: Digest") {
			t.Fatalf("Expected Digest auth header, got: %s", res)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Keep the watched namespace and RBAC in place, but clean workload resources
		// so re-runs start from a known-good state.
		if err := c.Client().Resources(haProxyNS).Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete MarklogicCluster %s/%s: %v", haProxyNS, cr.Name, err)
		}
		cleanupHAProxyNamespaceArtifacts(haProxyNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
