// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

// Volume Resize – Cluster-Scoped E2E Test
//
// This test exercises the operator's volume-resizing feature against the
// cluster-scoped operator deployed by `make deploy` in TestMain.
//
// Two MarklogicCluster instances are created in TWO DIFFERENT NAMESPACES at
// the same time (one in ml-resize-a, one in ml-resize-b) and resized in
// parallel. This validates that the cluster-scoped operator can reconcile
// resize operations concurrently across namespaces.
//
// Result summary:
// At the end of the test a banner is printed for each namespace with
// PASS/FAIL plus the initial / requested / observed PVC capacity, so the
// outcome is easy to read at a glance in CI logs.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	frameworkutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

// resizeNamespaces are the two parallel test namespaces. They must NOT collide
// with any other test namespace in this package.
var resizeNamespaces = []string{"ml-resize-a", "ml-resize-b"}

const (
	resizeClusterName = "ml-resize-cluster"
	resizeGroupName   = "node"
	resizeInitialSize = "2Gi"
	resizeTargetSize  = "3Gi"
	resizeWaitTimeout = 15 * time.Minute
)

// resizeOutcome captures the per-namespace result for the final summary banner.
type resizeOutcome struct {
	namespace    string
	initialSize  string
	requestSize  string
	observedSize string
	phase        string
	passed       bool
	failReason   string
}

// TestVolumeResizeClusterScoped resizes PVCs in two namespaces concurrently
// against the cluster-scoped operator and prints a clear pass/fail summary.
func TestVolumeResizeClusterScoped(t *testing.T) {
	trackTest(t)
	feature := features.New("Volume Resize — Cluster-Scoped, Multi-Namespace").
		WithLabel("type", "volume-resize")

	// ── Pre-flight: storage class must allow expansion ────────────────────────
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := assertDefaultStorageClassExpandable(ctx, c.Client()); err != nil {
			t.Fatalf("Pre-flight failed: %v", err)
		}
		return ctx
	})

	// ── Create both namespaces and both clusters in parallel ──────────────────
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources().GetScheme())

		var wg sync.WaitGroup
		errCh := make(chan error, len(resizeNamespaces))
		for _, ns := range resizeNamespaces {
			ns := ns
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := createResizeNamespaceAndCluster(ctx, t, client, ns); err != nil {
					errCh <- fmt.Errorf("[%s] %w", ns, err)
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("Setup failed: %v", err)
		}
		return ctx
	})

	// ── Wait for node-0 in BOTH namespaces in parallel ────────────────────────
	feature.Assess("MarkLogic pods Ready in both namespaces", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var wg sync.WaitGroup
		errCh := make(chan error, len(resizeNamespaces))
		for _, ns := range resizeNamespaces {
			ns := ns
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := utils.WaitForPod(ctx, t, client, ns, "node-0", 5*time.Minute, true); err != nil {
					errCh <- fmt.Errorf("[%s] node-0 not ready: %w", ns, err)
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("%v", err)
		}
		return ctx
	})

	// ── Trigger resize and verify outcome in parallel ─────────────────────────
	feature.Assess("PVCs resized to target size in both namespaces", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			outcomes = make([]resizeOutcome, 0, len(resizeNamespaces))
		)
		for _, ns := range resizeNamespaces {
			ns := ns
			wg.Add(1)
			go func() {
				defer wg.Done()
				out := triggerAndWaitForResize(ctx, t, client, ns)
				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()
			}()
		}
		wg.Wait()

		printResizeSummary(t, "Volume Resize — Cluster-Scoped", outcomes)

		for _, o := range outcomes {
			if !o.passed {
				dumpResizeDiagnostics(t, o.namespace)
				t.Errorf("[%s] resize did NOT complete: %s", o.namespace, o.failReason)
			}
		}
		return ctx
	})

	// ── Teardown: delete both namespaces (drops cluster + PVCs) ───────────────
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		for _, ns := range resizeNamespaces {
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			if err := client.Resources().Delete(ctx, nsObj); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("Warning: failed to delete namespace %s: %v", ns, err)
			}
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// assertDefaultStorageClassExpandable returns nil only if the cluster's
// **default** StorageClass exists and has allowVolumeExpansion=true. The
// resize-test MarklogicCluster does not set spec.persistence.storageClassName,
// so PVCs are bound through the default SC; checking any random expandable SC
// would let the test proceed and then fail later during actual resize.
func assertDefaultStorageClassExpandable(ctx context.Context, client klient.Client) error {
	scList := &storagev1.StorageClassList{}
	if err := client.Resources().List(ctx, scList); err != nil {
		return fmt.Errorf("list StorageClasses: %w", err)
	}
	var defaults []storagev1.StorageClass
	for _, sc := range scList.Items {
		if isDefaultStorageClass(sc) {
			defaults = append(defaults, sc)
		}
	}
	switch len(defaults) {
	case 0:
		return fmt.Errorf("no default StorageClass found (annotation storageclass.kubernetes.io/is-default-class=true); volume resize cannot be tested")
	case 1:
		sc := defaults[0]
		if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
			return fmt.Errorf("default StorageClass %q has allowVolumeExpansion=false; volume resize cannot succeed", sc.Name)
		}
		return nil
	default:
		names := make([]string, 0, len(defaults))
		for _, sc := range defaults {
			names = append(names, sc.Name)
		}
		return fmt.Errorf("multiple default StorageClasses found (%s); cannot determine which one PVCs will bind to", strings.Join(names, ", "))
	}
}

