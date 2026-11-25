// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type reloadHarness struct {
	kube    client.WithWatch
	cc      *ClusterContext
	active  *corev1.ConfigMap
	desired map[string]string
}

func newReloadHarness(t *testing.T) *reloadHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		marklogicv1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	pathBased := false
	replicas := int32(1)
	cr := &marklogicv1.MarklogicCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "test", UID: "cluster-uid", Generation: 1},
		Spec: marklogicv1.MarklogicClusterSpec{
			ClusterDomain:   "cluster.local",
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{{Name: "ml", Replicas: &replicas}},
			HAProxy: &marklogicv1.HAProxy{
				Enabled: true, Image: "haproxy:test", ReplicaCount: 2,
				PathBasedRouting: &pathBased, Timeout: marklogicv1.Timeout{Client: 60, Connect: 5, Server: 60},
				AppServers: []marklogicv1.AppServers{{Name: "admin", Port: 8001}, {Name: "manage", Port: 8002}},
			},
		},
	}
	original := generateHAProxyConfigMapData(context.Background(), cr)
	cr.Spec.HAProxy.Timeout.Client = 90
	active := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "marklogic-haproxy", Namespace: cr.Namespace},
		Data:       original,
	}
	hash := haproxyreload.Hash([]byte(original["haproxy.cfg"]))
	candidate := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: haproxyCandidateConfigMap, Namespace: cr.Namespace},
		Data: map[string]string{
			"phase": "idle", "transaction": "bootstrap", "attempt": "0", "hash": hash,
			"haproxy.cfg": original["haproxy.cfg"], "previous.cfg": original["haproxy.cfg"],
		},
	}
	status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: haproxyReloadStatusMap, Namespace: cr.Namespace}}
	objects := []client.Object{cr, active, candidate, status}
	for _, name := range []string{"haproxy-1", "haproxy-2"} {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: getHAProxySelectorLabels(cr.Name)},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		})
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cr).WithObjects(objects...).Build()
	cc := &ClusterContext{
		Ctx: context.Background(), Client: kube, APIReader: kube, Scheme: scheme, MarklogicCluster: cr,
		ReqLogger: logr.Discard(), Recorder: record.NewFakeRecorder(20),
		HAProxyReloaderImage: "operator:test",
		Request:              &reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}},
	}
	return &reloadHarness{kube: kube, cc: cc, active: active, desired: generateHAProxyConfigMapData(cc.Ctx, cr)}
}

func (h *reloadHarness) getConfigMap(t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, cm); err != nil {
		t.Fatal(err)
	}
	return cm
}

func (h *reloadHarness) report(t *testing.T, phase, hash string) {
	t.Helper()
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	statusMap := h.getConfigMap(t, haproxyReloadStatusMap)
	if statusMap.Data == nil {
		statusMap.Data = map[string]string{}
	}
	for _, pod := range []string{"haproxy-1", "haproxy-2"} {
		status, err := json.Marshal(haproxyreload.PodStatus{
			Transaction: candidate.Data["transaction"], Phase: phase, Hash: hash, WorkerPID: 456,
		})
		if err != nil {
			t.Fatal(err)
		}
		statusMap.Data[pod] = string(status)
	}
	if err := h.cc.Client.Update(h.cc.Ctx, statusMap); err != nil {
		t.Fatal(err)
	}
}

func (h *reloadHarness) run(t *testing.T) (ctrl.Result, error) {
	active := h.getConfigMap(t, "marklogic-haproxy")
	step := h.cc.reconcileHAProxyReload(active, h.desired)
	if step.Completed() {
		return step.Output()
	}
	return ctrl.Result{}, nil
}

func TestHAProxyReloadUsesPodLocalPrepareCommitAndReportsSuccess(t *testing.T) {
	h := newReloadHarness(t)
	outcome, err := h.run(t)
	if outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("expected prepare phase to requeue: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "prepare" || candidate.Data["haproxy.cfg"] != h.desired["haproxy.cfg"] {
		t.Fatalf("candidate was not published for local validation: %#v", candidate.Data)
	}
	targetHash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.report(t, "prepared", targetHash)
	if outcome, err = h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("expected commit phase to requeue: result=%+v err=%v", outcome, err)
	}
	candidate = h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "commit" {
		t.Fatalf("all-replica validation did not lead to commit: %s", candidate.Data["phase"])
	}
	h.report(t, "applied", targetHash)
	if outcome, err = h.run(t); outcome.RequeueAfter != 0 || err != nil {
		t.Fatalf("successful transaction failed: result=%+v err=%v", outcome, err)
	}
	active := h.getConfigMap(t, "marklogic-haproxy")
	if !reflect.DeepEqual(active.Data, h.desired) {
		t.Fatal("committed ConfigMap does not match applied configuration")
	}
	if candidate := h.getConfigMap(t, haproxyCandidateConfigMap); candidate.Data["phase"] != "idle" || candidate.Data["hash"] != targetHash {
		t.Fatal("completed candidate was not reset to the committed configuration")
	}
	cluster := &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Status.HAProxyReload == nil || cluster.Status.HAProxyReload.Phase != "Success" {
		t.Fatalf("success status not recorded: %+v", cluster.Status.HAProxyReload)
	}
	foundSuccessEvent := false
	for len(h.cc.Recorder.(*record.FakeRecorder).Events) > 0 {
		if event := <-h.cc.Recorder.(*record.FakeRecorder).Events; strings.Contains(event, "Normal HAProxyReloadSuccess") {
			foundSuccessEvent = true
		}
	}
	if !foundSuccessEvent {
		t.Fatal("success event missing")
	}
}

func TestHAProxyReloadValidationFailureAbortsWithoutCommitting(t *testing.T) {
	h := newReloadHarness(t)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("start prepare: result=%+v err=%v", outcome, err)
	}
	targetHash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	statusMap := h.getConfigMap(t, haproxyReloadStatusMap)
	status, _ := json.Marshal(haproxyreload.PodStatus{
		Transaction: candidate.Data["transaction"], Phase: "failed", Hash: targetHash, Message: "invalid bind port",
	})
	statusMap.Data = map[string]string{"haproxy-1": string(status)}
	if err := h.cc.Client.Update(h.cc.Ctx, statusMap); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "invalid bind port") {
		t.Fatalf("validation failure was not surfaced: %v", err)
	}
	candidate = h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "idle" || candidate.Data["hash"] != haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"])) {
		t.Fatalf("invalid candidate was not reset to the last commit: %#v", candidate.Data)
	}
	active := h.getConfigMap(t, "marklogic-haproxy")
	if !reflect.DeepEqual(active.Data, h.active.Data) {
		t.Fatal("invalid configuration replaced the active ConfigMap")
	}
}

