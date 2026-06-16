// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	frameworkutils "sigs.k8s.io/e2e-framework/pkg/utils"
)

const (
	dynamicE2ENamespace       = "ml-dynamic-host"
	dynamicE2EClusterName     = "ml-dynamic-cluster"
	dynamicE2EBootstrapGroup  = "node"
	dynamicE2EDynamicGroup    = "dynamic"
	dynamicE2EClusterDomain   = "cluster.local"
	dynamicE2ETokenDuration   = "PT15M"
	dynamicE2EMLContainerName = "marklogic-server"
	dynamicE2EDefaultImage    = "progressofficial/marklogic-db:12.0.0-ubi9-rootless-2.2.2"

	dynamicE2EBootstrapReadyTimeout = 10 * time.Minute
	dynamicE2EStatusTimeout         = 12 * time.Minute
	dynamicE2ERecoveryIdleTimeout   = 110 * time.Second
	dynamicE2ERecoveryPodTimeout    = 110 * time.Second
	dynamicE2ERecoveryHostsTimeout  = 110 * time.Second
	dynamicE2EPodDeleteTimeout      = 6 * time.Minute
	dynamicE2ENamespaceResetTimeout = 4 * time.Minute
	dynamicE2ERetryInterval         = 5 * time.Second
)

// TestDynamicHostLifecycleClusterScoped validates dynamic-host lifecycle in a
// cluster-scoped deployment:
// 1) initial dynamic reconcile
// 2) scale-up
// 3) membership-loss recovery (restart-recovery equivalent signal)
// 4) scale-down
// 5) scale-to-zero
func TestDynamicHostLifecycleClusterScoped(t *testing.T) {
	trackTest(t)

	feature := features.New("Dynamic Host Lifecycle — Cluster-Scoped").
		WithLabel("type", "dynamic-host")

	var (
		hostOneIDBeforeRecovery string
	)

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		marklogicv1.AddToScheme(client.Resources().GetScheme())
		if err := createDynamicHostNamespaceAndCluster(ctx, client); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}
		return ctx
	})

	feature.Assess("Dynamic resources reconcile and initial host joins", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()

		if err := ensureDynamicGroupResourcesMaterialized(ctx, t, client); err != nil {
			dumpDynamicDiagnostics(t)
			t.Fatalf("dynamic child resources were not materialized: %v", err)
		}

		if err := utils.WaitForPod(ctx, t, client, dynamicE2ENamespace, dynamicBootstrapPodName(), dynamicE2EBootstrapReadyTimeout, true); err != nil {
			t.Fatalf("bootstrap pod not ready: %v", err)
		}
		if err := utils.WaitForPod(ctx, t, client, dynamicE2ENamespace, dynamicPodName(0), dynamicE2EBootstrapReadyTimeout, true); err != nil {
			t.Fatalf("dynamic pod-0 not ready: %v", err)
		}

		group := &marklogicv1.MarklogicGroup{}
		if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, group); err != nil {
			t.Fatalf("failed to get dynamic MarklogicGroup: %v", err)
		}
		if !group.Spec.IsDynamic {
			t.Fatalf("expected child MarklogicGroup spec.isDynamic=true")
		}
		if group.Spec.Persistence == nil || group.Spec.Persistence.Enabled {
			t.Fatalf("expected dynamic child MarklogicGroup persistence.enabled=false by default")
		}
		if group.Spec.UpdateStrategy != appsv1.RollingUpdateStatefulSetStrategyType {
			t.Fatalf("expected dynamic child updateStrategy=RollingUpdate, got %q", group.Spec.UpdateStrategy)
		}

		assertDynamicResourceLabels(ctx, t, client)
		assertNoDynamicDatadirPVC(ctx, t, client)

		idleGroup := waitForDynamicIdle(t, ctx, client, 1, 1)

		hostZero, ok := findHostStatus(idleGroup, dynamicPodName(0))
		if !ok {
			t.Fatalf("status.dynamic.hosts missing %s", dynamicPodName(0))
		}
		if hostZero.HostID == "" {
			t.Fatalf("expected non-empty hostId for %s", dynamicPodName(0))
		}
		if hostZero.State != "joined" && hostZero.State != "rejoined" {
			t.Fatalf("expected host state joined/rejoined for %s, got %q", dynamicPodName(0), hostZero.State)
		}

		assertAllowDynamicHostsEnabled(t)
		waitForGroupHostsContainment(t, dynamicGroupHostFQDNs(0), nil, 2*time.Minute)
		assertDynamicEventPresent(ctx, t, client, []string{"DynamicReconciling", "DynamicIdle"}, 2*time.Minute)
		return ctx
	})

	feature.Assess("Scale-up joins additional dynamic host", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := setDynamicReplicas(ctx, client, 2); err != nil {
			t.Fatalf("failed to scale dynamic group to 2: %v", err)
		}

		if err := utils.WaitForPod(ctx, t, client, dynamicE2ENamespace, dynamicPodName(1), dynamicE2EStatusTimeout, true); err != nil {
			t.Fatalf("dynamic pod-1 not ready after scale-up: %v", err)
		}

		idleGroup := waitForDynamicIdle(t, ctx, client, 2, 2)

		hostZero, ok := findHostStatus(idleGroup, dynamicPodName(0))
		if !ok || hostZero.HostID == "" {
			t.Fatalf("expected host status with hostId for %s after scale-up", dynamicPodName(0))
		}
		hostOne, ok := findHostStatus(idleGroup, dynamicPodName(1))
		if !ok || hostOne.HostID == "" {
			t.Fatalf("expected host status with hostId for %s after scale-up", dynamicPodName(1))
		}
		hostOneIDBeforeRecovery = hostOne.HostID

		waitForGroupHostsContainment(t, dynamicGroupHostFQDNs(0, 1), nil, 2*time.Minute)
		return ctx
	})

	feature.Assess("Membership-loss recovery rejoins existing pod", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if hostOneIDBeforeRecovery == "" {
			t.Fatalf("precondition failed: missing host-id for %s", dynamicPodName(1))
		}

		if err := removeDynamicHostByID(t, hostOneIDBeforeRecovery); err != nil {
			t.Fatalf("failed to remove host-id %s from MarkLogic: %v", hostOneIDBeforeRecovery, err)
		}

		// Ensure we actually observed membership loss before expecting reconciliation.
		waitForGroupHostsContainment(t, nil, []string{dynamicHostFQDN(dynamicPodName(1)), hostOneIDBeforeRecovery}, 90*time.Second)
		if err := kickDynamicGroupReconcile(ctx, client); err != nil {
			t.Logf("dynamic group reconcile kick failed: %v", err)
		}

		// Wait for the operator to start processing the membership loss (leave Idle).
		// Without this gate, waitForDynamicIdle may catch the stale pre-loss Idle
		// reading and return immediately before recovery has begun.
		waitForDynamicPhaseNotIdle(t, ctx, client, 45*time.Second)
		ensureDynamicPodRestarted(t, ctx, client, dynamicPodName(1))

		idleGroup := waitForDynamicIdleWithPodRecovery(t, ctx, client, 2, 2, dynamicPodName(1), dynamicE2ERecoveryIdleTimeout)
		hostOneAfter, ok := findHostStatus(idleGroup, dynamicPodName(1))
		if !ok || hostOneAfter.HostID == "" {
			t.Fatalf("expected recovered host status for %s", dynamicPodName(1))
		}
		if hostOneAfter.State != "joined" && hostOneAfter.State != "rejoined" {
			t.Fatalf("expected recovered state joined/rejoined for %s, got %q", dynamicPodName(1), hostOneAfter.State)
		}

		waitForGroupHostsContainment(t, dynamicGroupHostFQDNs(0, 1), nil, dynamicE2ERecoveryHostsTimeout)
		assertDynamicEventPresent(ctx, t, client, []string{"ClusterRestartDetected", "DynamicReconciling", "DynamicIdle"}, 2*time.Minute)
		return ctx
	})

	feature.Assess("Scale-down removes highest ordinal before pod deletion", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := setDynamicReplicas(ctx, client, 1); err != nil {
			t.Fatalf("failed to scale dynamic group to 1: %v", err)
		}

		// For EmptyDir-backed dynamic hosts, membership must be removed first.
		waitForGroupHostsContainment(t, nil, []string{dynamicHostFQDN(dynamicPodName(1))}, 2*time.Minute)

		if err := waitForPodDeleted(ctx, client, dynamicE2ENamespace, dynamicPodName(1), dynamicE2EPodDeleteTimeout); err != nil {
			t.Fatalf("dynamic pod-1 was not deleted after scale-down: %v", err)
		}

		idleGroup := waitForDynamicIdle(t, ctx, client, 1, 1)
		if _, ok := findHostStatus(idleGroup, dynamicPodName(1)); ok {
			t.Fatalf("expected %s host status to be pruned after scale-down", dynamicPodName(1))
		}
		waitForGroupHostsContainment(t, dynamicGroupHostFQDNs(0), []string{dynamicHostFQDN(dynamicPodName(1))}, 2*time.Minute)
		return ctx
	})

	feature.Assess("Scale-to-zero cleans up all dynamic hosts", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		if err := setDynamicReplicas(ctx, client, 0); err != nil {
			t.Fatalf("failed to scale dynamic group to 0: %v", err)
		}

		waitForGroupHostsContainment(t, nil, []string{dynamicHostFQDN(dynamicPodName(0))}, 2*time.Minute)
		if err := waitForPodDeleted(ctx, client, dynamicE2ENamespace, dynamicPodName(0), dynamicE2EPodDeleteTimeout); err != nil {
			t.Fatalf("dynamic pod-0 was not deleted after scale-to-zero: %v", err)
		}

		idleGroup := waitForDynamicIdle(t, ctx, client, 0, 0)
		if idleGroup.Status.Dynamic.LocalReadyReplicas != 0 {
			t.Fatalf("expected localReadyReplicas=0 at scale-to-zero, got %d", idleGroup.Status.Dynamic.LocalReadyReplicas)
		}
		if len(idleGroup.Status.Dynamic.Hosts) != 0 {
			t.Fatalf("expected no dynamic host entries at scale-to-zero, got %d", len(idleGroup.Status.Dynamic.Hosts))
		}

		waitForGroupHostsContainment(t, nil, dynamicGroupHostFQDNs(0, 1), 2*time.Minute)
		assertDynamicEventPresent(ctx, t, client, []string{"DynamicIdle"}, 2*time.Minute)
		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client := c.Client()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dynamicE2ENamespace}}
		if err := client.Resources().Delete(ctx, nsObj); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete namespace %s: %v", dynamicE2ENamespace, err)
		}
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func createDynamicHostNamespaceAndCluster(ctx context.Context, client klient.Client) error {
	if err := ensureFreshDynamicHostNamespace(ctx, client); err != nil {
		return err
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   dynamicE2ENamespace,
			Labels: namespaceLabels(),
		},
	}
	if err := client.Resources().Create(ctx, ns); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}

	bootstrapReplicas := int32(1)
	dynamicReplicas := int32(1)
	cluster := &marklogicv1.MarklogicCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "marklogic.progress.com/v1",
			Kind:       "MarklogicCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dynamicE2EClusterName,
			Namespace: dynamicE2ENamespace,
		},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: dynamicHostTestImage(),
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &adminUsername,
				AdminPassword: &adminPassword,
			},
			Persistence: &marklogicv1.Persistence{
				Enabled: false,
				Size:    "10Gi",
			},
			ClusterDomain: dynamicE2EClusterDomain,
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{
				{
					Name:        dynamicE2EBootstrapGroup,
					Replicas:    &bootstrapReplicas,
					IsBootstrap: true,
				},
				{
					Name:      dynamicE2EDynamicGroup,
					Replicas:  &dynamicReplicas,
					IsDynamic: true,
					GroupConfig: &marklogicv1.GroupConfig{
						Name:          dynamicE2EDynamicGroup,
						EnableXdqpSsl: true,
					},
					Dynamic: &marklogicv1.DynamicGroupConfig{
						TokenDuration: dynamicE2ETokenDuration,
					},
				},
			},
		},
	}

	if err := client.Resources(dynamicE2ENamespace).Create(ctx, cluster); err != nil {
		return fmt.Errorf("create MarklogicCluster: %w", err)
	}
	return nil
}

