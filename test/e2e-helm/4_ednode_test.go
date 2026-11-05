// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"fmt"
	"os/exec"
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
	edNodeNS     = "ml-ns-ednode" // must be in watchedNamespaces
	dnodeGrpName = "dnode"
	enodeGrpName = "enode"
)

var (
	edSecretName  = "ml-admin-secrets"
	edIncrReplica = int32(2)
	edGroups      = []*marklogicv1.MarklogicGroups{
		{
			Name:        dnodeGrpName,
			Replicas:    &replicas,
			IsBootstrap: true,
			GroupConfig: &marklogicv1.GroupConfig{
				Name:          "dnode",
				EnableXdqpSsl: true,
			},
		},
		{
			Name:     enodeGrpName,
			Replicas: &replicas,
			GroupConfig: &marklogicv1.GroupConfig{
				Name:          "enode",
				EnableXdqpSsl: true,
			},
		},
	}
	edCluster = &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mlclusterednode",
			Namespace: edNodeNS,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				SecretName: &edSecretName,
			},
			MarkLogicGroups: edGroups,
		},
	}
)

// cleanupStaleClusterData removes stale MarkLogic cluster data from PVCs that could
// interfere with test isolation. This ensures that consecutive test runs don't see
// leftover cluster registration data from previous runs.
func cleanupStaleClusterData(ctx context.Context, t *testing.T, c *envconf.Config, namespace string) error {
	client := c.Client()

	// Check if namespace exists; if not, skip cleanup
	ns := &corev1.Namespace{}
	err := client.Resources().Get(ctx, namespace, "", ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			t.Logf("Namespace %s not found, skipping cleanup", namespace)
			return nil
		}
		t.Logf("Warning: failed to check namespace %s: %v", namespace, err)
		return nil
	}

	// Delete any existing MarklogicCluster resources FIRST to allow PVCs to be cleaned
	mlCluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mlclusterednode",
			Namespace: namespace,
		},
	}
	if err := client.Resources(namespace).Delete(ctx, mlCluster); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete MarklogicCluster during cleanup: %v", err)
		}
	}

	// Wait for cluster to be deleted before deleting PVCs
	// This allows StatefulSet to clean up properly
	time.Sleep(3 * time.Second)

	// Delete PVCs explicitly to remove stale data
	// This is CRITICAL to prevent "stale host registration" errors
	// when the same pod name is reused in the next test run
	// Use --force --grace-period=0 to ensure immediate deletion instead of waiting for finalizers
	t.Logf("Force-deleting PVCs in namespace %s to remove stale cluster data", namespace)
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "pvc", "--all", "-n", namespace,
		"--ignore-not-found=true", "--force", "--grace-period=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Logf("Warning: failed to delete PVCs: %v, output: %s", err, string(output))
	}

	// Wait for PVCs to be actually deleted
	time.Sleep(2 * time.Second)

	return nil
}