func TestHAProxyReloadActivationFailureRollsBackEveryReplica(t *testing.T) {
	h := newReloadHarness(t)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("start prepare: result=%+v err=%v", outcome, err)
	}
	targetHash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.report(t, "prepared", targetHash)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("advance to commit: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	statusMap := h.getConfigMap(t, haproxyReloadStatusMap)
	statusMap.Data = map[string]string{}
	for pod, phase := range map[string]string{"haproxy-1": "applied", "haproxy-2": "failed"} {
		report, _ := json.Marshal(haproxyreload.PodStatus{
			Transaction: candidate.Data["transaction"], Phase: phase, Hash: targetHash, Message: "bind failed",
		})
		statusMap.Data[pod] = string(report)
	}
	if err := h.cc.Client.Update(h.cc.Ctx, statusMap); err != nil {
		t.Fatal(err)
	}
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("expected rollback to be requested: result=%+v err=%v", outcome, err)
	}
	candidate = h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "rollback" {
		t.Fatalf("partial activation was not rolled back: %s", candidate.Data["phase"])
	}
	oldHash := haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"]))
	h.report(t, "rolledback", oldHash)
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "all replicas were restored") {
		t.Fatalf("rollback completion was not reported as failure: %v", err)
	}
	active := h.getConfigMap(t, "marklogic-haproxy")
	if !reflect.DeepEqual(active.Data, h.active.Data) {
		t.Fatal("failed activation committed the candidate")
	}
	cluster := &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Status.HAProxyReload == nil || cluster.Status.HAProxyReload.Phase != "Failed" {
		t.Fatalf("rollback failure status missing: %+v", cluster.Status.HAProxyReload)
	}
}

func TestHAProxyReloadFinalizesRollbackOfSupersededGeneration(t *testing.T) {
	h := newReloadHarness(t)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("start prepare: result=%+v err=%v", outcome, err)
	}
	targetHash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.report(t, "prepared", targetHash)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("advance to commit: result=%+v err=%v", outcome, err)
	}
	cluster := &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Generation = 2
	if err := h.cc.Client.Update(h.cc.Ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Generation != 2 {
		t.Skip("fake client does not preserve explicit generations")
	}
	h.cc.MarklogicCluster = cluster
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("superseded commit was not rolled back: result=%+v err=%v", outcome, err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "rollback" {
		t.Fatalf("superseded commit phase = %s, want rollback", phase)
	}
	oldHash := haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"]))
	h.report(t, "rolledback", oldHash)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("finalizing superseded rollback: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "idle" || candidate.Data["hash"] != oldHash {
		t.Fatalf("superseded rollback was not finalized: %#v", candidate.Data)
	}
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("latest generation was not prepared: result=%+v err=%v", outcome, err)
	}
	candidate = h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "prepare" || candidate.Data["generation"] != "2" {
		t.Fatalf("latest generation transaction = %#v", candidate.Data)
	}
}

func TestHAProxyReloadWaitsForPodLocalRecoveryOfRunningUnreadyPod(t *testing.T) {
	h := newReloadHarness(t)
	active := h.getConfigMap(t, "marklogic-haproxy")
	active.Annotations = map[string]string{haproxyBootstrapAnnotation: "pending"}
	if err := h.cc.Client.Update(h.cc.Ctx, active); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: "haproxy-2", Namespace: "test"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "haproxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	if err := h.cc.Client.Delete(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.ResourceVersion = ""
	if err := h.cc.Client.Create(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("running unready pod should wait for local recovery: result=%+v err=%v", outcome, err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "idle" {
		t.Fatalf("reload began while a replica was unready: %s", phase)
	}
}

func TestHAProxyBootstrapCorrectionUpdatesIdleCandidate(t *testing.T) {
	h := newReloadHarness(t)
	active := h.getConfigMap(t, "marklogic-haproxy")
	active.Annotations = map[string]string{haproxyBootstrapAnnotation: "pending"}
	if err := h.cc.Client.Update(h.cc.Ctx, active); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"haproxy-1", "haproxy-2"} {
		pod := &corev1.Pod{}
		if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, pod); err != nil {
			t.Fatal(err)
		}
		if err := h.cc.Client.Delete(h.cc.Ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.ResourceVersion = ""
		pod.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "haproxy", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "invalid port"}},
		}}}
		if err := h.cc.Client.Create(h.cc.Ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("bootstrap correction: result=%+v err=%v", outcome, err)
	}
	if active := h.getConfigMap(t, "marklogic-haproxy"); !reflect.DeepEqual(active.Data, h.desired) {
		t.Fatal("bootstrap correction did not update the startup ConfigMap")
	}
	// Idle sidecars converge on the candidate and must not reapply the invalid bootstrap.
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	targetHash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	if candidate.Data["phase"] != "idle" || candidate.Data["hash"] != targetHash ||
		candidate.Data["haproxy.cfg"] != h.desired["haproxy.cfg"] {
		t.Fatalf("idle candidate does not describe the bootstrap correction: %#v", candidate.Data)
	}
}

func TestHAProxyReloadWaitsUntilEveryPodIsReady(t *testing.T) {
	h := newReloadHarness(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "haproxy-2", Namespace: "test"}}
	if err := h.cc.Client.Delete(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	outcome, err := h.run(t)
	if outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("missing replica should return a retry: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "idle" {
		t.Fatal("reload began without all desired replicas")
	}
}

func TestHAProxyHotReloadDeploymentUsesSidecarAndPrivateStatusRBAC(t *testing.T) {
	h := newReloadHarness(t)
	meta := generateObjectMeta("marklogic-haproxy", "test", h.cc.GetHAProxyLabels("cluster"), nil)
	deployment := h.cc.createHAProxyDeploymentDef(meta)
	if err := h.cc.configureHAProxyHotReload(deployment); err != nil {
		t.Fatal(err)
	}
	spec := deployment.Spec.Template.Spec
	if spec.ServiceAccountName != haproxyReloaderAccount || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("HAProxy pod does not use a dedicated, non-automounted ServiceAccount")
	}
	if len(spec.Containers) != 2 || spec.Containers[1].Name != "haproxy-reloader" {
		t.Fatal("pod-local reloader sidecar is missing")
	}
	for _, mount := range spec.Containers[0].VolumeMounts {
		if mount.Name == "haproxy-reloader-api" {
			t.Fatal("Kubernetes API token is mounted into the HAProxy container")
		}
	}
	foundToken := false
	for _, mount := range spec.Containers[1].VolumeMounts {
		foundToken = foundToken || mount.Name == "haproxy-reloader-api"
	}
	if !foundToken {
		t.Fatal("projected API token is not mounted into the reloader sidecar")
	}
	// Candidates must be validated by the HAProxy binary, which the operator image lacks.
	if sidecar := spec.Containers[1]; sidecar.Image != spec.Containers[0].Image ||
		len(sidecar.Command) == 0 || sidecar.Command[0] != haproxyreload.Executable {
		t.Fatalf("reloader sidecar does not run the installed reloader in the HAProxy image: image=%s command=%v",
			sidecar.Image, sidecar.Command)
	}
	if spec.InitContainers[0].Image != "operator:test" {
		t.Fatalf("reloader installer image = %s", spec.InitContainers[0].Image)
	}
	if err := h.cc.ensureHAProxyReloaderResources(h.desired, marklogicClusterAsOwner(h.cc.MarklogicCluster)); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: haproxyReloaderAccount, Namespace: "test"}, role); err != nil {
		t.Fatal(err)
	}
	// Exactly two named permissions and nothing else: write the status entry, and read
	// (not write) the candidate. Every rule is limited to one ConfigMap by name.
	granted := map[string][]string{}
	for _, rule := range role.Rules {
		if len(rule.ResourceNames) != 1 || !reflect.DeepEqual(rule.Resources, []string{"configmaps"}) ||
			!reflect.DeepEqual(rule.APIGroups, []string{""}) {
			t.Fatalf("every reloader rule must name a single ConfigMap: %+v", rule)
		}
		granted[rule.ResourceNames[0]] = append(granted[rule.ResourceNames[0]], rule.Verbs...)
	}
	if !reflect.DeepEqual(granted, map[string][]string{
		haproxyReloadStatusMap: {"patch"}, haproxyCandidateConfigMap: {"get"},
	}) {
		t.Fatalf("reloader RBAC exceeds patching its status entry and reading the candidate: %+v", role.Rules)
	}
	if !reflect.DeepEqual(spec.InitContainers[0].Resources.Requests, spec.Containers[0].Resources.Requests) {
		t.Fatal("reloader init container must preserve the application's effective resource requests")
	}
}