// isDefaultStorageClass reports whether sc carries either the GA or the legacy
// beta default-class annotation set to "true".
func isDefaultStorageClass(sc storagev1.StorageClass) bool {
	if sc.Annotations == nil {
		return false
	}
	return sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
		sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}

// createResizeNamespaceAndCluster creates the namespace and a MarklogicCluster
// with persistence size = resizeInitialSize.
func createResizeNamespaceAndCluster(ctx context.Context, t *testing.T, client klient.Client, ns string) error {
	t.Logf("[%s] creating namespace + MarklogicCluster (size=%s)", ns, resizeInitialSize)

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: namespaceLabels()}}
	if err := client.Resources().Create(ctx, nsObj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace: %w", err)
	}

	r := int32(1)
	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: resizeClusterName, Namespace: ns},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			Persistence: &marklogicv1.Persistence{
				Enabled: true,
				Size:    resizeInitialSize,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        resizeGroupName,
					Replicas:    &r,
					IsBootstrap: true,
				},
			},
		},
	}
	if err := client.Resources(ns).Create(ctx, cluster); err != nil {
		return fmt.Errorf("create MarklogicCluster: %w", err)
	}
	return nil
}

// triggerAndWaitForResize patches the cluster persistence size to the target
// value and polls until either the MarklogicGroup VolumeResizeStatus reports
// Completed (and the PVC capacity matches) or the timeout elapses.
func triggerAndWaitForResize(ctx context.Context, t *testing.T, client klient.Client, ns string) resizeOutcome {
	out := resizeOutcome{namespace: ns, initialSize: resizeInitialSize, requestSize: resizeTargetSize}

	// Capture the current PVC capacity for the per-namespace summary.
	if size, err := minPVCCapacity(ctx, client, ns); err == nil {
		out.initialSize = size
	}

	// Patch spec.persistence.size on the MarklogicCluster (triggers reconcile).
	patch := []byte(fmt.Sprintf(`{"spec":{"persistence":{"size":"%s"}}}`, resizeTargetSize))
	cluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: resizeClusterName, Namespace: ns},
	}
	if err := client.Resources(ns).Patch(ctx, cluster, k8s.Patch{PatchType: types.MergePatchType, Data: patch}); err != nil {
		out.failReason = fmt.Sprintf("patch cluster: %v", err)
		return out
	}
	t.Logf("[%s] patched MarklogicCluster persistence.size → %s", ns, resizeTargetSize)

	// Poll until Completed or timeout.
	deadline := time.Now().Add(resizeWaitTimeout)
	for time.Now().Before(deadline) {
		grp := &marklogicv1.MarklogicGroup{}
		if err := client.Resources(ns).Get(ctx, resizeGroupName, ns, grp); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		if grp.Status.VolumeResizeStatus != nil {
			out.phase = string(grp.Status.VolumeResizeStatus.Phase)
		}
		obs, _ := minPVCCapacity(ctx, client, ns)
		out.observedSize = obs

		// Success criteria: phase=Completed AND every PVC reaches the target.
		if out.phase == string(marklogicv1.VolumeResizePhaseCompleted) && sizesEqual(obs, resizeTargetSize) {
			out.passed = true
			return out
		}
		// Hard-fail criteria: phase=Failed.
		if out.phase == string(marklogicv1.VolumeResizePhaseFailed) {
			out.failReason = fmt.Sprintf("phase=Failed reason=%s msg=%s",
				grp.Status.VolumeResizeStatus.Reason, grp.Status.VolumeResizeStatus.Message)
			return out
		}
		t.Logf("[%s] resize in progress: phase=%s observed=%s target=%s", ns, out.phase, obs, resizeTargetSize)
		time.Sleep(15 * time.Second)
	}
	out.failReason = fmt.Sprintf("timeout after %s (last phase=%s observed=%s)", resizeWaitTimeout, out.phase, out.observedSize)
	return out
}