func ensureFreshDynamicHostNamespace(ctx context.Context, client klient.Client) error {
	ns := &corev1.Namespace{}
	err := client.Resources().Get(ctx, dynamicE2ENamespace, "", ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get namespace %s: %w", dynamicE2ENamespace, err)
	}

	if err := deleteStaleMarkLogicResourcesInNamespace(ctx, client); err != nil {
		return fmt.Errorf("delete stale MarkLogic resources in namespace %s: %w", dynamicE2ENamespace, err)
	}

	if err := client.Resources().Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", dynamicE2ENamespace, err)
	}

	if err := waitForNamespaceDeletion(ctx, client, dynamicE2ENamespace, dynamicE2ENamespaceResetTimeout); err == nil {
		return nil
	}

	if err := forceRemoveNamespaceFinalizers(dynamicE2ENamespace); err != nil {
		return fmt.Errorf("force-remove finalizers for namespace %s: %w", dynamicE2ENamespace, err)
	}

	if err := waitForNamespaceDeletion(ctx, client, dynamicE2ENamespace, 90*time.Second); err != nil {
		return err
	}

	return nil
}

func waitForNamespaceDeletion(ctx context.Context, client klient.Client, nsName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ns := &corev1.Namespace{}
		err := client.Resources().Get(ctx, nsName, "", ns)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("check namespace %s deletion status: %w", nsName, err)
		}
		time.Sleep(dynamicE2ERetryInterval)
	}

	return fmt.Errorf("timeout waiting for namespace %s to be deleted", nsName)
}

