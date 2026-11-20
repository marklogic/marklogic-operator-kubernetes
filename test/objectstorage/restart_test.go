// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func readyOperatorPod(name string, uid types.UID) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "operators", UID: uid},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "candidate:exact"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", Ready: true, ContainerID: "containerd://" + string(uid), ImageID: "sha256:known", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Unix(100, 0))}},
		}}},
	}
}

func readyOperatorDeployment() *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "release-controller-manager", Namespace: "operators", UID: "deployment", Generation: 4},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "candidate:exact"}}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2},
	}
}

func TestOperatorRestartPlanPreservesReplicasAndRequiresStableRollout(t *testing.T) {
	d := readyOperatorDeployment()
	plan, err := deploymentPlan(d, "candidate:exact", 2)
	if err != nil || plan.Replicas != 2 || plan.UID != d.UID || plan.Image != "candidate:exact" {
		t.Fatal("ready multi-replica Deployment did not produce the original restart plan")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.Deployment)
	}{
		{"replacement_without_identity", func(d *appsv1.Deployment) { d.UID = "" }},
		{"terminating", func(d *appsv1.Deployment) { now := metav1.Now(); d.DeletionTimestamp = &now }},
		{"scaled_to_zero", func(d *appsv1.Deployment) { zero := int32(0); d.Spec.Replicas = &zero }},
		{"default_replicas_ambiguous", func(d *appsv1.Deployment) { d.Spec.Replicas = nil }},
		{"unobserved_change", func(d *appsv1.Deployment) { d.Generation++ }},
		{"old_replica_remaining", func(d *appsv1.Deployment) { d.Status.UpdatedReplicas-- }},
		{"unready_replica", func(d *appsv1.Deployment) { d.Status.ReadyReplicas-- }},
		{"surge_replica", func(d *appsv1.Deployment) { d.Status.Replicas++ }},
		{"different_image", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "other:image" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := readyOperatorDeployment()
			tc.mutate(d)
			if _, err := deploymentPlan(d, "candidate:exact", 2); err == nil {
				t.Fatal("unsafe Deployment accepted for restart")
			}
		})
	}
	if _, err := deploymentPlan(d, "candidate:exact", 1); err == nil {
		t.Fatal("partially observed pod set accepted for restart")
	}
}

func TestOperatorLogSourcesRequireEveryManagerIdentity(t *testing.T) {
	pods := []corev1.Pod{readyOperatorPod("first", "one"), readyOperatorPod("second", "two")}
	pods[1].Status.ContainerStatuses[0].RestartCount = 3
	sources, err := operatorSources(pods, "candidate:exact")
	if err != nil || len(sources) != 2 || sources["two"] != 3 {
		t.Fatal("log baseline did not capture every manager restart count")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"missing_uid", func(p *corev1.Pod) { p.UID = "" }},
		{"terminating", func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now }},
		{"wrong_image", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "different" }},
		{"missing_manager_spec", func(p *corev1.Pod) { p.Spec.Containers[0].Name = "sidecar" }},
		{"missing_manager_status", func(p *corev1.Pod) { p.Status.ContainerStatuses = nil }},
		{"unready", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false }},
		{"missing_image_id", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "" }},
		{"missing_container_id", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" }},
		{"missing_start_time", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.Time{} }},
		{"terminated", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := readyOperatorPod("first", "one")
			tc.mutate(&pod)
			if _, err := operatorSources([]corev1.Pod{pod}, "candidate:exact"); err == nil {
				t.Fatal("incomplete operator log source was accepted")
			}
		})
	}
	if _, err := operatorSources(nil, "candidate:exact"); err == nil {
		t.Fatal("empty operator log baseline was accepted")
	}
}