// minPVCCapacity returns the smallest .status.capacity.storage across the PVCs
// owned by the resize StatefulSet in ns, formatted as a human-readable string.
func minPVCCapacity(ctx context.Context, client klient.Client, ns string) (string, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := client.Resources(ns).List(ctx, pvcs); err != nil {
		return "", err
	}
	var min *resource.Quantity
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		// Restrict to PVCs whose name belongs to the resize StatefulSet.
		if !strings.HasPrefix(pvc.Name, "datadir-"+resizeGroupName+"-") {
			continue
		}
		q, ok := pvc.Status.Capacity[corev1.ResourceStorage]
		if !ok {
			continue
		}
		if min == nil || q.Cmp(*min) < 0 {
			qq := q.DeepCopy()
			min = &qq
		}
	}
	if min == nil {
		return "", fmt.Errorf("no PVC capacity reported yet")
	}
	return min.String(), nil
}

// sizesEqual compares two storage size strings (e.g. "3Gi" == "3072Mi").
func sizesEqual(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return false
	}
	return qa.Cmp(qb) == 0
}

// dumpResizeDiagnostics prints, for a failing namespace, the resources most
// likely to explain why the resize did not complete: PVCs, the StatefulSet,
// the MarklogicCluster + MarklogicGroup CRs, recent Kubernetes events in the
// namespace, and the last 200 lines of the operator pod logs.
func dumpResizeDiagnostics(t *testing.T, ns string) {
	t.Helper()
	line := strings.Repeat("-", 78)
	t.Logf("\n%s\n  DIAGNOSTICS for namespace %s\n%s", line, ns, line)
	for _, cmd := range []string{
		fmt.Sprintf("kubectl get pvc -n %s -o wide", ns),
		fmt.Sprintf("kubectl describe pvc -n %s", ns),
		fmt.Sprintf("kubectl get statefulset -n %s -o wide", ns),
		fmt.Sprintf("kubectl get pods -n %s -o wide", ns),
		fmt.Sprintf("kubectl get marklogiccluster,marklogicgroup -n %s -o yaml", ns),
		fmt.Sprintf("kubectl get events -n %s --sort-by=.lastTimestamp", ns),
	} {
		p := frameworkutils.RunCommand(cmd)
		t.Logf("$ %s\n%s", cmd, p.Result())
	}
	p := frameworkutils.RunCommand(
		fmt.Sprintf("kubectl logs -n %s -l control-plane=controller-manager --tail=200 --prefix=true", namespace),
	)
	t.Logf("Operator logs (last 200 lines):\n%s", p.Result())
	t.Logf("%s\n", line)
}

// printResizeSummary prints an easy-to-read banner describing each namespace's
// outcome. Designed to stand out in CI log scrollback.
func printResizeSummary(t *testing.T, title string, outcomes []resizeOutcome) {
	t.Helper()
	line := strings.Repeat("=", 78)
	t.Logf("\n%s\n  %s — RESULT SUMMARY\n%s", line, title, line)
	for _, o := range outcomes {
		status := "✅ PASS"
		if !o.passed {
			status = "❌ FAIL"
		}
		t.Logf("  [%-12s] %s  initial=%-6s  requested=%-6s  observed=%-6s  phase=%s",
			o.namespace, status, o.initialSize, o.requestSize, o.observedSize, o.phase)
		if !o.passed && o.failReason != "" {
			t.Logf("                    reason: %s", o.failReason)
		}
	}
	t.Logf("%s\n", line)
}