func forceRemoveNamespaceFinalizers(nsName string) error {
	cmd := fmt.Sprintf("kubectl patch namespace %s --type=merge -p '{\"spec\":{\"finalizers\":[]}}'", nsName)
	p := frameworkutils.RunCommand(cmd)
	if p.Err() != nil {
		return fmt.Errorf("%w: %s", p.Err(), p.Result())
	}
	return nil
}

func deleteStaleMarkLogicResourcesInNamespace(ctx context.Context, client klient.Client) error {
	groupList := &marklogicv1.MarklogicGroupList{}
	if err := client.Resources(dynamicE2ENamespace).List(ctx, groupList); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list MarklogicGroups: %w", err)
	}
	for i := range groupList.Items {
		group := groupList.Items[i].DeepCopy()
		if len(group.Finalizers) > 0 {
			group.Finalizers = nil
			if err := client.Resources(dynamicE2ENamespace).Update(ctx, group); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("clear finalizers on MarklogicGroup %s: %w", group.Name, err)
			}
		}
		if err := client.Resources(dynamicE2ENamespace).Delete(ctx, group); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete MarklogicGroup %s: %w", group.Name, err)
		}
	}

	clusterList := &marklogicv1.MarklogicClusterList{}
	if err := client.Resources(dynamicE2ENamespace).List(ctx, clusterList); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list MarklogicClusters: %w", err)
	}
	for i := range clusterList.Items {
		cluster := clusterList.Items[i].DeepCopy()
		if len(cluster.Finalizers) > 0 {
			cluster.Finalizers = nil
			if err := client.Resources(dynamicE2ENamespace).Update(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("clear finalizers on MarklogicCluster %s: %w", cluster.Name, err)
			}
		}
		if err := client.Resources(dynamicE2ENamespace).Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete MarklogicCluster %s: %w", cluster.Name, err)
		}
	}

	// Delete StatefulSets before pods: if pods are deleted while StatefulSets remain,
	// the StatefulSet controller recreates the pods (including operator-managed finalizers)
	// which blocks namespace deletion.
	stsList := &appsv1.StatefulSetList{}
	if err := client.Resources(dynamicE2ENamespace).List(ctx, stsList); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list StatefulSets: %w", err)
	}
	for i := range stsList.Items {
		sts := stsList.Items[i].DeepCopy()
		if len(sts.Finalizers) > 0 {
			sts.Finalizers = nil
			if err := client.Resources(dynamicE2ENamespace).Update(ctx, sts); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("clear finalizers on StatefulSet %s: %w", sts.Name, err)
			}
		}
		if err := client.Resources(dynamicE2ENamespace).Delete(ctx, sts); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete StatefulSet %s: %w", sts.Name, err)
		}
	}

	podList := &corev1.PodList{}
	if err := client.Resources(dynamicE2ENamespace).List(ctx, podList); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list pods: %w", err)
	}
	for i := range podList.Items {
		pod := podList.Items[i].DeepCopy()
		if len(pod.Finalizers) > 0 {
			pod.Finalizers = nil
			if err := client.Resources(dynamicE2ENamespace).Update(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("clear finalizers on pod %s: %w", pod.Name, err)
			}
		}
		if err := client.Resources(dynamicE2ENamespace).Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
	}

	return nil
}