func TestOperatorLogEpochRejectsUnscannedStartupRestarts(t *testing.T) {
	started := metav1.NewTime(time.Unix(200, 0))
	oldSources := map[types.UID]int32{"previous-pod": 0}
	closed := []operatorLogEpoch{{Sources: oldSources, Scanned: true, Closed: true}}
	for _, tc := range []struct {
		name     string
		epochs   []operatorLogEpoch
		previous map[types.UID]int32
		uid      types.UID
		restarts int32
		started  int64
		ok       bool
	}{
		{"initial_no_restart", nil, nil, "new-pod", 0, 300, true},
		{"initial_historical_restart", nil, nil, "existing-pod", 3, 100, true},
		{"initial_restart_during_run", nil, nil, "existing-pod", 1, 250, false},
		{"initial_restart_at_run_start", nil, nil, "existing-pod", 1, 200, false},
		{"new_epoch_clean_start", closed, oldSources, "new-pod", 0, 300, true},
		{"new_epoch_startup_crash", closed, oldSources, "new-pod", 1, 300, false},
		{"new_epoch_multiple_startup_crashes", closed, oldSources, "new-pod", 2, 300, false},
		{"new_epoch_reused_pod", closed, oldSources, "previous-pod", 0, 100, false},
		{"new_epoch_wrong_previous_sources", closed, map[types.UID]int32{"unrelated-pod": 0}, "new-pod", 0, 300, false},
		{"reset_without_transition", closed, nil, "new-pod", 0, 300, false},
		{"transition_without_history", nil, oldSources, "new-pod", 0, 300, false},
		{"unscanned_prior_epoch", []operatorLogEpoch{{Sources: oldSources, Closed: true}}, oldSources, "new-pod", 0, 300, false},
		{"unclosed_prior_epoch", []operatorLogEpoch{{Sources: oldSources, Scanned: true}}, oldSources, "new-pod", 0, 300, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := readyOperatorPod("controller", tc.uid)
			pod.Status.ContainerStatuses[0].RestartCount = tc.restarts
			pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Unix(tc.started, 0))
			instances, err := operatorLogInstances([]corev1.Pod{pod}, "candidate:exact")
			if err != nil {
				t.Fatal(err)
			}
			err = validateOperatorLogEpochStart(tc.epochs, tc.previous, instances, started)
			if (err == nil) != tc.ok {
				t.Fatal("log epoch accepted missing startup logs or rejected an intact baseline")
			}
		})
	}
}

func TestOperatorLogInstancesDetectRestartCounterReset(t *testing.T) {
	before := readyOperatorPod("controller", "same-pod")
	baseline, err := operatorLogInstances([]corev1.Pod{before}, "candidate:exact")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.ContainerStatus)
	}{
		{"container_replaced", func(s *corev1.ContainerStatus) { s.ContainerID = "containerd://replacement" }},
		{"image_replaced", func(s *corev1.ContainerStatus) { s.ImageID = "sha256:different" }},
		{"instance_started_again", func(s *corev1.ContainerStatus) { s.State.Running.StartedAt = metav1.NewTime(time.Unix(300, 0)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := before.DeepCopy()
			tc.mutate(&after.Status.ContainerStatuses[0])
			// Preserve both Pod UID and restart count, which the former baseline used.
			instances, err := operatorLogInstances([]corev1.Pod{*after}, "candidate:exact")
			if err != nil || reflect.DeepEqual(baseline, instances) {
				t.Fatal("different controller log instance was not detected")
			}
		})
	}
}

func TestOperatorStartupTracksSourcesBeforeReadiness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]corev1.Pod) []corev1.Pod
		ok     bool
	}{
		{"same_instance_ready", func(pods []corev1.Pod) []corev1.Pod { pods[0].Status.ContainerStatuses[0].Ready = true; return pods }, true},
		{"observed_pod_disappears", func(_ []corev1.Pod) []corev1.Pod { return nil }, false},
		{"new_pod_replaces_unready_pod", func(_ []corev1.Pod) []corev1.Pod {
			return []corev1.Pod{readyOperatorPod("replacement", "replacement-uid")}
		}, false},
		{"container_replaced_before_ready", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
			return pods
		}, false},
		{"terminated_before_ready", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			return pods
		}, false},
		{"restarted_before_ready", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].RestartCount = 1
			return pods
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := readyOperatorPod("controller", "startup-pod")
			pod.Status.ContainerStatuses[0].Ready = false
			seen := map[types.UID]operatorLogInstance{}
			if err := observeOperatorStartup([]corev1.Pod{pod}, seen, nil, 1); err != nil {
				t.Fatal("could not record running but not-yet-ready controller")
			}
			err := observeOperatorStartup(tc.mutate([]corev1.Pod{*pod.DeepCopy()}), seen, nil, 1)
			if (err == nil) != tc.ok {
				t.Fatal("startup observation lost a controller log source or rejected an intact source")
			}
		})
	}
	// Pending Pods can acquire their first container identity normally.
	pod := readyOperatorPod("controller", "startup-pod")
	pending := pod.DeepCopy()
	pending.Status.ContainerStatuses = nil
	seen := map[types.UID]operatorLogInstance{}
	if err := observeOperatorStartup([]corev1.Pod{*pending}, seen, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := observeOperatorStartup([]corev1.Pod{pod}, seen, nil, 1); err != nil {
		t.Fatal("normal first container startup was rejected")
	}
}