func TestHAProxyConfigurationRenderingIsDeterministic(t *testing.T) {
	h := newReloadHarness(t)
	proxy := h.cc.MarklogicCluster.Spec.HAProxy
	proxy.AppServers[0].Path = "/admin"
	proxy.AppServers[1].Path = "/manage"
	proxy.TcpPorts = &marklogicv1.Tcpports{
		Enabled: true, Ports: []marklogicv1.TcpPort{{Name: "one", Port: 9001}, {Name: "two", Port: 9002}},
	}
	for _, pathBased := range []bool{false, true} {
		proxy.PathBasedRouting = &pathBased
		expected := generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
		for range 100 {
			if actual := generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster); !reflect.DeepEqual(actual, expected) {
				t.Fatal("map iteration order changes the configuration hash")
			}
		}
	}
}

func TestHAProxySidecarMigrationIsStableForConfigurationOnlyChanges(t *testing.T) {
	h := newReloadHarness(t)
	meta := generateObjectMeta("marklogic-haproxy", "test", h.cc.GetHAProxyLabels("cluster"), nil)
	before := h.cc.createHAProxyDeploymentDef(meta)
	if err := h.cc.configureHAProxyHotReload(before); err != nil {
		t.Fatal(err)
	}
	h.cc.MarklogicCluster.Spec.HAProxy.Timeout.Client++
	after := h.cc.createHAProxyDeploymentDef(meta)
	if err := h.cc.configureHAProxyHotReload(after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Spec, after.Spec) {
		t.Fatal("configuration-only change altered the pod template")
	}
	if len(after.Spec.Template.Spec.Containers) != 2 || after.Spec.Template.Spec.Containers[1].Command[1] != "watch" {
		t.Fatal("sidecar watcher is not configured")
	}
}

func TestHAProxyReconcileInstallsSidecarAndMigratesOnlyOnce(t *testing.T) {
	h := newReloadHarness(t)
	h.cc.MarklogicCluster.Spec.HAProxy.Timeout.Client = 60
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
	meta := generateObjectMeta("marklogic-haproxy", "test", h.cc.GetHAProxyLabels("cluster"), nil)
	legacy := h.cc.createHAProxyDeploymentDef(meta)
	legacy.Spec.Template.Annotations = map[string]string{"configmap-hash": "legacy-config"}
	if err := h.cc.Client.Create(h.cc.Ctx, legacy); err != nil {
		t.Fatal(err)
	}
	step := h.cc.ReconcileHAProxy()
	if !step.Completed() {
		t.Fatal("legacy deployment migration did not wait for rollout")
	}
	if _, err := step.Output(); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Name: "marklogic-haproxy", Namespace: "test"}
	if err := h.cc.Client.Get(h.cc.Ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	if _, exists := deployment.Spec.Template.Annotations["configmap-hash"]; exists {
		t.Fatal("migration retained the pod-restart configuration hash")
	}
	if len(deployment.Spec.Template.Spec.Containers) != 2 ||
		deployment.Spec.Template.Spec.Containers[1].Name != "haproxy-reloader" {
		t.Fatal("legacy deployment did not install the reloader sidecar")
	}
	h.reportPods(t, haproxyreload.Hash([]byte(h.desired["haproxy.cfg"])), map[string]string{"haproxy-1": "active", "haproxy-2": "active"})
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		outcome, err := step.Output()
		if err != nil || outcome.RequeueAfter > 0 {
			t.Fatalf("migrated deployment failed its first reconciliation: %+v %v", outcome, err)
		}
	}
	if err := h.cc.Client.Get(h.cc.Ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	resourceVersion := deployment.ResourceVersion
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		_, err := step.Output()
		t.Fatalf("steady-state reconcile failed: %v", err)
	}
	if err := h.cc.Client.Get(h.cc.Ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ResourceVersion != resourceVersion {
		t.Fatal("steady-state reconcile repeated the one-time pod migration")
	}
	sa := &corev1.ServiceAccount{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: haproxyReloaderAccount, Namespace: "test"}, sa); err != nil {
		t.Fatal(err)
	}
	roleBinding := &rbacv1.RoleBinding{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: haproxyReloaderAccount, Namespace: "test"}, roleBinding); err != nil {
		t.Fatal(err)
	}
}

func TestHAProxyTransactionRejectsUnexpectedPhase(t *testing.T) {
	h := newReloadHarness(t)
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	candidate.Data["phase"] = "unexpected"
	candidate.Data["transaction"] = "test-transaction"
	candidate.Data["generation"] = fmt.Sprint(h.cc.MarklogicCluster.Generation)
	candidate.Data["hash"] = haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	if err := h.cc.Client.Update(h.cc.Ctx, candidate); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "unknown HAProxy reload phase") {
		t.Fatalf("invalid transaction phase was not surfaced: %v", err)
	}
}

func TestHAProxyCommitRemovesLegacyLastAppliedConfiguration(t *testing.T) {
	h := newReloadHarness(t)
	active := h.getConfigMap(t, "marklogic-haproxy")
	active.Annotations = map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "legacy snapshot with sensitive configuration",
	}
	if err := h.cc.Client.Update(h.cc.Ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := h.cc.commitHAProxyConfigMap(active, h.desired); err != nil {
		t.Fatal(err)
	}
	active = h.getConfigMap(t, "marklogic-haproxy")
	if _, exists := active.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; exists {
		t.Fatal("legacy last-applied snapshot was retained")
	}
	if !reflect.DeepEqual(active.Data, h.desired) {
		t.Fatal("the committed configuration was not saved")
	}
}