func dynamicHostTestImage() string {
	if strings.TrimSpace(marklogicImage) != "" {
		return marklogicImage
	}
	return dynamicE2EDefaultImage
}

func setDynamicReplicas(ctx context.Context, client klient.Client, replicas int32) error {
	cluster := &marklogicv1.MarklogicCluster{}
	if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EClusterName, dynamicE2ENamespace, cluster); err != nil {
		return fmt.Errorf("get MarklogicCluster: %w", err)
	}

	updated := false
	for _, group := range cluster.Spec.MarkLogicGroups {
		if group != nil && group.Name == dynamicE2EDynamicGroup {
			r := replicas
			group.Replicas = &r
			updated = true
			break
		}
	}
	if !updated {
		return fmt.Errorf("dynamic group %q not found in cluster spec", dynamicE2EDynamicGroup)
	}

	if err := client.Resources(dynamicE2ENamespace).Update(ctx, cluster); err != nil {
		return fmt.Errorf("update MarklogicCluster replicas: %w", err)
	}
	return nil
}

// waitForDynamicPhaseNotIdle polls until the dynamic group's phase is no longer
// "Idle", indicating the operator detected a change and started reconciling.
// If the phase stays Idle for the entire timeout, it returns silently — the
// operator may have recovered before the first poll.
func waitForDynamicPhaseNotIdle(t *testing.T, ctx context.Context, client klient.Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		group := &marklogicv1.MarklogicGroup{}
		if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, group); err != nil {
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}
		if group.Status.Dynamic == nil || group.Status.Dynamic.Phase != "Idle" {
			phase := "<nil>"
			if group.Status.Dynamic != nil {
				phase = group.Status.Dynamic.Phase
			}
			t.Logf("operator transitioned away from Idle (phase=%q) — proceeding to waitForDynamicIdle", phase)
			return
		}
		time.Sleep(dynamicE2ERetryInterval)
	}
	t.Logf("operator stayed Idle throughout phase-not-idle window; proceeding anyway")
}

func waitForDynamicIdle(t *testing.T, ctx context.Context, client klient.Client, desiredReplicas, readyReplicas int32) *marklogicv1.MarklogicGroup {
	t.Helper()
	deadline := time.Now().Add(dynamicE2EStatusTimeout)
	for time.Now().Before(deadline) {
		group := &marklogicv1.MarklogicGroup{}
		if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, group); err != nil {
			t.Logf("waiting for dynamic status: failed to fetch MarklogicGroup: %v", err)
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}
		if group.Status.Dynamic == nil {
			t.Logf("waiting for dynamic status: status.dynamic is nil")
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}

		ds := group.Status.Dynamic
		t.Logf("dynamic status: phase=%s reason=%s desired=%d localReady=%d ready=%d message=%s", ds.Phase, ds.Reason, ds.DesiredReplicas, ds.LocalReadyReplicas, ds.ReadyReplicas, ds.Message)

		if ds.Phase == "Failed" {
			dumpDynamicDiagnostics(t)
			t.Fatalf("dynamic group entered Failed state: reason=%s message=%s", ds.Reason, ds.Message)
		}

		if ds.Phase == "Idle" && ds.DesiredReplicas == desiredReplicas && ds.ReadyReplicas == readyReplicas {
			return group
		}
		time.Sleep(dynamicE2ERetryInterval)
	}

	dumpDynamicDiagnostics(t)
	t.Fatalf("timeout waiting for dynamic group Idle desired=%d ready=%d", desiredReplicas, readyReplicas)
	return nil
}

