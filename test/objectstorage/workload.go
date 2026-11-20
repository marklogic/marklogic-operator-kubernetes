// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

type workloadContainerState struct {
	ID, ImageID  string
	RestartCount int32
	StartedAt    metav1.Time
}

type workloadPodState struct {
	UID        types.UID
	Containers map[string]workloadContainerState
}

// A Pod UID survives container crashes. Retain each running instance as well so
// a crash/restart cannot masquerade as an uninterrupted workload.
func workloadPodIdentity(pod *corev1.Pod) (workloadPodState, error) {
	invalid := errors.New("workload pod is not ready with complete running container identities")
	state := workloadPodState{UID: pod.UID, Containers: map[string]workloadContainerState{}}
	if pod.UID == "" || pod.DeletionTimestamp != nil {
		return state, invalid
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready || len(pod.Spec.Containers) == 0 {
		return state, invalid
	}
	for _, container := range pod.Spec.Containers {
		found := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != container.Name {
				continue
			}
			if found || !status.Ready || status.ContainerID == "" || status.ImageID == "" || status.State.Running == nil || status.State.Running.StartedAt.IsZero() {
				return state, invalid
			}
			found = true
			state.Containers[container.Name] = workloadContainerState{ID: status.ContainerID, ImageID: status.ImageID, RestartCount: status.RestartCount, StartedAt: status.State.Running.StartedAt}
		}
		if !found {
			return state, invalid
		}
	}
	return state, nil
}

type childSnapshot struct {
	GroupUID, StatefulSetUID                  types.UID
	Annotations, Labels                       map[string]string
	StatefulSetAnnotations, StatefulSetLabels map[string]string
	Template                                  corev1.PodTemplateSpec
	Pods                                      []workloadPodState
}

func snapshotChildren(g *ml.MarklogicGroup, sts *appsv1.StatefulSet, pods []*corev1.Pod) (childSnapshot, error) {
	state := childSnapshot{}
	if g.UID == "" || sts.UID == "" || g.DeletionTimestamp != nil || sts.DeletionTimestamp != nil {
		return state, errors.New("workload group or StatefulSet identity is unavailable or terminating")
	}
	state = childSnapshot{GroupUID: g.UID, StatefulSetUID: sts.UID, Annotations: g.Annotations, Labels: g.Labels,
		StatefulSetAnnotations: sts.Annotations, StatefulSetLabels: sts.Labels, Template: sts.Spec.Template}
	for _, pod := range pods {
		identity, err := workloadPodIdentity(pod)
		if err != nil {
			return childSnapshot{}, err
		}
		state.Pods = append(state.Pods, identity)
	}
	return state, nil
}

func (s *scenario) assertChildrenStable(t *testing.T, before childSnapshot) {
	t.Helper()
	settled := time.Time{}
	err := wait.PollUntilContextTimeout(s.ctx, pollInterval, statusTimeout, true, func(ctx context.Context) (bool, error) {
		if !reflect.DeepEqual(before, s.childrenAt(ctx, t)) {
			return false, errors.New("workload metadata or running container identity changed")
		}
		sts := &appsv1.StatefulSet{}
		if s.client.Get(ctx, s.key(s.group), sts) != nil || sts.Status.ObservedGeneration < sts.Generation || sts.Status.ReadyReplicas != int32(len(before.Pods)) {
			settled = time.Time{}
			return false, nil
		}
		if settled.IsZero() {
			settled = time.Now()
		}
		return time.Since(settled) >= 10*time.Second, nil
	})
	if err != nil {
		t.Fatal("workload changed or did not remain settled after reconciliation (details suppressed)")
	}
}

// Current markers are required here so stale generation/revision cannot be the
// reason a deliberately incomplete checkpoint is rejected by the shared waiter.
func currentIncompleteCheckpoint(c *ml.MarklogicCluster, p testutils.ObjectStorageProvider, secret testutils.SecretMeta) error {
	e := providerStatus(c, p)
	name := ""
	if c.Spec.ObjectStorage != nil {
		if p == testutils.ObjectStorageAWS && c.Spec.ObjectStorage.AWS != nil {
			name = c.Spec.ObjectStorage.AWS.SecretName
		}
		if p == testutils.ObjectStorageAzure && c.Spec.ObjectStorage.Azure != nil {
			name = c.Spec.ObjectStorage.Azure.SecretName
		}
	}
	if name == "" || name != secret.Name || secret.UID == "" || secret.ResourceVersion == "" ||
		e == nil || e.AuthType != "secret" || e.Phase != ml.ObjectStoragePhaseApplied || e.ObservedGeneration != c.Generation || e.ObservedSecret == nil ||
		e.ObservedSecret.Name != secret.Name || e.ObservedSecret.UID != secret.UID || e.ObservedSecret.ResourceVersion != secret.ResourceVersion ||
		e.DetachEligible != nil || e.AppliedSecret != nil || e.AppliedFingerprint == "" || e.LastAppliedTime == nil {
		return errors.New("incomplete checkpoint does not have current observed markers and preserved success history")
	}
	if ready, _ := testutils.ObjectStorageReady(c, p, secret); ready {
		return errors.New("incomplete checkpoint with current observed markers incorrectly satisfies readiness")
	}
	return nil
}

func (s *scenario) assertCurrentIncompleteCheckpoint(t *testing.T) {
	t.Helper()
	c := &ml.MarklogicCluster{}
	if s.client.Get(s.ctx, s.key(s.name), c) != nil || c.UID != s.clusterUID {
		t.Fatal("cannot read persisted incomplete checkpoints from the same cluster")
	}
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		e := providerStatus(c, provider)
		if e == nil || e.ObservedSecret == nil {
			t.Fatal("incomplete checkpoint lacks observed Secret metadata")
		}
		secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
		if s.client.Get(s.ctx, s.key(e.ObservedSecret.Name), secret) != nil {
			t.Fatal("cannot read current Secret metadata for incomplete-checkpoint assertion")
		}
		if err := currentIncompleteCheckpoint(c, provider, testutils.SecretMeta{Name: secret.Name, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion}); err != nil {
			t.Fatal(err)
		}
	}
}
