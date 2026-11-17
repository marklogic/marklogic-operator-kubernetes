// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type reloadHarness struct {
	cc       *ClusterContext
	cm       *corev1.ConfigMap
	desired  map[string]string
	states   map[string]string
	calls    []string
	failure  string
	recorder *record.FakeRecorder
}

func newReloadHarness(t *testing.T) *reloadHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
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
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "marklogic-haproxy", Namespace: cr.Namespace}, Data: original}
	objects := []client.Object{cr, cm}
	for _, name := range []string{"haproxy-1", "haproxy-2"} {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: getHAProxySelectorLabels(cr.Name)},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		})
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cr).WithObjects(objects...).Build()
	recorder := record.NewFakeRecorder(20)
	cc := &ClusterContext{
		Ctx: context.Background(), Client: kube, Scheme: scheme, MarklogicCluster: cr, ReqLogger: logr.Discard(),
		Recorder: recorder, Request: &reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}},
		HAProxyReloaderImage: "operator:test",
	}
	h := &reloadHarness{
		cc: cc, cm: cm, desired: generateHAProxyConfigMapData(cc.Ctx, cr), recorder: recorder,
		states: map[string]string{
			"haproxy-1": haproxyreload.Hash([]byte(original["haproxy.cfg"])),
			"haproxy-2": haproxyreload.Hash([]byte(original["haproxy.cfg"])),
		},
	}
	cc.HAProxyExec = func(ctx context.Context, pod *corev1.Pod, op string, data []byte) ([]byte, error) {
		h.calls = append(h.calls, op+":"+pod.Name)
		if op == "status" {
			return json.Marshal(haproxyreload.Status{Hash: h.states[pod.Name], WorkerPID: 123})
		}
		if op == "apply" {
			h.states[pod.Name] = haproxyreload.Hash(data)
		}
		if h.failure == op+":"+pod.Name && string(data) == h.desired["haproxy.cfg"] {
			return nil, errors.New("injected " + op + " failure")
		}
		return nil, nil
	}
	return h
}

func (h *reloadHarness) saved(t *testing.T) (*corev1.ConfigMap, *marklogicv1.MarklogicCluster) {
	t.Helper()
	cm, cr := &corev1.ConfigMap{}, &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cm), cm); err != nil {
		t.Fatal(err)
	}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cr); err != nil {
		t.Fatal(err)
	}
	return cm, cr
}

func TestHAProxyReloadValidatesAllReplicasAndDeduplicates(t *testing.T) {
	h := newReloadHarness(t)
	step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
	if step.Completed() {
		_, err := step.Output()
		t.Fatalf("reload failed: %v", err)
	}
	expected := []string{"status:haproxy-1", "status:haproxy-2", "validate:haproxy-1", "validate:haproxy-2", "apply:haproxy-1", "apply:haproxy-2"}
	if !reflect.DeepEqual(h.calls, expected) {
		t.Fatalf("validation/activation order: %v", h.calls)
	}
	cm, cr := h.saved(t)
	if !reflect.DeepEqual(cm.Data, h.desired) || cr.Status.HAProxyReload.Phase != "Success" ||
		cr.Status.HAProxyReload.LastAttemptTime.IsZero() {
		t.Fatalf("success was not committed/reported: cm=%v status=%+v", cm.Data, cr.Status.HAProxyReload)
	}
	timestamp := cr.Status.HAProxyReload.LastAttemptTime
	event := <-h.recorder.Events
	if !strings.Contains(event, "Normal HAProxyReloadSuccess") {
		t.Fatalf("missing success event: %s", event)
	}
	h.calls = nil
	if step := h.cc.reconcileHAProxyReload(cm, h.desired); step.Completed() {
		t.Fatal("unchanged configuration failed")
	}
	if !reflect.DeepEqual(h.calls, []string{"status:haproxy-1", "status:haproxy-2"}) {
		t.Fatalf("unchanged hash caused a reload: %v", h.calls)
	}
	_, cr = h.saved(t)
	if !cr.Status.HAProxyReload.LastAttemptTime.Equal(&timestamp) || len(h.recorder.Events) != 0 {
		t.Fatal("unchanged configuration refreshed status or emitted redundant events")
	}
}