func waitForDynamicIdleWithPodRecovery(t *testing.T, ctx context.Context, client klient.Client, desiredReplicas, readyReplicas int32, recoveryPodName string, timeout time.Duration) *marklogicv1.MarklogicGroup {
	t.Helper()
	if timeout <= 0 {
		timeout = dynamicE2EStatusTimeout
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		group := &marklogicv1.MarklogicGroup{}
		if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, group); err != nil {
			t.Logf("waiting for dynamic status: failed to fetch MarklogicGroup: %v", err)
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}
		if group.Status.Dynamic == nil {
			t.Logf("waiting for dynamic status: status.dynamic is nil")
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}

		ds := group.Status.Dynamic
		t.Logf("dynamic status: phase=%s reason=%s desired=%d localReady=%d ready=%d message=%s", ds.Phase, ds.Reason, ds.DesiredReplicas, ds.LocalReadyReplicas, ds.ReadyReplicas, ds.Message)

		if ds.Phase == "Failed" {
			dumpDynamicDiagnostics(t)
			t.Fatalf("dynamic group entered Failed state: reason=%s message=%s", ds.Reason, ds.Message)
		}

		if ds.Phase == "Idle" && ds.DesiredReplicas == desiredReplicas && ds.ReadyReplicas == readyReplicas {
			return group
		}

		// During membership-loss recovery, a dynamic pod can land in Failed while
		// the operator waits for deletion before recreating/rejoining it. If that
		// happens, force-delete the failed pod so reconciliation can proceed.
		if strings.Contains(ds.Message, "waiting for pod deletion") || strings.Contains(ds.Message, "waiting for local-ready pods to join MarkLogic") {
			pod := &corev1.Pod{}
			err := client.Resources(dynamicE2ENamespace).Get(ctx, recoveryPodName, dynamicE2ENamespace, pod)
			if err == nil && podNeedsRecoveryDelete(pod) {
				t.Logf("forcing delete of recovery pod %s (phase=%s deleting=%t) to unblock dynamic reconciliation", recoveryPodName, pod.Status.Phase, pod.DeletionTimestamp != nil)
				if len(pod.Finalizers) > 0 {
					pod.Finalizers = nil
					_ = client.Resources(dynamicE2ENamespace).Update(ctx, pod)
				}
				frameworkutils.RunCommand(fmt.Sprintf("kubectl delete pod %s -n %s --ignore-not-found --grace-period=0 --force", recoveryPodName, dynamicE2ENamespace))
			}
		}

		time.Sleep(dynamicE2ERetryInterval)
	}

	dumpDynamicDiagnostics(t)
	t.Fatalf("timeout waiting for dynamic group Idle desired=%d ready=%d", desiredReplicas, readyReplicas)
	return nil
}

func podNeedsRecoveryDelete(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.DeletionTimestamp != nil {
		return true
	}
	if pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return true
		}
	}
	return false
}

func assertDynamicResourceLabels(ctx context.Context, t *testing.T, client klient.Client) {
	t.Helper()

	sts := &appsv1.StatefulSet{}
	if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, sts); err != nil {
		t.Fatalf("failed to get dynamic StatefulSet: %v", err)
	}
	if sts.Spec.Selector.MatchLabels["app.kubernetes.io/component"] != "dynamic-host" {
		t.Fatalf("expected dynamic StatefulSet selector component label dynamic-host")
	}
	if sts.Spec.Template.Labels["app.kubernetes.io/component"] != "dynamic-host" {
		t.Fatalf("expected dynamic StatefulSet pod template component label dynamic-host")
	}

	headless := &corev1.Service{}
	if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, headless); err != nil {
		t.Fatalf("failed to get dynamic headless Service: %v", err)
	}
	if headless.Spec.Selector["app.kubernetes.io/component"] != "dynamic-host" {
		t.Fatalf("expected dynamic headless Service selector component label dynamic-host")
	}

	clusterServiceName := dynamicE2EDynamicGroup + "-cluster"
	clusterSvc := &corev1.Service{}
	if err := client.Resources(dynamicE2ENamespace).Get(ctx, clusterServiceName, dynamicE2ENamespace, clusterSvc); err != nil {
		t.Fatalf("failed to get dynamic cluster Service: %v", err)
	}
	if clusterSvc.Spec.Selector["app.kubernetes.io/component"] != "dynamic-host" {
		t.Fatalf("expected dynamic cluster Service selector component label dynamic-host")
	}

	pods := &corev1.PodList{}
	if err := client.Resources(dynamicE2ENamespace).List(ctx, pods); err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}
	for _, pod := range pods.Items {
		if !strings.HasPrefix(pod.Name, dynamicE2EDynamicGroup+"-") {
			continue
		}
		if pod.Labels["app.kubernetes.io/component"] != "dynamic-host" {
			t.Fatalf("expected dynamic pod %s to have component label dynamic-host", pod.Name)
		}
	}
}