func TestHAProxyCandidateTransactionIDIncludesGenerationAndHash(t *testing.T) {
	h := newReloadHarness(t)
	if outcome, err := h.run(t); outcome.RequeueAfter == 0 || err != nil {
		t.Fatalf("start transaction: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	expected := fmt.Sprintf("%d-%s-1", h.cc.MarklogicCluster.Generation, hash[:12])
	if candidate.Data["transaction"] != expected {
		t.Fatalf("transaction=%q, want %q", candidate.Data["transaction"], expected)
	}
}

func renderHotReloadPod(t *testing.T, h *reloadHarness, mutate func(*appsv1.Deployment)) corev1.PodSpec {
	t.Helper()
	meta := generateObjectMeta("marklogic-haproxy", "test", h.cc.GetHAProxyLabels("cluster"), nil)
	deployment := h.cc.createHAProxyDeploymentDef(meta)
	if mutate != nil {
		mutate(deployment)
	}
	if err := h.cc.configureHAProxyHotReload(deployment); err != nil {
		t.Fatal(err)
	}
	return deployment.Spec.Template.Spec
}

func TestHAProxySidecarAlwaysRunsAsNonRootUser(t *testing.T) {
	user := func(uid int64) func(*appsv1.Deployment) {
		return func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser = &uid }
	}
	for name, tc := range map[string]struct {
		mutate func(*appsv1.Deployment)
		want   int64
	}{
		"default spec leaves HAProxy as root":  {nil, haproxyReloaderDefaultUser},
		"explicit root is not inherited":       {user(0), haproxyReloaderDefaultUser},
		"explicit non-root user is preserved":  {user(2000), 2000},
		"pod-level non-root user is preserved": {func(d *appsv1.Deployment) { uid := int64(3000); d.Spec.Template.Spec.SecurityContext.RunAsUser = &uid }, 3000},
	} {
		t.Run(name, func(t *testing.T) {
			sidecar := renderHotReloadPod(t, newReloadHarness(t), tc.mutate).Containers[1]
			sc := sidecar.SecurityContext
			if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != tc.want {
				t.Fatalf("sidecar must run as non-root uid %d: %+v", tc.want, sc)
			}
		})
	}
}

func TestHAProxyInstallerUsesExplicitNonRootUIDAndDefaultsFSGroups(t *testing.T) {
	h := newReloadHarness(t)
	rootUID := int64(0)
	podContext := &corev1.PodSecurityContext{RunAsUser: &rootUID}
	h.cc.MarklogicCluster.Spec.HAProxy.PodSecurityContext = podContext
	spec := renderHotReloadPod(t, h, nil)

	if spec.SecurityContext.FSGroup == nil || *spec.SecurityContext.FSGroup != 101 {
		t.Fatalf("shared volumes need a default fsGroup: %+v", spec.SecurityContext)
	}
	if podContext.FSGroup != nil {
		t.Fatal("defaulting fsGroup must not mutate the cluster spec")
	}
	installer := spec.InitContainers[0].SecurityContext
	if installer.RunAsNonRoot == nil || !*installer.RunAsNonRoot ||
		installer.RunAsUser == nil || *installer.RunAsUser != haproxyReloaderInstallerUser {
		t.Fatalf("installer must use its explicit non-root UID: %+v", installer)
	}
}

func TestHAProxyReloaderExecutableIsReadOnlyForLongRunningContainers(t *testing.T) {
	spec := renderHotReloadPod(t, newReloadHarness(t), nil)
	if !strings.HasPrefix(haproxyreload.Executable, haproxyreload.BinDir+"/") ||
		strings.HasPrefix(haproxyreload.Executable, haproxyreload.RuntimeDir) {
		t.Fatalf("executable must not live in the shared writable runtime volume: %s", haproxyreload.Executable)
	}
	mountOf := func(c corev1.Container, volume string) *corev1.VolumeMount {
		for i := range c.VolumeMounts {
			if c.VolumeMounts[i].Name == volume {
				return &c.VolumeMounts[i]
			}
		}
		return nil
	}
	for _, c := range spec.Containers {
		if m := mountOf(c, "haproxy-reloader-bin"); m == nil || !m.ReadOnly || m.MountPath != haproxyreload.BinDir {
			t.Fatalf("%s must mount the reloader executable read-only: %+v", c.Name, m)
		}
	}
	init := spec.InitContainers[0]
	if m := mountOf(init, "haproxy-reloader-bin"); m == nil || m.ReadOnly {
		t.Fatalf("init container must be the only writer of the executable volume: %+v", m)
	}
	if mountOf(init, "haproxy-runtime") != nil {
		t.Fatal("init container must not receive the shared runtime volume")
	}
	if mountOf(spec.Containers[0], "haproxy-reloader-health") != nil || mountOf(spec.Containers[1], "haproxy-reloader-health") == nil {
		t.Fatal("heartbeat volume must be private to the sidecar")
	}
	probe := spec.Containers[1].LivenessProbe
	if probe == nil || probe.Exec == nil || !reflect.DeepEqual(probe.Exec.Command, []string{haproxyreload.Executable, "live"}) {
		t.Fatalf("sidecar liveness probe missing: %+v", probe)
	}
}

func (h *reloadHarness) age(t *testing.T, phaseAge time.Duration) {
	t.Helper()
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	candidate.Data[haproxyPhaseStartedAt] = time.Now().Add(-phaseAge).UTC().Format(time.RFC3339)
	if err := h.cc.Client.Update(h.cc.Ctx, candidate); err != nil {
		t.Fatal(err)
	}
}

func (h *reloadHarness) reportPods(t *testing.T, hash string, phases map[string]string) {
	t.Helper()
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	statusMap := h.getConfigMap(t, haproxyReloadStatusMap)
	statusMap.Data = map[string]string{}
	for pod, phase := range phases {
		report, _ := json.Marshal(haproxyreload.PodStatus{Transaction: candidate.Data["transaction"], Phase: phase, Hash: hash})
		statusMap.Data[pod] = string(report)
	}
	if err := h.cc.Client.Update(h.cc.Ctx, statusMap); err != nil {
		t.Fatal(err)
	}
}

func (h *reloadHarness) toCommit(t *testing.T) string {
	t.Helper()
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.report(t, "prepared", hash)
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "commit" {
		t.Fatalf("expected commit phase, got %s", phase)
	}
	return hash
}

func (h *reloadHarness) status(t *testing.T) *marklogicv1.HAProxyReloadStatus {
	t.Helper()
	cluster := &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cluster); err != nil {
		t.Fatal(err)
	}
	return cluster.Status.HAProxyReload
}

func TestHAProxyTransactionPhasesRecordTheirStartTime(t *testing.T) {
	h := newReloadHarness(t)
	h.toCommit(t)
	started, err := time.Parse(time.RFC3339, h.getConfigMap(t, haproxyCandidateConfigMap).Data[haproxyPhaseStartedAt])
	if err != nil || time.Since(started) > time.Minute {
		t.Fatalf("commit phase has no usable start time: %v", err)
	}
}