func TestHAProxyReloadFailureRollsBackEveryChangedReplica(t *testing.T) {
	for _, failure := range []string{"validate:haproxy-2", "apply:haproxy-1", "apply:haproxy-2"} {
		t.Run(failure, func(t *testing.T) {
			h := newReloadHarness(t)
			h.failure = failure
			step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
			if !step.Completed() {
				t.Fatal("failure was not returned")
			}
			if _, err := step.Output(); err == nil {
				t.Fatal("failure was hidden")
			}
			cm, cr := h.saved(t)
			if !reflect.DeepEqual(cm.Data, h.cm.Data) || cr.Status.HAProxyReload.Phase != "Failed" {
				t.Fatalf("failed transaction was committed: %+v", cr.Status.HAProxyReload)
			}
			for pod, hash := range h.states {
				if hash != haproxyreload.Hash([]byte(h.cm.Data["haproxy.cfg"])) {
					t.Errorf("pod %s left on partial configuration", pod)
				}
			}
			if strings.HasPrefix(failure, "validate") {
				for _, call := range h.calls {
					if strings.HasPrefix(call, "apply") {
						t.Error("validation failure changed a running configuration")
					}
				}
			}
			if event := <-h.recorder.Events; !strings.Contains(event, "Warning HAProxyReloadFailed") {
				t.Fatalf("missing failure event: %s", event)
			}
		})
	}
}

func TestHAProxyReloadWaitsForAllReplicas(t *testing.T) {
	h := newReloadHarness(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "haproxy-2", Namespace: "test"}}
	if err := h.cc.Client.Delete(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
	if !step.Completed() {
		t.Fatal("missing replica did not defer reload")
	}
	outcome, err := step.Output()
	if err != nil || outcome.RequeueAfter == 0 || len(h.calls) != 0 {
		t.Fatalf("unexpected waiting result: %+v %v %v", outcome, err, h.calls)
	}
	cm, cr := h.saved(t)
	if !reflect.DeepEqual(cm.Data, h.cm.Data) || cr.Status.HAProxyReload.Phase != "Pending" {
		t.Fatal("missing replica changed configuration or failed to report Pending")
	}
}

func TestHAProxyHotReloadDeploymentDoesNotRollForConfigChanges(t *testing.T) {
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
		t.Fatal("configuration-only change altered the deployment")
	}
	if _, exists := after.Spec.Template.Annotations["configmap-hash"]; exists {
		t.Fatal("configuration hash still triggers pod rollouts")
	}
	container := after.Spec.Template.Spec.Containers[0]
	if container.VolumeMounts[0].SubPath != "" || container.Command[0] != haproxyreload.Executable {
		t.Fatal("hot reload is not wired to directory mounts and the reloader")
	}
	if after.Spec.Template.Spec.InitContainers[0].Image != "operator:test" {
		t.Fatal("reloader init container uses the wrong image")
	}
	if !reflect.DeepEqual(after.Spec.Template.Spec.InitContainers[0].Resources.Requests, container.Resources.Requests) {
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

func TestHAProxyHotReloadReconciliationKeepsDeploymentStable(t *testing.T) {
	h := newReloadHarness(t)
	if step := h.cc.ReconcileHAProxy(); !step.Completed() {
		t.Fatal("initial deployment did not wait for rollout")
	} else if _, err := step.Output(); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Name: "marklogic-haproxy", Namespace: "test"}
	if err := h.cc.Client.Get(h.cc.Ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	before := deployment.DeepCopy()
	for range 2 {
		step := h.cc.ReconcileHAProxy()
		if step.Completed() {
			_, err := step.Output()
			t.Fatalf("configuration reconciliation triggered rollout or failure: %v", err)
		}
	}
	h.cc.MarklogicCluster.Spec.HAProxy.Timeout.Client++
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		_, err := step.Output()
		t.Fatalf("configuration change triggered rollout or failure: %v", err)
	}
	if err := h.cc.Client.Get(h.cc.Ctx, key, deployment); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Spec, deployment.Spec) || before.ResourceVersion != deployment.ResourceVersion {
		t.Fatal("configuration-only reconciliation updated the deployment")
	}
}