type failedOperatorLogReader struct{}

func (failedOperatorLogReader) Read([]byte) (int, error) {
	return 0, errors.New("sensitive-reader-error")
}

func TestOperatorLogCaptureFailsClosedAtClientLimit(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		limit      int64
		ok         bool
	}{
		{"short_complete_log", "safe log", 16, true},
		{"exact_limit_complete_log", "safe log", 8, true},
		{"one_byte_over_limit", "safe logs", 8, false},
		{"credential_after_bounded_prefix", "safe logs sensitive-value", 8, false},
		{"credential_in_complete_log", "sensitive-value", 128, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inspectOperatorLogStream(strings.NewReader(tc.data), tc.limit, []string{"sensitive-value"})
			if (err == nil) != tc.ok {
				t.Fatal("bounded log capture accepted incomplete or unsafe evidence")
			}
			if err != nil && strings.Contains(err.Error(), "sensitive-value") {
				t.Fatal("log safety assertion exposed matched credential material")
			}
		})
	}
	if err := inspectOperatorLogStream(failedOperatorLogReader{}, 128, nil); err == nil || strings.Contains(err.Error(), "sensitive-reader-error") {
		t.Fatal("log read failure was accepted or exposed reader diagnostics")
	}
}

func TestDeploymentLookupFollowsControllerUIDsAndRejectsAmbiguity(t *testing.T) {
	controller := true
	owner := func(kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: &controller}
	}
	for _, tc := range []struct {
		name string
		edit func([]corev1.Pod, *appsv1.ReplicaSet, *appsv1.Deployment) []runtime.Object
		ok   bool
	}{
		{"helm_prefix_agnostic", func(_ []corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) []runtime.Object { return nil }, true},
		{"pod_owner_replaced", func(p []corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) []runtime.Object {
			p[0].OwnerReferences[0].UID = "stale-rs"
			return nil
		}, false},
		{"deployment_owner_replaced", func(_ []corev1.Pod, rs *appsv1.ReplicaSet, _ *appsv1.Deployment) []runtime.Object {
			rs.OwnerReferences[0].UID = "stale-deployment"
			return nil
		}, false},
		{"unmanaged_pod", func(p []corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) []runtime.Object {
			p[0].OwnerReferences = nil
			return nil
		}, false},
		{"multiple_deployments", func(p []corev1.Pod, _ *appsv1.ReplicaSet, d *appsv1.Deployment) []runtime.Object {
			other := d.DeepCopy()
			other.Name, other.UID = "another-operator", "other-deployment"
			otherRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "another-rs", Namespace: "operators", UID: "other-rs", OwnerReferences: []metav1.OwnerReference{owner("Deployment", other.Name, other.UID)}}}
			p[1].OwnerReferences = []metav1.OwnerReference{owner("ReplicaSet", otherRS.Name, otherRS.UID)}
			return []runtime.Object{other, otherRS}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := readyOperatorDeployment()
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "release-controller-manager-7a", Namespace: "operators", UID: "rs", OwnerReferences: []metav1.OwnerReference{owner("Deployment", d.Name, d.UID)}}}
			pods := []corev1.Pod{readyOperatorPod("one", "pod-one"), readyOperatorPod("two", "pod-two")}
			for i := range pods {
				pods[i].OwnerReferences = []metav1.OwnerReference{owner("ReplicaSet", rs.Name, rs.UID)}
			}
			extra := tc.edit(pods, rs, d)
			objects := append([]runtime.Object{d, rs}, extra...)
			s := &scenario{opts: Options{OperatorNamespace: "operators"}, kube: fake.NewClientset(objects...)}
			result, err := s.deploymentForPods(context.Background(), pods)
			if (err == nil) != tc.ok {
				t.Fatal("operator owner lookup did not enforce Deployment identity")
			}
			if tc.ok && result.UID != d.UID {
				t.Fatal("operator owner lookup selected a different Deployment")
			}
		})
	}
}
