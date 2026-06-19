// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

// Volume Resize – Namespace-Scoped E2E Test (Helm)
//
// Validates that the operator — installed via the Helm chart with
// scope.type=namespace and a watch list — can resize PVCs concurrently in
// TWO DIFFERENT WATCHED NAMESPACES (ml-ns-resize-a, ml-ns-resize-b).
//
// Both target namespaces MUST be present in test/e2e-helm/main_test.go's
// watchedNamespaces constant; otherwise the namespace-scoped operator has no
// Role/RoleBinding there and reconciliation will silently no-op.
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

// resizeNSNamespaces are the two parallel namespaces. They MUST be listed in
// watchedNamespaces in main_test.go.
var resizeNSNamespaces = []string{"ml-ns-resize-a", "ml-ns-resize-b"}

const (
	resizeNSClusterName  = "ml-ns-resize-cluster"
	resizeNSGroupName    = "node"
	resizeNSExtraPVCName = "logs"
	resizeNSInitialSize  = "2Gi"
	resizeNSTargetSize   = "3Gi"
	resizeNSWaitTimeout  = 15 * time.Minute
)

type resizeNSOutcome struct {
	namespace    string
	initialSize  string
	requestSize  string
	observedSize string
	phase        string
	passed       bool
	failReason   string
}

// TestVolumeResizeNamespaceScoped resizes PVCs in two watched namespaces
// concurrently and prints a clear pass/fail summary.
func TestVolumeResizeNamespaceScoped(t *testing.T) {
	trackTest(t)
	feature := features.New("Volume Resize — Namespace-Scoped, Multi-Namespace").
		WithLabel("type", "volume-resize-ns")

	// ── Pre-flight ─────────────────────────────────────────────────────────────
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if err := assertResizeNSNamespacesWatched(); err != nil {
			t.Fatalf("namespace-scoped Helm resize test misconfigured: %v", err)
		}
		if err := assertNSStorageClassExpandable(ctx, c.Client()); err != nil {
			t.Skipf("Skipping namespace-scoped Helm volume resize test: %v", err)
		}
		return ctx
	})

	// ── Create both clusters in parallel (namespaces already created in TestMain) ──
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources().GetScheme())

		var wg sync.WaitGroup
		errCh := make(chan error, len(resizeNSNamespaces))
		for _, ns := range resizeNSNamespaces {
			ns := ns
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := createNSResizeCluster(ctx, client, ns); err != nil {
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
	feature.Assess("MarkLogic pods Ready in both watched namespaces", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		var wg sync.WaitGroup
		errCh := make(chan error, len(resizeNSNamespaces))
		for _, ns := range resizeNSNamespaces {
			ns := ns
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := utils.WaitForPod(ctx, t, client, ns, "node-0", 5*time.Minute, true); err != nil {
					logDiagnostics(t, ns)
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
	feature.Assess("PVCs resized to target size in both watched namespaces", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			outcomes = make([]resizeNSOutcome, 0, len(resizeNSNamespaces))
		)
		for _, ns := range resizeNSNamespaces {
			ns := ns
			wg.Add(1)
			go func() {
				defer wg.Done()
				out := triggerNSResizeAndWait(ctx, t, client, ns)
				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()
			}()
		}
		wg.Wait()

		printNSResizeSummary(t, "Volume Resize — Namespace-Scoped (Helm)", outcomes)

		for _, o := range outcomes {
			if !o.passed {
				dumpNSResizeDiagnostics(t, o.namespace)
				t.Errorf("[%s] resize did NOT complete: %s", o.namespace, o.failReason)
			}
		}
		return ctx
	})

	// ── Teardown: delete the two clusters (namespaces survive — they are watched) ──
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		for _, ns := range resizeNSNamespaces {
			cluster := &marklogicv1.MarklogicCluster{
				ObjectMeta: metav1.ObjectMeta{Name: resizeNSClusterName, Namespace: ns},
			}
			if err := client.Resources(ns).Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("Warning: failed to delete cluster in %s: %v", ns, err)
			}
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func assertNSStorageClassExpandable(ctx context.Context, client klient.Client) error {
	scList := &storagev1.StorageClassList{}
	if err := client.Resources().List(ctx, scList); err != nil {
		return fmt.Errorf("list StorageClasses: %w", err)
	}
	var defaults []storagev1.StorageClass
	for _, sc := range scList.Items {
		if isNSDefaultStorageClass(sc) {
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

// assertResizeNSNamespacesWatched validates that all resize namespaces are in
// the Helm operator watch list configured in main_test.go.
func assertResizeNSNamespacesWatched() error {
	watched := make(map[string]struct{})
	for _, ns := range strings.Split(watchedNamespaces, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		watched[ns] = struct{}{}
	}

	missing := make([]string, 0)
	for _, ns := range resizeNSNamespaces {
		if _, ok := watched[ns]; !ok {
			missing = append(missing, ns)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing from watchedNamespaces: %s", strings.Join(missing, ", "))
	}
	return nil
}

// isNSDefaultStorageClass reports whether sc carries either the GA or the
// legacy beta default-class annotation set to "true".
func isNSDefaultStorageClass(sc storagev1.StorageClass) bool {
	if sc.Annotations == nil {
		return false
	}
	return sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
		sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}

func createNSResizeCluster(ctx context.Context, client klient.Client, ns string) error {
	r := int32(1)
	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: resizeNSClusterName, Namespace: ns},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: marklogicImage,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			Persistence: &marklogicv1.Persistence{
				Enabled: true,
				Size:    resizeNSInitialSize,
			},
			AdditionalVolumeClaimTemplates: &[]corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: resizeNSExtraPVCName},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(resizeNSInitialSize),
							},
						},
					},
				},
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        resizeNSGroupName,
					Replicas:    &r,
					IsBootstrap: true,
				},
			},
		},
	}
	if err := client.Resources(ns).Create(ctx, cluster); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create MarklogicCluster: %w", err)
	}
	return nil
}