func TestHAProxyPrepareTimeoutAbortsAndNamesUnresponsivePods(t *testing.T) {
	h := newReloadHarness(t)
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.reportPods(t, hash, map[string]string{"haproxy-1": "prepared"})
	h.age(t, haproxyPhaseTimeout+time.Minute)
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "haproxy-2") || strings.Contains(err.Error(), "haproxy-1,") {
		t.Fatalf("timeout must name only the pod that never acknowledged: %v", err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "idle" || candidate.Data["hash"] != haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"])) {
		t.Fatalf("timed-out candidate was not abandoned: %#v", candidate.Data)
	}
	if status := h.status(t); status == nil || status.Phase != "Failed" || !strings.Contains(status.Message, "timed out") {
		t.Fatalf("timeout not reported in status: %+v", status)
	}
	if !reflect.DeepEqual(h.getConfigMap(t, "marklogic-haproxy").Data, h.active.Data) {
		t.Fatal("timed-out candidate replaced the active ConfigMap")
	}
}

func TestHAProxyCommitTimeoutRollsBackAndReportsTheCause(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "applied"})
	h.age(t, haproxyPhaseTimeout+time.Minute)
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("expected a rollback to be requested: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "rollback" || !strings.Contains(candidate.Data[haproxyFailureReason], "haproxy-2") {
		t.Fatalf("commit timeout did not start a rollback naming the pod: %#v", candidate.Data)
	}
	if started, err := time.Parse(time.RFC3339, candidate.Data[haproxyPhaseStartedAt]); err != nil || time.Since(started) > time.Minute {
		t.Fatal("rollback must get its own fresh deadline")
	}
	h.reportPods(t, haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"])), map[string]string{"haproxy-1": "rolledback", "haproxy-2": "rolledback"})
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "all replicas were restored") {
		t.Fatalf("final failure must carry the timeout cause: %v", err)
	}
	if !reflect.DeepEqual(h.getConfigMap(t, "marklogic-haproxy").Data, h.active.Data) {
		t.Fatal("timed-out activation committed the candidate")
	}
}

func TestHAProxyRollbackTimeoutKeepsTransactionOpenAndReportsFailure(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "failed", "haproxy-2": "applied"})
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	oldHash := haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"]))
	h.reportPods(t, oldHash, map[string]string{"haproxy-1": "rolledback"})
	h.age(t, haproxyPhaseTimeout+time.Minute)
	_, err := h.run(t)
	if err == nil || !strings.Contains(err.Error(), "restore the previous configuration: haproxy-2") {
		t.Fatalf("rollback timeout must name the pod still on the candidate: %v", err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "rollback" {
		t.Fatalf("transaction must stay open while a pod may still hold the candidate: %s", phase)
	}
}

func TestHAProxyOverdueTransactionContinuesWhenEveryPodAcknowledged(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "applied", "haproxy-2": "applied"})
	h.age(t, 3*haproxyPhaseTimeout)
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
		t.Fatalf("acknowledged transaction must complete, not time out: result=%+v err=%v", outcome, err)
	}
	if !reflect.DeepEqual(h.getConfigMap(t, "marklogic-haproxy").Data, h.desired) {
		t.Fatal("fully acknowledged configuration was not committed")
	}
}

func TestHAProxyTransactionWithoutStartTimeGetsOne(t *testing.T) {
	h := newReloadHarness(t)
	h.toCommit(t)
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	delete(candidate.Data, haproxyPhaseStartedAt)
	if err := h.cc.Client.Update(h.cc.Ctx, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, h.getConfigMap(t, haproxyCandidateConfigMap).Data[haproxyPhaseStartedAt]); err != nil {
		t.Fatalf("legacy transaction did not receive a deadline: %v", err)
	}
	if h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"] != "commit" {
		t.Fatal("stamping a start time must not change the phase")
	}
}

func TestHAProxyFailedRollbackIsSurfacedWhileSidecarRetries(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "failed", "haproxy-2": "applied"})
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	statusMap := h.getConfigMap(t, haproxyReloadStatusMap)
	report, _ := json.Marshal(haproxyreload.PodStatus{
		Transaction: candidate.Data["transaction"], Phase: "rollback-failed", Message: "socket closed",
		Hash: haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"])),
	})
	statusMap.Data = map[string]string{"haproxy-1": string(report)}
	if err := h.cc.Client.Update(h.cc.Ctx, statusMap); err != nil {
		t.Fatal(err)
	}
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("a failed rollback must keep polling, not give up: result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status == nil || status.Phase != "Failed" || !strings.Contains(status.Message, "socket closed") {
		t.Fatalf("failed rollback not visible in status: %+v", status)
	}
}

// steadyState makes the committed ConfigMap equal to the desired configuration.
func (h *reloadHarness) steadyState(t *testing.T) string {
	t.Helper()
	h.cc.MarklogicCluster.Spec.HAProxy.Timeout.Client = 60
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
	if !reflect.DeepEqual(h.desired, h.active.Data) {
		t.Fatal("test setup: committed configuration must equal the desired one")
	}
	return haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
}

func (h *reloadHarness) reportSteady(t *testing.T, reports map[string]haproxyreload.PodStatus) {
	t.Helper()
	statusMap := h.getConfigMap(t, haproxyReloadStatusMap)
	statusMap.Data = map[string]string{}
	for pod, report := range reports {
		raw, _ := json.Marshal(report)
		statusMap.Data[pod] = string(raw)
	}
	if err := h.cc.Client.Update(h.cc.Ctx, statusMap); err != nil {
		t.Fatal(err)
	}
}

func TestHAProxySuccessRequiresEveryReplicaToRunTheCommittedConfiguration(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("unreported replicas must keep the reconcile pending: result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status == nil || status.Phase != "Pending" ||
		!strings.Contains(status.Message, "haproxy-1 (no report)") || !strings.Contains(status.Message, "haproxy-2 (no report)") {
		t.Fatalf("pending status must name the replicas: %+v", status)
	}
	// One replica runs a stale configuration (for example after a container restart).
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Transaction: "bootstrap", Phase: "active", Hash: hash},
		"haproxy-2": {Transaction: "bootstrap", Phase: "active", Hash: strings.Repeat("0", 64)},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("a replica on a stale configuration must keep the reconcile pending: result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status == nil || status.Phase != "Pending" || strings.Contains(status.Message, "haproxy-1") ||
		!strings.Contains(status.Message, "haproxy-2") {
		t.Fatalf("only the stale replica should be named: %+v", status)
	}
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Transaction: "bootstrap", Phase: "active", Hash: hash},
		"haproxy-2": {Transaction: "bootstrap", Phase: "active", Hash: hash},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
		t.Fatalf("converged replicas must complete the reconcile: result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status == nil || status.Phase != "Success" {
		t.Fatalf("success not recorded once every replica reported: %+v", status)
	}
}

func TestHAProxyAcceptsActivationAndRollbackAcknowledgementsAsConvergence(t *testing.T) {
	for _, phase := range []string{"applied", "rolledback"} {
		t.Run(phase, func(t *testing.T) {
			h := newReloadHarness(t)
			hash := h.steadyState(t)
			h.reportSteady(t, map[string]haproxyreload.PodStatus{
				"haproxy-1": {Phase: phase, Hash: hash}, "haproxy-2": {Phase: phase, Hash: hash},
			})
			if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
				t.Fatalf("result=%+v err=%v", outcome, err)
			}
		})
	}
	// A validation-only acknowledgement says nothing about what is running.
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "prepared", Hash: hash}, "haproxy-2": {Phase: "active", Hash: hash},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("a prepared replica is not running the configuration: result=%+v err=%v", outcome, err)
	}
}