// TestMlClusterWithEdnode verifies a two-group (dnode + enode) cluster is reconciled
// correctly in a watched namespace using only Role/RoleBinding RBAC from the Helm chart.
func TestMlClusterWithEdnode(t *testing.T) {
	trackTest(t)
	feature := features.New("MarklogicCluster with 2 MarkLogicGroups (dnode + enode)").
		WithLabel("type", "ednode")

	// Create namespace and admin secret, then deploy the CR.
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		// Clean up any stale data from previous test runs BEFORE checking namespace state
		cleanupStaleClusterData(ctx, t, c, edNodeNS)

		// Wait a bit to allow cleanup to take effect
		time.Sleep(3 * time.Second)

		// Wait for any previous terminating namespace to finish before creating a new one.
		ns := &corev1.Namespace{}
		for i := 0; i < 60; i++ {
			err := client.Resources().Get(ctx, edNodeNS, "", ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					break
				}
				t.Fatalf("Error checking namespace %s: %v", edNodeNS, err)
			}
			if ns.Status.Phase == corev1.NamespaceTerminating {
				if i == 59 {
					t.Fatalf("Timeout waiting for namespace %s to finish terminating", edNodeNS)
				}
				t.Logf("Namespace %s is terminating, waiting... (%d/60)", edNodeNS, i+1)
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}

		// After namespace is deleted, force-delete any lingering PVCs that may have stale MarkLogic data.
		// This prevents "duplicate registration" errors when the same pod names are reused.
		t.Logf("Force-deleting any lingering PVCs in namespace %s", edNodeNS)
		cmd := exec.CommandContext(ctx, "kubectl", "delete", "pvc", "--all", "-n", edNodeNS,
			"--ignore-not-found=true", "--force", "--grace-period=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Logf("Note: kubectl delete pvc output: %s (may be expected if namespace doesn't exist)", string(output))
		}

		// Wait to ensure PVCs are actually deleted
		time.Sleep(2 * time.Second)

		if err := client.Resources().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: edNodeNS},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create namespace %s: %v", edNodeNS, err)
		}
		marklogicv1.AddToScheme(client.Resources(edNodeNS).GetScheme())

		e2eutils.RunCommand(fmt.Sprintf(
			"kubectl -n %s delete secret %s --ignore-not-found=true",
			edNodeNS, edSecretName,
		))
		p := e2eutils.RunCommand(fmt.Sprintf(
			"kubectl -n %s create secret generic %s --from-literal=username=%s --from-literal=password=%s",
			edNodeNS, edSecretName, adminUsername, adminPassword,
		))
		if p.Err() != nil {
			t.Fatalf("Failed to create admin secret: %s", p.Result())
		}

		if err := client.Resources(edNodeNS).Create(ctx, edCluster); err != nil {
			t.Fatalf("Failed to create MarklogicCluster: %v", err)
		}

		// Wait for operator to process CR and activate credentials.
		// This ensures that when pods start, the operator user is ready for authentication.
		t.Log("Waiting for operator to activate credentials...")
		time.Sleep(15 * time.Second)

		if err := wait.For(
			conditions.New(client.Resources()).ResourceMatch(edCluster, func(object k8s.Object) bool {
				return true
			}),
			wait.WithTimeout(3*time.Minute),
			wait.WithInterval(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("MarklogicCluster CR exists", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var mc marklogicv1.MarklogicCluster
		if err := client.Resources().Get(ctx, "mlclusterednode", edNodeNS, &mc); err != nil {
			t.Fatalf("MarklogicCluster not found: %v", err)
		}
		return ctx
	})

	feature.Assess("dnode-0 pod is ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), edNodeNS, "dnode-0", 300*time.Second, true); err != nil {
			logDiagnostics(t, edNodeNS)
			t.Fatalf("dnode-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("enode-0 pod is ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), edNodeNS, "enode-0", 420*time.Second, true); err != nil {
			logDiagnostics(t, edNodeNS)
			t.Fatalf("enode-0 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("MarkLogic groups created", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Retrieve operator credentials for post-initialization API calls.
		operatorSecret := &corev1.Secret{}
		if err := c.Client().Resources(edNodeNS).Get(ctx, "mlclusterednode-operator", edNodeNS, operatorSecret); err != nil {
			t.Fatalf("Failed to read operator Secret: %v", err)
		}
		operatorUsername := "marklogic-kubernetes-operator"
		operatorPassword := string(operatorSecret.Data["password"])
		if operatorPassword == "" {
			t.Fatalf("Operator Secret has empty password")
		}

		url := "http://localhost:8002/manage/v2/groups"
		cmd := fmt.Sprintf("curl %s --anyauth -u %s:%s", url, operatorUsername, operatorPassword)
		output, err := utils.ExecCmdInPod("dnode-0", edNodeNS, mlContainerName, cmd)
		if err != nil {
			t.Fatalf("Failed to query groups: %v", err)
		}
		if !strings.Contains(output, "<nameref>dnode</nameref>") || !strings.Contains(output, "<nameref>enode</nameref>") {
			t.Logf("Groups output: %s", output)
			t.Fatal("expected dnode and enode groups in MarkLogic cluster")
		}
		return ctx
	})

	// Scale both groups to 2 replicas.
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var mc marklogicv1.MarklogicCluster
		if err := client.Resources().Get(ctx, "mlclusterednode", edNodeNS, &mc); err != nil {
			t.Fatal(err)
		}
		mc.Spec.MarkLogicGroups[0].Replicas = &edIncrReplica
		mc.Spec.MarkLogicGroups[1].Replicas = &edIncrReplica
		if err := client.Resources().Update(ctx, &mc); err != nil {
			t.Fatal(err)
		}
		return ctx
	})

	feature.Assess("dnode-1 pod created after scaling", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), edNodeNS, "dnode-1", 240*time.Second, true); err != nil {
			logDiagnostics(t, edNodeNS)
			t.Fatalf("dnode-1 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("enode-1 pod created after scaling", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := utils.WaitForPod(ctx, t, c.Client(), edNodeNS, "enode-1", 300*time.Second, true); err != nil {
			logDiagnostics(t, edNodeNS)
			t.Fatalf("enode-1 not ready: %v", err)
		}
		return ctx
	})

	feature.Assess("4 pods running after scaling", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		podList := &corev1.PodList{}
		if err := client.Resources(edNodeNS).List(ctx, podList, func(lo *metav1.ListOptions) {
			lo.LabelSelector = "app.kubernetes.io/name=marklogic"
		}); err != nil {
			t.Fatal(err)
		}
		if got := len(podList.Items); got != 4 {
			t.Fatalf("expected 4 pods after scaling, got %d", got)
		}
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := client.Resources(edNodeNS).Delete(ctx, edCluster); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete MarklogicCluster: %v", err)
		}

		// Clean up stale cluster data and PVCs to ensure test isolation
		cleanupStaleClusterData(ctx, t, c, edNodeNS)

		// Wait for resources to be fully cleaned up
		time.Sleep(5 * time.Second)

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
