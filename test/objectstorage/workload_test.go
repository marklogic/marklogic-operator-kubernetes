// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"reflect"
	"testing"
	"time"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readyWorkloadPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "marklogic-server"}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "marklogic-server", Ready: true, ContainerID: "container", ImageID: "image", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Unix(100, 0))}}}}}}
}

func TestWorkloadSnapshotDetectsSamePodContainerRestart(t *testing.T) {
	pod := readyWorkloadPod()
	before, err := workloadPodIdentity(pod)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"count", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount++ }},
		{"container-replaced-with-count-reset", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "replacement" }},
		{"start-time", func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Unix(200, 0))
		}},
		{"image", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "replacement-image" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := pod.DeepCopy()
			tc.mutate(changed)
			after, err := workloadPodIdentity(changed)
			if err != nil || reflect.DeepEqual(before, after) {
				t.Fatal("same-Pod container change was not observable")
			}
		})
	}
}

func TestWorkloadSnapshotRejectsIncompleteOrUnreadyPods(t *testing.T) {
	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.UID = "" },
		func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
		func(p *corev1.Pod) { p.Status.Conditions = nil },
		func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false },
		func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" },
		func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running = nil },
		func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar"}) },
	} {
		pod := readyWorkloadPod()
		mutate(pod)
		if _, err := workloadPodIdentity(pod); err == nil {
			t.Fatal("unverifiable or disrupted workload was accepted")
		}
	}
}

func TestChildSnapshotDetectsGroupAndStatefulSetReplacement(t *testing.T) {
	g := &ml.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{UID: "group"}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{UID: "sts"}}
	pods := []*corev1.Pod{readyWorkloadPod()}
	before, err := snapshotChildren(g, sts, pods)
	if err != nil {
		t.Fatal(err)
	}
	g.UID = "replacement-group"
	after, err := snapshotChildren(g, sts, pods)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("replaced group was accepted")
	}
	g.UID = "group"
	sts.UID = "replacement-sts"
	after, err = snapshotChildren(g, sts, pods)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("replaced StatefulSet was accepted")
	}
}

func TestIncompleteCheckpointAssertionRequiresOtherwiseCurrentMarkers(t *testing.T) {
	secret := testutils.SecretMeta{Name: "credentials", UID: "secret", ResourceVersion: "revision"}
	entry := &ml.ObjectStorageProviderStatus{Phase: ml.ObjectStoragePhaseApplied, AuthType: "secret", ObservedGeneration: 3,
		ObservedSecret:     &ml.ObjectStorageSecretRef{Name: secret.Name, UID: secret.UID, ResourceVersion: secret.ResourceVersion},
		AppliedFingerprint: "opaque-existing-fingerprint", LastAppliedTime: &metav1.Time{Time: time.Unix(1, 0)}}
	c := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Spec: ml.MarklogicClusterSpec{ObjectStorage: &ml.ObjectStorageConfig{AWS: &ml.AWSObjectStorage{SecretName: secret.Name}, Azure: &ml.AzureObjectStorage{SecretName: secret.Name}}}, Status: ml.MarklogicClusterStatus{ObjectStorage: &ml.ObjectStorageStatus{AWS: entry.DeepCopy(), Azure: entry.DeepCopy()}}}
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		if err := currentIncompleteCheckpoint(c, provider, secret); err != nil {
			t.Fatal(err)
		}
		for _, mutate := range []func(*ml.MarklogicCluster){
			func(c *ml.MarklogicCluster) { c.Generation++ },
			func(c *ml.MarklogicCluster) { providerStatus(c, provider).ObservedSecret.ResourceVersion = "stale" },
			func(c *ml.MarklogicCluster) { providerStatus(c, provider).ObservedSecret.UID = "stale" },
			func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage = nil },
			func(c *ml.MarklogicCluster) { value := true; providerStatus(c, provider).DetachEligible = &value },
		} {
			changed := c.DeepCopy()
			mutate(changed)
			if currentIncompleteCheckpoint(changed, provider, secret) == nil {
				t.Fatal("confounded checkpoint readiness test accepted")
			}
		}
	}
}