func TestHAProxyDivergedReplicaErrorIsSurfaced(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "active", Hash: hash},
		"haproxy-2": {Phase: "diverged", Hash: strings.Repeat("0", 64), Message: "cannot bind socket"},
	})
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if status := h.status(t); status == nil || !strings.Contains(status.Message, "haproxy-2 (cannot bind socket)") {
		t.Fatalf("the replica's apply error must reach the status: %+v", status)
	}
}

func TestHAProxyUnconvergedReplicasEscalateToFailedAndStayFailed(t *testing.T) {
	h := newReloadHarness(t)
	h.steadyState(t)
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("result=%+v err=%v", outcome, err)
	}
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Annotations[haproxyConvergenceAnnotation] == "" {
		t.Fatal("the convergence wait must be timestamped on the first pending reconcile")
	}
	candidate.Annotations[haproxyConvergenceAnnotation] = hash + "|" + time.Now().Add(-haproxyPhaseTimeout-time.Minute).UTC().Format(time.RFC3339)
	if err := h.cc.Client.Update(h.cc.Ctx, candidate); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, err := h.run(t)
		if err == nil || !strings.Contains(err.Error(), "not converged") || !strings.Contains(err.Error(), "haproxy-1") {
			t.Fatalf("replicas that never converge must fail naming them (attempt %d): %v", i, err)
		}
		if status := h.status(t); status == nil || status.Phase != "Failed" {
			t.Fatalf("status must stay Failed instead of flapping back to Pending (attempt %d): %+v", i, status)
		}
	}
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "active", Hash: haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))},
		"haproxy-2": {Phase: "active", Hash: haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
		t.Fatalf("the failure must clear once replicas converge: result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status == nil || status.Phase != "Success" {
		t.Fatalf("recovery not reported: %+v", status)
	}
	if _, present := h.getConfigMap(t, haproxyCandidateConfigMap).Annotations[haproxyConvergenceAnnotation]; present {
		t.Fatal("the convergence clock must be cleared once replicas converge")
	}
}

func TestHAProxyConvergenceClockIgnoresChangingDiagnostics(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	diverged := func(message string) map[string]haproxyreload.PodStatus {
		return map[string]haproxyreload.PodStatus{
			"haproxy-1": {Phase: "active", Hash: hash},
			"haproxy-2": {Phase: "diverged", Hash: strings.Repeat("0", 64), Message: message},
		}
	}
	h.reportSteady(t, diverged("first error"))
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	first := h.getConfigMap(t, haproxyCandidateConfigMap).Annotations[haproxyConvergenceAnnotation]
	h.reportSteady(t, diverged("a different error on the next retry"))
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if again := h.getConfigMap(t, haproxyCandidateConfigMap).Annotations[haproxyConvergenceAnnotation]; again != first || first == "" {
		t.Fatalf("changing replica diagnostics must not restart the escalation clock: %q -> %q", first, again)
	}
}

func TestHAProxyConvergenceClockIsPerConfiguration(t *testing.T) {
	h := newReloadHarness(t)
	h.steadyState(t)
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	candidate.Annotations = map[string]string{
		haproxyConvergenceAnnotation: "an-older-configuration|" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	if err := h.cc.Client.Update(h.cc.Ctx, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t); err != nil {
		t.Fatalf("a wait recorded for another configuration must not trigger a failure: %v", err)
	}
	if got := h.getConfigMap(t, haproxyCandidateConfigMap).Annotations[haproxyConvergenceAnnotation]; !strings.HasPrefix(got, haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))+"|") {
		t.Fatalf("clock was not restarted for the current configuration: %q", got)
	}
}

func TestHAProxyBootstrapMarkerIsKeptUntilReplicasConverge(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	active := h.getConfigMap(t, "marklogic-haproxy")
	active.Annotations = map[string]string{haproxyBootstrapAnnotation: "pending"}
	if err := h.cc.Client.Update(h.cc.Ctx, active); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if h.getConfigMap(t, "marklogic-haproxy").Annotations[haproxyBootstrapAnnotation] != "pending" {
		t.Fatal("bootstrap marker was cleared before the replicas converged")
	}
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "active", Hash: hash}, "haproxy-2": {Phase: "active", Hash: hash},
	})
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if _, present := h.getConfigMap(t, "marklogic-haproxy").Annotations[haproxyBootstrapAnnotation]; present {
		t.Fatal("bootstrap marker was not cleared after convergence")
	}
}

func resetDiscoveredReloaderImage(t *testing.T) {
	t.Helper()
	discoveredReloaderImage.Lock()
	discoveredReloaderImage.image = ""
	discoveredReloaderImage.Unlock()
	t.Cleanup(func() {
		discoveredReloaderImage.Lock()
		discoveredReloaderImage.image = ""
		discoveredReloaderImage.Unlock()
	})
}

func operatorPod(image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-0", Namespace: "operator-ns"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar", Image: "other:1"}, {Name: "manager", Image: image}}},
	}
}

func TestHAProxyReloaderImageFromEnvironmentNeedsNoAPILookup(t *testing.T) {
	resetDiscoveredReloaderImage(t)
	h := newReloadHarness(t)
	h.cc.HAProxyReloaderImage = ""
	t.Setenv("HAPROXY_RELOADER_IMAGE", "  registry.example/operator:2.0  ")
	t.Setenv("POD_NAME", "operator-0")
	t.Setenv("POD_NAMESPACE", "operator-ns")
	// No operator Pod exists: reaching the API would fail.
	image, err := h.cc.haproxyReloaderImage()
	if err != nil || image != "registry.example/operator:2.0" {
		t.Fatalf("environment image not used: %q %v", image, err)
	}
	spec := renderHotReloadPod(t, h, nil)
	if spec.InitContainers[0].Image != "registry.example/operator:2.0" {
		t.Fatalf("init container must use the configured image: %s", spec.InitContainers[0].Image)
	}
}