func triggerNSResizeAndWait(ctx context.Context, t *testing.T, client klient.Client, ns string) resizeNSOutcome {
	out := resizeNSOutcome{namespace: ns, initialSize: resizeNSInitialSize, requestSize: resizeNSTargetSize}

	templatePrefixes := map[string]string{
		"datadir":            "datadir-" + resizeNSGroupName + "-",
		resizeNSExtraPVCName: resizeNSExtraPVCName + "-" + resizeNSGroupName + "-",
	}

	if sizes, err := minNSPVCCapacityByTemplate(ctx, client, ns, templatePrefixes); err == nil {
		out.initialSize = formatNSTemplateSizes(sizes)
	}

	patch := []byte(fmt.Sprintf(`{"spec":{"persistence":{"size":"%s"},"additionalVolumeClaimTemplates":[{"metadata":{"name":"%s"},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"%s"}}}}]}}`,
		resizeNSTargetSize, resizeNSExtraPVCName, resizeNSTargetSize))
	cluster := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: resizeNSClusterName, Namespace: ns},
	}
	if err := client.Resources(ns).Patch(ctx, cluster, k8s.Patch{PatchType: types.MergePatchType, Data: patch}); err != nil {
		out.failReason = fmt.Sprintf("patch cluster: %v", err)
		return out
	}
	t.Logf("[%s] patched MarklogicCluster persistence.size + additionalVolumeClaimTemplates[%s].storage → %s", ns, resizeNSExtraPVCName, resizeNSTargetSize)

	deadline := time.Now().Add(resizeNSWaitTimeout)
	for time.Now().Before(deadline) {
		grp := &marklogicv1.MarklogicGroup{}
		if err := client.Resources(ns).Get(ctx, resizeNSGroupName, ns, grp); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		if grp.Status.VolumeResizeStatus != nil {
			out.phase = string(grp.Status.VolumeResizeStatus.Phase)
		}
		sizes, _ := minNSPVCCapacityByTemplate(ctx, client, ns, templatePrefixes)
		out.observedSize = formatNSTemplateSizes(sizes)

		if out.phase == string(marklogicv1.VolumeResizePhaseCompleted) &&
			nsTemplateSizeEquals(sizes, "datadir", resizeNSTargetSize) &&
			nsTemplateSizeEquals(sizes, resizeNSExtraPVCName, resizeNSTargetSize) {
			out.passed = true
			return out
		}
		if out.phase == string(marklogicv1.VolumeResizePhaseFailed) {
			out.failReason = fmt.Sprintf("phase=Failed reason=%s msg=%s",
				grp.Status.VolumeResizeStatus.Reason, grp.Status.VolumeResizeStatus.Message)
			return out
		}
		t.Logf("[%s] resize in progress: phase=%s observed=%s target=%s", ns, out.phase, out.observedSize, resizeNSTargetSize)
		time.Sleep(15 * time.Second)
	}
	out.failReason = fmt.Sprintf("timeout after %s (last phase=%s observed=%s)", resizeNSWaitTimeout, out.phase, out.observedSize)
	return out
}