func TestHAProxyDefaultReloadMigratesLegacyDeploymentOnce(t *testing.T) {
	h := newReloadHarness(t)
	objectMeta := generateObjectMeta("marklogic-haproxy", "test", h.cc.GetHAProxyLabels("cluster"), nil)
	legacy := h.cc.createHAProxyDeploymentDef(objectMeta)
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
	migrated := &appsv1.Deployment{}
	key := client.ObjectKeyFromObject(legacy)
	if err := h.cc.Client.Get(h.cc.Ctx, key, migrated); err != nil {
		t.Fatal(err)
	}
	if _, exists := migrated.Spec.Template.Annotations["configmap-hash"]; exists {
		t.Fatal("migration retained the pod-restart configuration hash")
	}
	if migrated.Spec.Template.Spec.Containers[0].Command[0] != haproxyreload.Executable {
		t.Fatal("legacy deployment was not migrated to the hot-reload runtime")
	}
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		_, err := step.Output()
		_, cluster := h.saved(t)
		t.Fatalf("migrated deployment triggered another rollout or failed: %v, status: %+v", err, cluster.Status.HAProxyReload)
	}
	after := &appsv1.Deployment{}
	if err := h.cc.Client.Get(h.cc.Ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if migrated.ResourceVersion != after.ResourceVersion {
		t.Fatal("default hot-reload reconciliation repeated the migration")
	}
}

type failHAProxyConfigMapUpdate struct{ client.Client }

func (c failHAProxyConfigMapUpdate) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if _, ok := object.(*corev1.ConfigMap); ok {
		return errors.New("injected ConfigMap commit failure")
	}
	return c.Client.Update(ctx, object, options...)
}

func TestHAProxyConfigMapCommitFailureRollsBack(t *testing.T) {
	h := newReloadHarness(t)
	h.cc.Client = failHAProxyConfigMapUpdate{h.cc.Client}
	step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
	if !step.Completed() {
		t.Fatal("ConfigMap commit failure was ignored")
	}
	_, err := step.Output()
	if err == nil || !strings.Contains(err.Error(), "ConfigMap commit failure") {
		t.Fatalf("commit error was hidden: %v", err)
	}
	for pod, hash := range h.states {
		if hash != haproxyreload.Hash([]byte(h.cm.Data["haproxy.cfg"])) {
			t.Errorf("pod %s not rolled back after ConfigMap commit failure", pod)
		}
	}
	cm, cr := h.saved(t)
	if !reflect.DeepEqual(cm.Data, h.cm.Data) || cr.Status.HAProxyReload.Phase != "Failed" {
		t.Fatal("failed commit altered the startup configuration or omitted status")
	}
}

func TestHAProxyRollbackFailureIsReported(t *testing.T) {
	h := newReloadHarness(t)
	execute := h.cc.HAProxyExec
	h.failure = "apply:haproxy-2"
	h.cc.HAProxyExec = func(ctx context.Context, pod *corev1.Pod, op string, data []byte) ([]byte, error) {
		if op == "apply" && string(data) == h.cm.Data["haproxy.cfg"] && pod.Name == "haproxy-1" {
			return nil, errors.New("replica unreachable during rollback")
		}
		return execute(ctx, pod, op, data)
	}
	step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
	_, err := step.Output()
	if err == nil || !strings.Contains(err.Error(), "replica unreachable during rollback") {
		t.Fatalf("rollback failure was hidden: %v", err)
	}
	_, cr := h.saved(t)
	if !strings.Contains(cr.Status.HAProxyReload.Message, "rollback of pod haproxy-1 failed") {
		t.Fatalf("rollback failure not reported in status: %+v", cr.Status.HAProxyReload)
	}
}

func TestHAProxyReloadRecoversPartialReplicaTransaction(t *testing.T) {
	h := newReloadHarness(t)
	h.states["haproxy-1"] = haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.failure = "validate:haproxy-2"
	step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
	if !step.Completed() {
		t.Fatal("validation failure after partial activation was ignored")
	}
	for _, hash := range h.states {
		if hash != haproxyreload.Hash([]byte(h.cm.Data["haproxy.cfg"])) {
			t.Fatal("previously activated replica not rolled back after operator restart")
		}
	}
}