func TestHAProxyReloaderImageLookupHappensOnce(t *testing.T) {
	resetDiscoveredReloaderImage(t)
	h := newReloadHarness(t)
	h.cc.HAProxyReloaderImage = ""
	t.Setenv("HAPROXY_RELOADER_IMAGE", "")
	t.Setenv("POD_NAME", "operator-0")
	t.Setenv("POD_NAMESPACE", "operator-ns")
	pod := operatorPod("operator:1.3.1")
	if err := h.cc.Client.Create(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	if image, err := h.cc.haproxyReloaderImage(); err != nil || image != "operator:1.3.1" {
		t.Fatalf("manager container image not discovered: %q %v", image, err)
	}
	// The operator image cannot change while running, so later reconciles must not
	// depend on the API: removing the Pod proves the cached value is used.
	if err := h.cc.Client.Delete(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	if image, err := h.cc.haproxyReloaderImage(); err != nil || image != "operator:1.3.1" {
		t.Fatalf("discovered image was not reused: %q %v", image, err)
	}
}

func TestHAProxyReloaderImageErrorsExplainTheRemedy(t *testing.T) {
	for name, tc := range map[string]struct {
		name, namespace string
		pod             *corev1.Pod
		want            string
	}{
		"no pod identity":         {"", "", nil, "POD_NAME and POD_NAMESPACE"},
		"operator pod unreadable": {"operator-0", "operator-ns", nil, "operator-ns/operator-0"},
		"no manager container":    {"operator-0", "operator-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "operator-0", Namespace: "operator-ns"}}, "no manager container image"},
	} {
		t.Run(name, func(t *testing.T) {
			resetDiscoveredReloaderImage(t)
			h := newReloadHarness(t)
			h.cc.HAProxyReloaderImage = ""
			t.Setenv("HAPROXY_RELOADER_IMAGE", "   ")
			t.Setenv("POD_NAME", tc.name)
			t.Setenv("POD_NAMESPACE", tc.namespace)
			if tc.pod != nil {
				if err := h.cc.Client.Create(h.cc.Ctx, tc.pod); err != nil {
					t.Fatal(err)
				}
			}
			_, err := h.cc.haproxyReloaderImage()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "HAPROXY_RELOADER_IMAGE") ||
				!strings.Contains(err.Error(), "haproxyReloaderImage") {
				t.Fatalf("error must name the cause and the Helm remedy: %v", err)
			}
			if h.cc.configureHAProxyHotReload(h.cc.createHAProxyDeploymentDef(generateObjectMeta("marklogic-haproxy", "test", nil, nil))) == nil {
				t.Fatal("an unresolvable image must stop the HAProxy deployment instead of rendering a broken pod")
			}
		})
	}
}

func TestHAProxyReloadReadsCandidateAndAcknowledgementsUncached(t *testing.T) {
	h := newReloadHarness(t)
	// Reads through the cached client are stale; the controller must not use them.
	h.cc.Client = interceptor.NewClient(h.kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap && key.Name != "marklogic-haproxy" {
				return errors.New("served from a stale cache")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("candidate and acknowledgements must be read uncached: result=%+v err=%v", outcome, err)
	}
}

func TestHAProxyConflictingUpdateIsRetriedQuietly(t *testing.T) {
	h := newReloadHarness(t)
	var conflicts atomic.Int32
	h.cc.Client = interceptor.NewClient(h.kube, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == haproxyCandidateConfigMap && conflicts.Add(1) == 1 {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("object was modified"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	outcome, err := h.run(t)
	if err != nil || outcome.RequeueAfter != time.Second {
		t.Fatalf("a conflict must be a quick quiet retry, not an error: result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status != nil && status.Phase == "Failed" {
		t.Fatalf("a conflict must not be reported as a failed reload: %+v", status)
	}
	for len(h.cc.Recorder.(*record.FakeRecorder).Events) > 0 {
		if event := <-h.cc.Recorder.(*record.FakeRecorder).Events; strings.Contains(event, "Warning") {
			t.Fatalf("a conflict must not raise a warning event: %s", event)
		}
	}
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("the retry must proceed: result=%+v err=%v", outcome, err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "prepare" {
		t.Fatalf("retry did not publish the candidate: %s", phase)
	}
}

func TestHAProxyCommitConflictDoesNotRollBackActivatedReplicas(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "applied", "haproxy-2": "applied"})
	var conflicts atomic.Int32
	h.cc.Client = interceptor.NewClient(h.kube, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == "marklogic-haproxy" && conflicts.Add(1) == 1 {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("object was modified"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
		t.Fatalf("a conflicting commit must be retried, not turned into a rollback: result=%+v err=%v", outcome, err)
	}
	if !reflect.DeepEqual(h.getConfigMap(t, "marklogic-haproxy").Data, h.desired) {
		t.Fatal("the activated configuration was not committed after the conflict")
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "idle" {
		t.Fatalf("replicas that already run the new configuration were rolled back: %s", phase)
	}
	if status := h.status(t); status == nil || status.Phase != "Success" {
		t.Fatalf("success not recorded: %+v", status)
	}
}

// failActivation drives a transaction to a completed rollback at the current generation.
func (h *reloadHarness) failActivation(t *testing.T) {
	t.Helper()
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "failed", "haproxy-2": "applied"})
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	h.reportPods(t, haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"])), map[string]string{"haproxy-1": "rolledback", "haproxy-2": "rolledback"})
	if _, err := h.run(t); err == nil || !strings.Contains(err.Error(), "all replicas were restored") {
		t.Fatalf("activation failure not reported: %v", err)
	}
}

func TestHAProxyRejectedConfigurationIsNotReappliedUntilTheSpecChanges(t *testing.T) {
	h := newReloadHarness(t)
	h.failActivation(t)
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	attempt := candidate.Data["attempt"]
	if candidate.Data["phase"] != "idle" || candidate.Annotations[haproxyRejectedAnnotation] == "" {
		t.Fatalf("the rejected configuration was not recorded: %#v %v", candidate.Data, candidate.Annotations)
	}
	for i := 0; i < 3; i++ {
		outcome, err := h.run(t)
		if err != nil || outcome.RequeueAfter != 0 || outcome.Requeue {
			t.Fatalf("a rejected configuration must not be retried or hot-loop (run %d): result=%+v err=%v", i, outcome, err)
		}
		after := h.getConfigMap(t, haproxyCandidateConfigMap)
		if after.Data["phase"] != "idle" || after.Data["attempt"] != attempt {
			t.Fatalf("the rejected configuration was published again (run %d): %#v", i, after.Data)
		}
	}
	if status := h.status(t); status == nil || status.Phase != "Failed" ||
		!strings.Contains(status.Message, "all replicas were restored") || !strings.Contains(status.Message, "not retried until the spec changes") {
		t.Fatalf("the rejection and how to retry must be visible: %+v", status)
	}
	if !reflect.DeepEqual(h.getConfigMap(t, "marklogic-haproxy").Data, h.active.Data) {
		t.Fatal("the rejected configuration reached the active ConfigMap")
	}

	// A spec change (new generation) allows the same configuration to be tried again.
	cluster := &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Generation = 2
	if err := h.cc.Client.Update(h.cc.Ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.cc.MarklogicCluster.Generation = 2
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("a new generation must retry: result=%+v err=%v", outcome, err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "prepare" {
		t.Fatalf("retry was not started after the spec changed: %s", phase)
	}
}

func TestHAProxyRejectionDoesNotBlockADifferentConfigurationOrAManualRetry(t *testing.T) {
	h := newReloadHarness(t)
	h.failActivation(t)
	// Deleting the marker (what removing the candidate ConfigMap achieves) retries at once.
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	rejected := candidate.Annotations[haproxyRejectedAnnotation]
	delete(candidate.Annotations, haproxyRejectedAnnotation)
	if err := h.cc.Client.Update(h.cc.Ctx, candidate); err != nil {
		t.Fatal(err)
	}
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("manual retry must publish the candidate: result=%+v err=%v", outcome, err)
	}
	// A different configuration is never blocked by the earlier rejection.
	h2 := newReloadHarness(t)
	h2.failActivation(t)
	h2.cc.MarklogicCluster.Spec.HAProxy.Timeout.Client = 120
	h2.desired = generateHAProxyConfigMapData(h2.cc.Ctx, h2.cc.MarklogicCluster)
	if outcome, err := h2.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("a different configuration must be tried: result=%+v err=%v (marker %q)", outcome, err, rejected)
	}
}

func TestHAProxyValidationFailureIsStillRetried(t *testing.T) {
	// Validation never touches live traffic, and may succeed later (for example once a
	// certificate is mounted), so only failed activations are remembered.
	h := newReloadHarness(t)
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.reportPods(t, hash, map[string]string{"haproxy-1": "failed", "haproxy-2": "prepared"})
	if _, err := h.run(t); err == nil {
		t.Fatal("validation failure not reported")
	}
	if annotation := h.getConfigMap(t, haproxyCandidateConfigMap).Annotations[haproxyRejectedAnnotation]; annotation != "" {
		t.Fatalf("a validation failure must not be recorded as a rejection: %q", annotation)
	}
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("validation must be attempted again: result=%+v err=%v", outcome, err)
	}
}

func TestHAProxyStatusEntriesOfDeletedPodsArePruned(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1":        {Phase: "active", Hash: hash},
		"haproxy-2":        {Phase: "active", Hash: hash},
		"haproxy-old-pod1": {Phase: "applied", Hash: strings.Repeat("0", 64)},
		"haproxy-old-pod2": {Phase: "active", Hash: strings.Repeat("0", 64)},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
		t.Fatalf("result=%+v err=%v", outcome, err)
	}
	remaining := h.getConfigMap(t, haproxyReloadStatusMap).Data
	if len(remaining) != 2 || remaining["haproxy-1"] == "" || remaining["haproxy-2"] == "" {
		t.Fatalf("only the entries of deleted pods may be removed: %v", remaining)
	}
}

func TestHAProxyStatusPruningLeavesLiveEntriesAndConcurrentWritersAlone(t *testing.T) {
	h := newReloadHarness(t)
	hash := h.steadyState(t)
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "active", Hash: hash}, "haproxy-2": {Phase: "active", Hash: hash},
		"haproxy-gone": {Phase: "active", Hash: hash},
	})
	// A sidecar patches its own entry between the controller's read and its prune.
	h.cc.Client = interceptor.NewClient(h.kube, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			current := h.getConfigMap(t, haproxyReloadStatusMap)
			current.Data["haproxy-2"] = `{"phase":"active","hash":"` + hash + `","transaction":"newer"}`
			if err := c.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	if _, err := h.run(t); err != nil {
		t.Fatalf("a concurrent sidecar write must not break pruning: %v", err)
	}
	remaining := h.getConfigMap(t, haproxyReloadStatusMap).Data
	if _, present := remaining["haproxy-gone"]; present || !strings.Contains(remaining["haproxy-2"], "newer") {
		t.Fatalf("pruning must remove only stale keys and keep concurrent writes: %v", remaining)
	}
}

func TestHAProxyTimedOutActivationIsRetriedNotRejected(t *testing.T) {
	// A replica that did not answer in time is a transient cause and may recover, so
	// unlike a failed activation it must not require a manual reset.
	h := newReloadHarness(t)
	hash := h.toCommit(t)
	h.reportPods(t, hash, map[string]string{"haproxy-1": "applied"})
	h.age(t, haproxyPhaseTimeout+time.Minute)
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	h.reportPods(t, haproxyreload.Hash([]byte(h.active.Data["haproxy.cfg"])), map[string]string{"haproxy-1": "rolledback", "haproxy-2": "rolledback"})
	if _, err := h.run(t); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout not reported: %v", err)
	}
	if annotation := h.getConfigMap(t, haproxyCandidateConfigMap).Annotations[haproxyRejectedAnnotation]; annotation != "" {
		t.Fatalf("a timeout must not be recorded as a rejection: %q", annotation)
	}
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("the configuration must be attempted again once replicas respond: result=%+v err=%v", outcome, err)
	}
}

func TestHAProxyRepairedSpecRetiresTheObsoleteTransaction(t *testing.T) {
	// Found on a live cluster: after a failed validation the invalid candidate stays in
	// "prepare". Reverting the spec to the committed configuration then left the pods
	// reporting the old failure forever, and the convergence wait escalated to Failed.
	h := newReloadHarness(t)
	if _, err := h.run(t); err != nil { // publishes the candidate for the unwanted configuration
		t.Fatal(err)
	}
	badHash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.reportPods(t, badHash, map[string]string{"haproxy-1": "failed", "haproxy-2": "failed"})
	if _, err := h.run(t); err == nil {
		t.Fatal("validation failure not reported")
	}
	if _, err := h.run(t); err != nil { // the retry publishes another prepare
		t.Fatal(err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "prepare" {
		t.Fatalf("test setup: expected a stale prepare, got %s", phase)
	}

	hash := h.steadyState(t) // the user repairs the spec: desired == committed again
	outcome, err := h.run(t)
	if err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("the obsolete transaction must be retired before waiting: result=%+v err=%v", outcome, err)
	}
	candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
	if candidate.Data["phase"] != "idle" || candidate.Data["hash"] != hash {
		t.Fatalf("candidate was not returned to the committed configuration: %#v", candidate.Data)
	}
	// The replicas now resume steady-state reporting, which completes the reconcile.
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "active", Hash: hash}, "haproxy-2": {Phase: "active", Hash: hash},
	})
	if outcome, err := h.run(t); err != nil || outcome.RequeueAfter != 0 {
		t.Fatalf("result=%+v err=%v", outcome, err)
	}
	if status := h.status(t); status == nil || status.Phase != "Success" {
		t.Fatalf("recovery not reported: %+v", status)
	}
}

func TestHAProxyReloaderRoleFromAnOlderOperatorIsUpdatedInPlace(t *testing.T) {
	// An upgrade must give existing sidecars the new read permission without any
	// manual step; until then they keep working from the mounted copy.
	h := newReloadHarness(t)
	old := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: haproxyReloaderAccount, Namespace: "test"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{haproxyReloadStatusMap},
			Verbs: []string{"patch"},
		}},
	}
	if err := h.cc.Client.Create(h.cc.Ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := h.cc.ensureHAProxyReloaderResources(h.desired, marklogicClusterAsOwner(h.cc.MarklogicCluster)); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(old), role); err != nil {
		t.Fatal(err)
	}
	granted := map[string][]string{}
	for _, rule := range role.Rules {
		granted[rule.ResourceNames[0]] = rule.Verbs
	}
	if !reflect.DeepEqual(granted, map[string][]string{haproxyReloadStatusMap: {"patch"}, haproxyCandidateConfigMap: {"get"}}) {
		t.Fatalf("the existing Role was not brought up to date: %+v", role.Rules)
	}
}