func assertNoDynamicDatadirPVC(ctx context.Context, t *testing.T, client klient.Client) {
	t.Helper()
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := client.Resources(dynamicE2ENamespace).List(ctx, pvcs); err != nil {
		t.Fatalf("failed to list PVCs: %v", err)
	}
	prefix := "datadir-" + dynamicE2EDynamicGroup + "-"
	for _, pvc := range pvcs.Items {
		if strings.HasPrefix(pvc.Name, prefix) {
			t.Fatalf("expected no dynamic datadir PVCs by default, found %s", pvc.Name)
		}
	}
}

func assertAllowDynamicHostsEnabled(t *testing.T) {
	t.Helper()
	cmd := fmt.Sprintf(
		"curl -sS --digest -u '%s:%s' 'http://localhost:8002/manage/v2/groups/%s/properties?format=json'",
		adminUsername,
		adminPassword,
		dynamicE2EDynamicGroup,
	)
	out, err := utils.ExecCmdInPod(dynamicBootstrapPodName(), dynamicE2ENamespace, dynamicE2EMLContainerName, cmd)
	if err != nil {
		t.Fatalf("failed to query dynamic group properties: %v", err)
	}
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(out, " ", ""), "\n", ""))
	if !strings.Contains(normalized, "\"allow-dynamic-hosts\":true") {
		t.Fatalf("allow-dynamic-hosts not enabled in group properties: %s", out)
	}
}

// fetchMLClusterName queries GET /manage/v2 on the bootstrap pod and returns the
// actual MarkLogic cluster name (the "name" field inside the version envelope).
// Example response: {"local-cluster-default":{"id":"...","name":"node-0.node.ml-dynamic-host.svc.cluster.local-cluster",...}}
// Falls back to the K8s resource name if the query or parse fails.
func fetchMLClusterName(t *testing.T) string {
	t.Helper()
	cmd := fmt.Sprintf(
		"curl -sS --digest -u '%s:%s' 'http://localhost:8002/manage/v2?format=json'",
		adminUsername,
		adminPassword,
	)
	out, err := utils.ExecCmdInPod(dynamicBootstrapPodName(), dynamicE2ENamespace, dynamicE2EMLContainerName, cmd)
	if err != nil {
		t.Logf("warning: failed to query ML cluster name, using K8s resource name %q: %v", dynamicE2EClusterName, err)
		return dynamicE2EClusterName
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Logf("warning: failed to parse /manage/v2 response, using K8s resource name %q: %v", dynamicE2EClusterName, err)
		return dynamicE2EClusterName
	}
	for key, raw := range envelope {
		if strings.EqualFold(key, "errorResponse") {
			continue
		}
		var inner struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &inner); err == nil && inner.Name != "" {
			t.Logf("resolved ML cluster name %q from /manage/v2", inner.Name)
			return inner.Name
		}
	}
	t.Logf("warning: no cluster name found in /manage/v2 response, using K8s resource name %q", dynamicE2EClusterName)
	return dynamicE2EClusterName
}

func removeDynamicHostByID(t *testing.T, hostID string) error {
	t.Helper()
	clusterName := fetchMLClusterName(t)
	payload := fmt.Sprintf("<dynamic-hosts><dynamic-host>%s</dynamic-host></dynamic-hosts>", hostID)
	cmd := fmt.Sprintf(
		"payload='%s'; status=$(curl -sS -o /tmp/dh-remove.out -w '%%{http_code}' --digest -u '%s:%s' -H 'Content-Type: application/xml' -X DELETE --data \"$payload\" 'http://localhost:8002/manage/v2/clusters/%s/dynamic-hosts'); echo \"HTTP_STATUS=$status\"; cat /tmp/dh-remove.out",
		payload,
		adminUsername,
		adminPassword,
		url.PathEscape(clusterName),
	)
	out, err := utils.ExecCmdInPod(dynamicBootstrapPodName(), dynamicE2ENamespace, dynamicE2EMLContainerName, cmd)
	if err != nil {
		return fmt.Errorf("remove dynamic host API call failed: %w", err)
	}
	if !strings.Contains(out, "HTTP_STATUS=204") && !strings.Contains(out, "HTTP_STATUS=202") && !strings.Contains(out, "HTTP_STATUS=200") {
		return fmt.Errorf("unexpected remove dynamic host response: %s", out)
	}
	return nil
}

