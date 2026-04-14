// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
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

// mlNsTestNS is listed in watchedNamespaces so the namespace-scoped operator
// has a Role/RoleBinding here and can reconcile MarklogicCluster resources.
const mlNsTestNS = "ml-ns-test"

var nsTestCluster = &marklogicv1.MarklogicCluster{
	TypeMeta: metav1.TypeMeta{
		APIVersion: "marklogic.progress.com/v1",
		Kind:       "MarklogicCluster",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name:      "ml-ns-cluster",
		Namespace: mlNsTestNS,
	},
	Spec: marklogicv1.MarklogicClusterSpec{
		Image: marklogicImage,
		Auth: &marklogicv1.AdminAuth{
			AdminUsername: &adminUsername,
			AdminPassword: &adminPassword,
		},
		MarkLogicGroups: []*marklogicv1.MarklogicGroups{
			{
				Name:        "node",
				Replicas:    &replicas,
				IsBootstrap: true,
			},
		},
	},
}

// TestMarklogicClusterNamespaceScoped deploys a MarklogicCluster into a watched namespace
// and verifies the operator reconciles it correctly using only the namespace-scoped
// Role/RoleBinding installed by the Helm chart (no ClusterRole backstop).
func TestMarklogicClusterNamespaceScoped(t *testing.T) {
	trackTest(t)
	feature := features.New("MarklogicCluster — Namespace-Scoped").
		WithLabel("type", "cluster-ns")

	// Create the watched test namespace.
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, mlNsTestNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", mlNsTestNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", mlNsTestNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", mlNsTestNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}
		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: mlNsTestNS, Labels: namespaceLabels()},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", mlNsTestNS, err)
		}
		return ctx
	})

	// Register the MarklogicCluster scheme and deploy the CR.
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources(mlNsTestNS).GetScheme())

		if err := client.Resources(mlNsTestNS).Create(ctx, nsTestCluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}
		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(nsTestCluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatalf("MarklogicCluster not created within timeout: %v", err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster CR exists", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var mc marklogicv1.MarklogicCluster
		if err := client.Resources().Get(ctx, nsTestCluster.Name, mlNsTestNS, &mc); err != nil {
			t.Fatalf("MarklogicCluster not found: %v", err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster pod running", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := utils.WaitForPod(ctx, t, client, mlNsTestNS, "node-0", 240*time.Second, true); err != nil {
			logDiagnostics(t, mlNsTestNS)
			t.Fatalf("Pod node-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(mlNsTestNS).Delete(ctx, nsTestCluster); err != nil {
			t.Logf("Warning: failed to delete MarklogicCluster: %v", err)
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mlNsTestNS}}
		if err := client.Resources().Delete(ctx, ns); err != nil {
			t.Logf("Warning: failed to delete namespace %s: %v", mlNsTestNS, err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