func TestHAProxyStartupValidationFailureIsReported(t *testing.T) {
	h := newReloadHarness(t)
	pod := &corev1.Pod{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: "haproxy-1", Namespace: "test"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "haproxy", LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "HAProxy validation failed"},
		},
	}}
	if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
	if !step.Completed() {
		t.Fatal("startup failure was not reported")
	}
	_, err := step.Output()
	if err == nil || !strings.Contains(err.Error(), "HAProxy validation failed") {
		t.Fatalf("startup failure details were lost: %v", err)
	}
	_, cr := h.saved(t)
	if cr.Status.HAProxyReload.Phase != "Failed" {
		t.Fatal("startup failure remained Pending")
	}
}

func TestHAProxyReadyPodIgnoresTerminationHistory(t *testing.T) {
	h := newReloadHarness(t)
	pod := &corev1.Pod{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: "haproxy-1", Namespace: "test"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = append([]corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}}, pod.Status.Conditions...)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "haproxy", LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "HAProxy validation failed"},
		},
	}}
	if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
		t.Fatal(err)
	}
	if step := h.cc.reconcileHAProxyReload(h.cm, h.desired); step.Completed() {
		_, err := step.Output()
		t.Fatalf("recovered Ready pod blocked reload: %v", err)
	}
	_, cr := h.saved(t)
	if cr.Status.HAProxyReload.Phase != "Success" {
		t.Fatal("recovered pod was not reported as successful")
	}
}

func TestHAProxyUnreadyRunningPodRecoversBeforeReadinessGate(t *testing.T) {
	for _, recoveryFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("recoveryFails=%v", recoveryFails), func(t *testing.T) {
			h := newReloadHarness(t)
			pod := &corev1.Pod{}
			if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: "haproxy-1", Namespace: "test"}, pod); err != nil {
				t.Fatal(err)
			}
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "haproxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Message: "stale startup failure",
				}},
			}}
			if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
				t.Fatal(err)
			}
			execute := h.cc.HAProxyExec
			h.cc.HAProxyExec = func(ctx context.Context, pod *corev1.Pod, op string, data []byte) ([]byte, error) {
				if recoveryFails {
					h.calls = append(h.calls, op+":"+pod.Name)
					return nil, errors.New("restoring reload failed")
				}
				return execute(ctx, pod, op, data)
			}
			step := h.cc.reconcileHAProxyReload(h.cm, h.desired)
			outcome, err := step.Output()
			if !step.Completed() || (err != nil) != recoveryFails ||
				!reflect.DeepEqual(h.calls, []string{"status:haproxy-1"}) {
				t.Fatalf("recovery did not run before readiness gate: %v %v", err, h.calls)
			}
			cm, cr := h.saved(t)
			if !reflect.DeepEqual(cm.Data, h.cm.Data) {
				t.Fatal("unready pod allowed a candidate commit")
			}
			if recoveryFails {
				if cr.Status.HAProxyReload.Phase != "Failed" || !strings.Contains(cr.Status.HAProxyReload.Message, "restoring reload failed") {
					t.Fatal("recovery failure was not reported")
				}
				return
			}
			if outcome.RequeueAfter == 0 || cr.Status.HAProxyReload.Phase != "Pending" {
				t.Fatal("recovery must wait for the readiness probe")
			}
			pod.Status.Conditions[0].Status = corev1.ConditionTrue
			if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
				t.Fatal(err)
			}
			if step := h.cc.reconcileHAProxyReload(cm, h.desired); step.Completed() {
				_, err := step.Output()
				t.Fatalf("recovered pod blocked subsequent reload: %v", err)
			}
		})
	}
}