func waitForGroupHostsContainment(t *testing.T, mustContain, mustNotContain []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := readDynamicGroupHostsRaw()
		if err == nil {
			allPresent := true
			for _, expected := range mustContain {
				if !strings.Contains(out, expected) {
					allPresent = false
					break
				}
			}
			allAbsent := true
			for _, unexpected := range mustNotContain {
				if strings.Contains(out, unexpected) {
					allAbsent = false
					break
				}
			}
			if allPresent && allAbsent {
				return
			}
			t.Logf("waiting for group host containment; mustContain=%v mustNotContain=%v", mustContain, mustNotContain)
		} else {
			t.Logf("waiting for group host containment; list hosts failed: %v", err)
		}
		time.Sleep(dynamicE2ERetryInterval)
	}
	out, _ := readDynamicGroupHostsRaw()
	dumpDynamicDiagnostics(t)
	t.Fatalf("timeout waiting for group host containment mustContain=%v mustNotContain=%v lastOutput=%s", mustContain, mustNotContain, out)
}

func readDynamicGroupHostsRaw() (string, error) {
	cmd := fmt.Sprintf(
		"curl -sS --digest -u '%s:%s' 'http://localhost:8002/manage/v2/hosts?group-id=%s&view=status&format=json'",
		adminUsername,
		adminPassword,
		dynamicE2EDynamicGroup,
	)
	return utils.ExecCmdInPod(dynamicBootstrapPodName(), dynamicE2ENamespace, dynamicE2EMLContainerName, cmd)
}

func waitForPodDeleted(ctx context.Context, client klient.Client, namespace, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod := &corev1.Pod{}
		err := client.Resources(namespace).Get(ctx, podName, namespace, pod)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			// Continue on transient list/get issues.
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}
		time.Sleep(dynamicE2ERetryInterval)
	}
	return fmt.Errorf("pod %s was not deleted within %s", podName, timeout)
}

func ensureDynamicPodRestarted(t *testing.T, ctx context.Context, client klient.Client, podName string) {
	t.Helper()
	pod := &corev1.Pod{}
	err := client.Resources(dynamicE2ENamespace).Get(ctx, podName, dynamicE2ENamespace, pod)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("warning: failed to get pod %s before restart: %v", podName, err)
	}
	if err == nil && len(pod.Finalizers) > 0 {
		pod.Finalizers = nil
		_ = client.Resources(dynamicE2ENamespace).Update(ctx, pod)
	}

	frameworkutils.RunCommand(fmt.Sprintf("kubectl delete pod %s -n %s --ignore-not-found --grace-period=0 --force", podName, dynamicE2ENamespace))
	_ = waitForPodDeleted(ctx, client, dynamicE2ENamespace, podName, 3*time.Minute)

	if err := utils.WaitForPod(ctx, t, client, dynamicE2ENamespace, podName, dynamicE2ERecoveryPodTimeout, true); err != nil {
		t.Fatalf("recovery pod %s not ready after forced restart: %v", podName, err)
	}
}

func ensureDynamicRecoveryPodReady(t *testing.T, ctx context.Context, client klient.Client, podName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod := &corev1.Pod{}
		err := client.Resources(dynamicE2ENamespace).Get(ctx, podName, dynamicE2ENamespace, pod)
		if apierrors.IsNotFound(err) {
			t.Logf("waiting for recovery pod %s to be recreated", podName)
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}
		if err != nil {
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}

		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed {
			t.Logf("recovery pod %s is stuck (phase=%s deleting=%t); forcing delete to unblock restart", podName, pod.Status.Phase, pod.DeletionTimestamp != nil)
			if len(pod.Finalizers) > 0 {
				pod.Finalizers = nil
				_ = client.Resources(dynamicE2ENamespace).Update(ctx, pod)
			}
			_ = client.Resources(dynamicE2ENamespace).Delete(ctx, pod)
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}

		if isPodReadyConditionTrue(pod) {
			return
		}

		t.Logf("waiting for recovery pod %s readiness (phase=%s)", podName, pod.Status.Phase)
		time.Sleep(dynamicE2ERetryInterval)
	}

	dumpDynamicDiagnostics(t)
	t.Fatalf("timeout waiting for recovery pod %s to become ready", podName)
}

