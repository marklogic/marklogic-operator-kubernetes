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
)

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
		marklogicv1.AddToScheme(client.Resources(haProxyPathNS).GetScheme())
		if err := client.Resources(haProxyPathNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
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
		if err := utils.WaitForPod(ctx, t, c.Client(), haProxyPathNS, "ml-0", 120*time.Second, true); err != nil {
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
		utils.DeleteNS(ctx, c, haProxyPathNS)
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
		marklogicv1.AddToScheme(client.Resources(haProxyNS).GetScheme())
		if err := client.Resources(haProxyNS).Create(ctx, cr); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
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
		if err := utils.WaitForPod(ctx, t, c.Client(), haProxyNS, "ml-0", 120*time.Second, true); err != nil {
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
		utils.DeleteNS(ctx, c, haProxyNS)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