func minNSPVCCapacityByTemplate(ctx context.Context, client klient.Client, ns string, templatePrefixes map[string]string) (map[string]string, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := client.Resources(ns).List(ctx, pvcs); err != nil {
		return nil, err
	}
	minByTemplate := make(map[string]*resource.Quantity, len(templatePrefixes))
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		matchedTemplate := ""
		for templateName, prefix := range templatePrefixes {
			if strings.HasPrefix(pvc.Name, prefix) {
				matchedTemplate = templateName
				break
			}
		}
		if matchedTemplate == "" {
			continue
		}
		q, ok := pvc.Status.Capacity[corev1.ResourceStorage]
		if !ok {
			continue
		}
		min := minByTemplate[matchedTemplate]
		if min == nil || q.Cmp(*min) < 0 {
			qq := q.DeepCopy()
			minByTemplate[matchedTemplate] = &qq
		}
	}
	out := make(map[string]string, len(templatePrefixes))
	for templateName := range templatePrefixes {
		if minByTemplate[templateName] == nil {
			return nil, fmt.Errorf("no PVC capacity reported yet for template %s", templateName)
		}
		out[templateName] = minByTemplate[templateName].String()
	}
	return out, nil
}

func nsTemplateSizeEquals(sizes map[string]string, templateName, target string) bool {
	if sizes == nil {
		return false
	}
	current, ok := sizes[templateName]
	if !ok {
		return false
	}
	return nsSizesEqual(current, target)
}

func formatNSTemplateSizes(sizes map[string]string) string {
	if sizes == nil {
		return ""
	}
	return fmt.Sprintf("datadir=%s %s=%s", sizes["datadir"], resizeNSExtraPVCName, sizes[resizeNSExtraPVCName])
}

func nsSizesEqual(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return false
	}
	return qa.Cmp(qb) == 0
}

func printNSResizeSummary(t *testing.T, title string, outcomes []resizeNSOutcome) {
	t.Helper()
	line := strings.Repeat("=", 78)
	t.Logf("\n%s\n  %s — RESULT SUMMARY\n%s", line, title, line)
	for _, o := range outcomes {
		status := "✅ PASS"
		if !o.passed {
			status = "❌ FAIL"
		}
		t.Logf("  [%-14s] %s  initial=%-6s  requested=%-6s  observed=%-6s  phase=%s",
			o.namespace, status, o.initialSize, o.requestSize, o.observedSize, o.phase)
		if !o.passed && o.failReason != "" {
			t.Logf("                      reason: %s", o.failReason)
		}
	}
	t.Logf("%s\n", line)
}

// dumpNSResizeDiagnostics prints PVCs, StatefulSet, MarklogicCluster /
// MarklogicGroup CRs, recent Kubernetes events for the namespace, and the
// last 200 lines of the operator pod logs. Called only when a resize fails
// so passing tests stay quiet.
func dumpNSResizeDiagnostics(t *testing.T, ns string) {
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
		fmt.Sprintf("kubectl logs -n %s -l control-plane=controller-manager --tail=200 --prefix=true", helmNS),
	)
	t.Logf("Operator logs (last 200 lines):\n%s", p.Result())
	t.Logf("%s\n", line)
}
