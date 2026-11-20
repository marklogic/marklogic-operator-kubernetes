// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type reloadHarness struct {
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
	return &reloadHarness{cc: cc, active: active, desired: generateHAProxyConfigMapData(cc.Ctx, cr)}
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
	if len(role.Rules) != 1 || !reflect.DeepEqual(role.Rules[0].ResourceNames, []string{haproxyReloadStatusMap}) ||
		!reflect.DeepEqual(role.Rules[0].Verbs, []string{"patch"}) {
		t.Fatalf("reloader RBAC exceeds the named status ConfigMap patch: %+v", role.Rules)
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