func TestHAProxyTLSRotationStagesBothSecretsUntilCommit(t *testing.T) {
	for _, newFile := range []string{"old.pem", "new.pem"} {
		for _, failValidation := range []bool{false, true} {
			t.Run(fmt.Sprintf("newFile=%s/failValidation=%v", newFile, failValidation), func(t *testing.T) {
				h := newReloadHarness(t)
				cr := h.cc.MarklogicCluster
				cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{
					Enabled: true, SecretName: "old-tls", CertFileName: "old.pem",
				}
				if err := h.cc.Client.Update(h.cc.Ctx, cr); err != nil {
					t.Fatal(err)
				}
				h.cm.Data = generateHAProxyConfigMapData(h.cc.Ctx, cr)
				if err := h.cc.Client.Update(h.cc.Ctx, h.cm); err != nil {
					t.Fatal(err)
				}
				for pod := range h.states {
					h.states[pod] = haproxyreload.Hash([]byte(h.cm.Data["haproxy.cfg"]))
				}
				if _, err := h.cc.ReconcileHAProxy().Output(); err != nil {
					t.Fatal(err)
				}
				_, cr = h.saved(t)
				cr.Spec.HAProxy.Tls.SecretName = "new-tls"
				cr.Spec.HAProxy.Tls.CertFileName = newFile
				if err := h.cc.Client.Update(h.cc.Ctx, cr); err != nil {
					t.Fatal(err)
				}
				h.cc.MarklogicCluster = cr
				h.desired = generateHAProxyConfigMapData(h.cc.Ctx, cr)
				if _, err := h.cc.ReconcileHAProxy().Output(); err != nil {
					t.Fatal(err)
				}
				deployment := &appsv1.Deployment{}
				if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cm), deployment); err != nil {
					t.Fatal(err)
				}
				var staged corev1.Volume
				for _, volume := range deployment.Spec.Template.Spec.Volumes {
					if volume.Name == "ssl-certificate" {
						staged = volume
					}
				}
				if staged.Projected == nil || len(staged.Projected.Sources) != 2 ||
					staged.Projected.Sources[0].Secret.Name != "old-tls" || staged.Projected.Sources[1].Secret.Name != "new-tls" {
					t.Fatalf("rotation did not retain both certificate Secrets: %+v", staged)
				}
				oldPath := staged.Projected.Sources[0].Secret.Items[0].Path
				newPath := staged.Projected.Sources[1].Secret.Items[0].Path
				if oldPath == newPath || !strings.Contains(h.cm.Data["haproxy.cfg"], "/"+oldPath) ||
					!strings.Contains(h.desired["haproxy.cfg"], "/"+newPath) {
					t.Fatal("staged certificate paths collide or do not match their configurations")
				}
				if _, err := h.cc.ReconcileHAProxy().Output(); err != nil || len(h.calls) != 0 {
					t.Fatalf("reload did not wait for staged pods: %v %v", err, h.calls)
				}
				for _, name := range []string{"haproxy-1", "haproxy-2"} {
					pod := &corev1.Pod{}
					if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, pod); err != nil {
						t.Fatal(err)
					}
					pod.Spec.Volumes = []corev1.Volume{staged}
					if err := h.cc.Client.Update(h.cc.Ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
				if failValidation {
					h.failure = "validate:haproxy-2"
				}
				execute := h.cc.HAProxyExec
				h.cc.HAProxyExec = func(ctx context.Context, pod *corev1.Pod, op string, data []byte) ([]byte, error) {
					current := &appsv1.Deployment{}
					if err := h.cc.Client.Get(ctx, client.ObjectKeyFromObject(h.cm), current); err != nil {
						t.Fatal(err)
					}
					for _, volume := range current.Spec.Template.Spec.Volumes {
						if volume.Name == staged.Name && !reflect.DeepEqual(volume, staged) {
							t.Fatal("old certificate removed before commit")
						}
					}
					return execute(ctx, pod, op, data)
				}
				_, err := h.cc.ReconcileHAProxy().Output()
				if (err != nil) != failValidation {
					t.Fatalf("unexpected rotation result: %v", err)
				}
				cm, _ := h.saved(t)
				if reflect.DeepEqual(cm.Data, h.desired) == failValidation {
					t.Fatal("rotation commit did not match validation outcome")
				}
				if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cm), deployment); err != nil {
					t.Fatal(err)
				}
				for _, volume := range deployment.Spec.Template.Spec.Volumes {
					if volume.Name != staged.Name {
						continue
					}
					if failValidation && !reflect.DeepEqual(volume, staged) {
						t.Fatal("failed rotation discarded the old certificate")
					}
					if !failValidation && (volume.Secret == nil || volume.Secret.SecretName != "new-tls") {
						t.Fatal("successful rotation did not remove the old Secret")
					}
				}
			})
		}
	}
}