func isPodReadyConditionTrue(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func findHostStatus(group *marklogicv1.MarklogicGroup, podName string) (marklogicv1.DynamicHostStatus, bool) {
	if group == nil || group.Status.Dynamic == nil {
		return marklogicv1.DynamicHostStatus{}, false
	}
	for _, host := range group.Status.Dynamic.Hosts {
		if host.PodName == podName {
			return host, true
		}
	}
	return marklogicv1.DynamicHostStatus{}, false
}

func dynamicBootstrapPodName() string {
	return dynamicE2EBootstrapGroup + "-0"
}

func dynamicPodName(ordinal int) string {
	return fmt.Sprintf("%s-%d", dynamicE2EDynamicGroup, ordinal)
}

func dynamicHostFQDN(podName string) string {
	return fmt.Sprintf("%s.%s.%s.svc.%s", podName, dynamicE2EDynamicGroup, dynamicE2ENamespace, dynamicE2EClusterDomain)
}

func dynamicGroupHostFQDNs(ordinals ...int) []string {
	hosts := make([]string, 0, len(ordinals))
	for _, ordinal := range ordinals {
		hosts = append(hosts, dynamicHostFQDN(dynamicPodName(ordinal)))
	}
	sort.Strings(hosts)
	return hosts
}

func ensureDynamicGroupResourcesMaterialized(ctx context.Context, t *testing.T, client klient.Client) error {
	t.Helper()

	deadline := time.Now().Add(4 * time.Minute)
	lastKick := time.Time{}

	for time.Now().Before(deadline) {
		bootstrapExists := marklogicGroupExists(ctx, client, dynamicE2ENamespace, dynamicE2EBootstrapGroup)
		dynamicExists := marklogicGroupExists(ctx, client, dynamicE2ENamespace, dynamicE2EDynamicGroup)
		if bootstrapExists && dynamicExists {
			return nil
		}

		t.Logf("waiting for child MarklogicGroups: bootstrap=%t dynamic=%t", bootstrapExists, dynamicExists)

		if lastKick.IsZero() || time.Since(lastKick) >= 20*time.Second {
			if err := kickDynamicClusterReconcile(ctx, client); err != nil {
				t.Logf("reconcile kick failed: %v", err)
			}
			lastKick = time.Now()
		}

		time.Sleep(dynamicE2ERetryInterval)
	}

	return fmt.Errorf("timed out waiting for child MarklogicGroups %q and %q", dynamicE2EBootstrapGroup, dynamicE2EDynamicGroup)
}

func marklogicGroupExists(ctx context.Context, client klient.Client, namespace, name string) bool {
	group := &marklogicv1.MarklogicGroup{}
	err := client.Resources(namespace).Get(ctx, name, namespace, group)
	return err == nil
}

func kickDynamicClusterReconcile(ctx context.Context, client klient.Client) error {
	cluster := &marklogicv1.MarklogicCluster{}
	if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EClusterName, dynamicE2ENamespace, cluster); err != nil {
		return fmt.Errorf("get MarklogicCluster for reconcile-kick: %w", err)
	}

	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations["e2e.marklogic.progress.com/reconcile-kick"] = time.Now().UTC().Format(time.RFC3339Nano)

	if err := client.Resources(dynamicE2ENamespace).Update(ctx, cluster); err != nil {
		return fmt.Errorf("update MarklogicCluster reconcile-kick annotation: %w", err)
	}
	return nil
}

func kickDynamicGroupReconcile(ctx context.Context, client klient.Client) error {
	group := &marklogicv1.MarklogicGroup{}
	if err := client.Resources(dynamicE2ENamespace).Get(ctx, dynamicE2EDynamicGroup, dynamicE2ENamespace, group); err != nil {
		return fmt.Errorf("get dynamic MarklogicGroup for reconcile-kick: %w", err)
	}

	if group.Annotations == nil {
		group.Annotations = map[string]string{}
	}
	group.Annotations["e2e.marklogic.progress.com/reconcile-kick"] = time.Now().UTC().Format(time.RFC3339Nano)

	if err := client.Resources(dynamicE2ENamespace).Update(ctx, group); err != nil {
		return fmt.Errorf("update MarklogicGroup reconcile-kick annotation: %w", err)
	}
	return nil
}

func assertDynamicEventPresent(ctx context.Context, t *testing.T, client klient.Client, reasons []string, timeout time.Duration) {
	t.Helper()
	if len(reasons) == 0 {
		return
	}

	reasonSet := map[string]bool{}
	for _, r := range reasons {
		reasonSet[r] = true
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := &corev1.EventList{}
		if err := client.Resources(dynamicE2ENamespace).List(ctx, events); err != nil {
			time.Sleep(dynamicE2ERetryInterval)
			continue
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Kind != "MarklogicGroup" || event.InvolvedObject.Name != dynamicE2EDynamicGroup {
				continue
			}
			if reasonSet[event.Reason] {
				return
			}
		}
		time.Sleep(dynamicE2ERetryInterval)
	}

	dumpDynamicDiagnostics(t)
	t.Fatalf("did not observe any expected dynamic events; expected one of reasons=%v", reasons)
}

func dumpDynamicDiagnostics(t *testing.T) {
	t.Helper()
	line := strings.Repeat("-", 78)
	t.Logf("\n%s\n  DYNAMIC HOST DIAGNOSTICS (%s)\n%s", line, dynamicE2ENamespace, line)
	for _, cmd := range []string{
		fmt.Sprintf("kubectl get pods -n %s -o wide", dynamicE2ENamespace),
		fmt.Sprintf("kubectl describe pods -n %s", dynamicE2ENamespace),
		fmt.Sprintf("kubectl get statefulset,svc,pvc -n %s -o wide", dynamicE2ENamespace),
		fmt.Sprintf("kubectl get marklogiccluster,marklogicgroup -n %s -o yaml", dynamicE2ENamespace),
		fmt.Sprintf("kubectl get events -n %s --sort-by=.lastTimestamp", dynamicE2ENamespace),
	} {
		p := frameworkutils.RunCommand(cmd)
		t.Logf("$ %s\n%s", cmd, p.Result())
	}
	p := frameworkutils.RunCommand(
		fmt.Sprintf("kubectl logs -n %s -l control-plane=controller-manager --tail=300 --prefix=true", namespace),
	)
	t.Logf("Operator logs (last 300 lines):\n%s", p.Result())
	t.Logf("%s\n", line)
}