func TestHAProxyTLSConfigChangeIgnoresDefaultedVolumeMode(t *testing.T) {
	h := newReloadHarness(t)
	h.cc.MarklogicCluster.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{
		Enabled: true, SecretName: "tls", CertFileName: "certificate.pem",
	}
	if err := h.cc.Client.Update(h.cc.Ctx, h.cc.MarklogicCluster); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cc.ReconcileHAProxy().Output(); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cm), deployment); err != nil {
		t.Fatal(err)
	}
	for i := range deployment.Spec.Template.Spec.Volumes {
		if secret := deployment.Spec.Template.Spec.Volumes[i].Secret; secret != nil {
			mode := int32(corev1.SecretVolumeSourceDefaultMode)
			secret.DefaultMode = &mode
		}
	}
	if err := h.cc.Client.Update(h.cc.Ctx, deployment); err != nil {
		t.Fatal(err)
	}
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		_, err := step.Output()
		t.Fatalf("defaulted volume mode triggered a rollout: %v", err)
	}
	current := &appsv1.Deployment{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cm), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Spec.Template, deployment.Spec.Template) {
		t.Fatal("configuration-only change altered the TLS pod template")
	}
}

func TestHAProxyInvalidBootstrapCanBeCorrected(t *testing.T) {
	h := newReloadHarness(t)
	h.cc.Annotations = map[string]string{"example.com/annotation": "preserved"}
	if err := h.cc.Client.Delete(h.cc.Ctx, h.cm); err != nil {
		t.Fatal(err)
	}
	h.cc.MarklogicCluster.Spec.HAProxy.Stats.Enabled = true
	h.cc.MarklogicCluster.Spec.HAProxy.Stats.Port = -1
	if err := h.cc.Client.Update(h.cc.Ctx, h.cc.MarklogicCluster); err != nil {
		t.Fatal(err)
	}
	if step := h.cc.ReconcileHAProxy(); !step.Completed() {
		t.Fatal("initial deployment did not wait for rollout")
	} else if _, err := step.Output(); err != nil {
		t.Fatal(err)
	}
	if h.cc.Annotations[haproxyBootstrapAnnotation] != "" {
		t.Fatal("bootstrap annotation leaked into shared resource annotations")
	}
	for _, name := range []string{"haproxy-1", "haproxy-2"} {
		pod := &corev1.Pod{}
		if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Conditions = nil
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "haproxy", LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "HAProxy validation failed: invalid port -1"},
			},
		}}
		if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.cc.ReconcileHAProxy().Output(); err == nil {
		t.Fatal("invalid bootstrap was not reported")
	}
	cm, cr := h.saved(t)
	if !strings.Contains(cm.Data["haproxy.cfg"], "bind *:-1") || cm.Annotations[haproxyBootstrapAnnotation] != "pending" {
		t.Fatal("initial configuration was not marked as bootstrap")
	}
	cr.Spec.HAProxy.Stats.Port = 8404
	if err := h.cc.Client.Update(h.cc.Ctx, cr); err != nil {
		t.Fatal(err)
	}
	h.cc.MarklogicCluster = cr
	corrected := generateHAProxyConfigMapData(h.cc.Ctx, cr)
	step := h.cc.ReconcileHAProxy()
	outcome, err := step.Output()
	if !step.Completed() || err != nil || outcome.RequeueAfter == 0 {
		t.Fatalf("bootstrap correction did not wait for startup: %+v %v", outcome, err)
	}
	cm, cr = h.saved(t)
	if !reflect.DeepEqual(cm.Data, corrected) || cr.Status.HAProxyReload.Phase != "Pending" || len(h.calls) != 0 {
		t.Fatal("failed bootstrap configuration was not corrected without exec")
	}
	for _, name := range []string{"haproxy-1", "haproxy-2"} {
		pod := &corev1.Pod{}
		if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
			t.Fatal(err)
		}
		h.states[name] = haproxyreload.Hash([]byte(corrected["haproxy.cfg"]))
	}
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		_, err := step.Output()
		t.Fatalf("corrected bootstrap did not recover: %v", err)
	}
	cm, cr = h.saved(t)
	if cm.Annotations[haproxyBootstrapAnnotation] != "" || cr.Status.HAProxyReload.Phase != "Success" {
		t.Fatal("successful bootstrap was not committed")
	}
}

func TestHAProxyBootstrapCorrectionRequiresAllReplicasToHaveFailedValidation(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		for _, failure := range []string{"HAProxy validation failed", "image startup failed"} {
			t.Run(fmt.Sprintf("bootstrap=%v/failure=%s", bootstrap, failure), func(t *testing.T) {
				h := newReloadHarness(t)
				if bootstrap {
					h.cm.Annotations = map[string]string{haproxyBootstrapAnnotation: "pending"}
					if err := h.cc.Client.Update(h.cc.Ctx, h.cm); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range []string{"haproxy-1", "haproxy-2"} {
					pod := &corev1.Pod{}
					if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, pod); err != nil {
						t.Fatal(err)
					}
					if !bootstrap || failure != "HAProxy validation failed" || name == "haproxy-1" {
						pod.Status.Conditions = nil
					}
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
						Name: "haproxy", State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: failure},
						},
					}}
					if err := h.cc.Client.Status().Update(h.cc.Ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := h.cc.reconcileHAProxyReload(h.cm, h.desired).Output(); err == nil {
					t.Fatal("startup failure was not reported")
				}
				cm, _ := h.saved(t)
				if !reflect.DeepEqual(cm.Data, h.cm.Data) || len(h.calls) != 0 {
					t.Fatal("committed or partially started configuration was overwritten")
				}
			})
		}
	}
}

func TestHAProxyTLSRemovalCommitsBeforeRemovingCertificate(t *testing.T) {
	for _, failValidation := range []bool{false, true} {
		t.Run(fmt.Sprintf("failValidation=%v", failValidation), func(t *testing.T) {
			h := newReloadHarness(t)
			h.cc.MarklogicCluster.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{
				Enabled: true, SecretName: "haproxy-tls", CertFileName: "certificate.pem",
			}
			if err := h.cc.Client.Update(h.cc.Ctx, h.cc.MarklogicCluster); err != nil {
				t.Fatal(err)
			}
			h.cm.Data = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
			if err := h.cc.Client.Update(h.cc.Ctx, h.cm); err != nil {
				t.Fatal(err)
			}
			for pod := range h.states {
				h.states[pod] = haproxyreload.Hash([]byte(h.cm.Data["haproxy.cfg"]))
			}
			if _, err := h.cc.ReconcileHAProxy().Output(); err != nil {
				t.Fatal(err)
			}
			_, cr := h.saved(t)
			cr.Spec.HAProxy.Tls = nil
			if err := h.cc.Client.Update(h.cc.Ctx, cr); err != nil {
				t.Fatal(err)
			}
			h.cc.MarklogicCluster = cr
			h.desired = generateHAProxyConfigMapData(h.cc.Ctx, cr)
			if failValidation {
				h.failure = "validate:haproxy-2"
			}
			execute := h.cc.HAProxyExec
			h.cc.HAProxyExec = func(ctx context.Context, pod *corev1.Pod, op string, data []byte) ([]byte, error) {
				deployment := &appsv1.Deployment{}
				if err := h.cc.Client.Get(ctx, client.ObjectKeyFromObject(h.cm), deployment); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, volume := range deployment.Spec.Template.Spec.Volumes {
					found = found || volume.Name == "ssl-certificate"
				}
				if !found {
					t.Fatal("certificate removed before non-TLS reload completed")
				}
				return execute(ctx, pod, op, data)
			}
			step := h.cc.ReconcileHAProxy()
			_, err := step.Output()
			if failValidation != (err != nil) {
				t.Fatalf("unexpected TLS transition result: %v", err)
			}
			cm, _ := h.saved(t)
			deployment := &appsv1.Deployment{}
			if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cm), deployment); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, volume := range deployment.Spec.Template.Spec.Volumes {
				found = found || volume.Name == "ssl-certificate"
			}
			if found != failValidation {
				t.Fatal("certificate removal did not respect reload success")
			}
			expected := h.desired
			if failValidation {
				expected = h.cm.Data
			}
			if !reflect.DeepEqual(cm.Data, expected) {
				t.Fatal("TLS transition committed the wrong startup configuration")
			}
		})
	}
}
