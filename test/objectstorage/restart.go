// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

const operatorSelector = "control-plane=controller-manager"

// Each intentional stop closes the current log epoch before deleting its pods. Unexpected
// pod replacement or container restart never silently establishes a new baseline.
type operatorLogEpoch struct {
	Sources   map[types.UID]int32
	Instances map[types.UID]operatorLogInstance
	Scanned   bool
	Closed    bool
}

// RestartCount is not a complete container identity: kubelet state loss can reset
// it. Track the actual instance as well so logs from a replacement cannot silently
// stand in for an earlier instance of the same Pod.
type operatorLogInstance struct {
	ContainerID string
	ImageID     string
	StartedAt   metav1.Time
	Restarts    int32
}

type operatorDeployment struct {
	Namespace string
	Name      string
	UID       types.UID
	Replicas  int32
	Image     string
}

func operatorSources(pods []corev1.Pod, image string) (map[types.UID]int32, error) {
	sources := map[types.UID]int32{}
	for _, pod := range pods {
		if pod.UID == "" || pod.DeletionTimestamp != nil {
			return nil, fmt.Errorf("operator pod is missing identity or terminating")
		}
		foundImage, foundStatus := false, false
		for _, container := range pod.Spec.Containers {
			if container.Name == "manager" {
				if container.Image != image {
					return nil, fmt.Errorf("operator manager image does not match requested image")
				}
				foundImage = true
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "manager" {
				if !status.Ready || status.ImageID == "" || status.ContainerID == "" || status.State.Running == nil || status.State.Running.StartedAt.IsZero() || status.RestartCount < 0 {
					return nil, fmt.Errorf("operator manager is not running with a ready image identity")
				}
				sources[pod.UID], foundStatus = status.RestartCount, true
			}
		}
		if !foundImage || !foundStatus {
			return nil, fmt.Errorf("operator manager identity is incomplete")
		}
	}
	if len(sources) == 0 || len(sources) != len(pods) {
		return nil, fmt.Errorf("operator log source set is empty or ambiguous")
	}
	return sources, nil
}

func operatorLogInstances(pods []corev1.Pod, image string) (map[types.UID]operatorLogInstance, error) {
	if _, err := operatorSources(pods, image); err != nil {
		return nil, err
	}
	instances := map[types.UID]operatorLogInstance{}
	for _, pod := range pods {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "manager" {
				instances[pod.UID] = operatorLogInstance{ContainerID: status.ContainerID, ImageID: status.ImageID, StartedAt: status.State.Running.StartedAt, Restarts: status.RestartCount}
			}
		}
	}
	return instances, nil
}

func validateOperatorLogEpochStart(epochs []operatorLogEpoch, previous map[types.UID]int32, instances map[types.UID]operatorLogInstance, runStarted metav1.Time) error {
	if len(instances) == 0 {
		return fmt.Errorf("operator log epoch has no container instances")
	}
	if previous != nil {
		if len(epochs) == 0 || !epochs[len(epochs)-1].Closed || !epochs[len(epochs)-1].Scanned || !reflect.DeepEqual(previous, epochs[len(epochs)-1].Sources) {
			return fmt.Errorf("operator log epoch transition lacks a verified pre-stop scan")
		}
		for uid, instance := range instances {
			if _, exists := previous[uid]; exists {
				return fmt.Errorf("operator restart retained a previous controller pod")
			}
			// Only current logs are available reliably. A controller that crashed while
			// startup was being awaited has already lost an unscanned log instance.
			if instance.Restarts != 0 {
				return fmt.Errorf("operator restarted before its new log epoch could be recorded")
			}
		}
	} else {
		if len(epochs) != 0 {
			return fmt.Errorf("operator log epoch cannot be reset without a controlled stop")
		}
		for _, instance := range instances {
			if instance.Restarts > 0 && !instance.StartedAt.Before(&runStarted) {
				return fmt.Errorf("operator restarted during this run before initial log capture")
			}
		}
	}
	return nil
}

// Remember sources while waiting for readiness too. Otherwise an observed failing
// startup Pod could be replaced by a healthy Pod before the log epoch opens.
func observeOperatorStartup(pods []corev1.Pod, seen map[types.UID]operatorLogInstance, previous map[types.UID]int32, replicas int32) error {
	if len(pods) > int(replicas) {
		return fmt.Errorf("unexpected extra controller pod appeared during startup")
	}
	present := map[types.UID]bool{}
	for _, pod := range pods {
		if pod.UID == "" || pod.DeletionTimestamp != nil || present[pod.UID] {
			return fmt.Errorf("controller pod identity is missing, duplicated or terminating during startup")
		}
		if _, exists := previous[pod.UID]; exists {
			return fmt.Errorf("previous controller pod appeared during startup")
		}
		present[pod.UID] = true
		instance := operatorLogInstance{}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != "manager" {
				continue
			}
			if status.RestartCount != 0 || status.State.Terminated != nil {
				return fmt.Errorf("controller exited or restarted during startup before log capture")
			}
			instance.ContainerID, instance.ImageID = status.ContainerID, status.ImageID
			if status.State.Running != nil {
				instance.StartedAt = status.State.Running.StartedAt
			}
		}
		prior := seen[pod.UID]
		if (prior.ContainerID != "" && prior.ContainerID != instance.ContainerID) ||
			(prior.ImageID != "" && prior.ImageID != instance.ImageID) ||
			(!prior.StartedAt.IsZero() && !prior.StartedAt.Equal(&instance.StartedAt)) {
			return fmt.Errorf("controller container instance changed while awaiting readiness")
		}
		seen[pod.UID] = instance
	}
	for uid := range seen {
		if !present[uid] {
			return fmt.Errorf("observed controller pod disappeared during startup before log capture")
		}
	}
	return nil
}

func inspectOperatorLogStream(stream io.Reader, limit int64, values []string) error {
	if limit < 1 {
		return fmt.Errorf("operator log capture requires a positive byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil {
		return fmt.Errorf("cannot read operator logs (details suppressed)")
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("operator log safety check incomplete: bounded log capture reached its limit")
	}
	if containsMaterial(data, values) {
		return fmt.Errorf("credential material detected in operator logs (value suppressed)")
	}
	return nil
}

func (s *scenario) operatorPods(ctx context.Context) ([]corev1.Pod, error) {
	pods, err := s.kube.CoreV1().Pods(s.opts.OperatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: operatorSelector})
	if err != nil {
		return nil, fmt.Errorf("cannot inspect operator pods (details suppressed)")
	}
	return pods.Items, nil
}

func (s *scenario) beginOperatorLogEpoch(t *testing.T, previous map[types.UID]int32) {
	t.Helper()
	pods, err := s.operatorPods(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := operatorSources(pods, s.opts.OperatorImage)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := operatorLogInstances(pods, s.opts.OperatorImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOperatorLogEpochStart(s.operatorEpochs, previous, instances, s.started); err != nil {
		t.Fatal(err)
	}
	s.operatorSources = sources
	s.operatorEpochs = append(s.operatorEpochs, operatorLogEpoch{Sources: sources, Instances: instances})
	for _, pod := range pods {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "manager" {
				t.Logf("operator epoch=%d image=%s imageID=%s", len(s.operatorEpochs), s.opts.OperatorImage, status.ImageID)
			}
		}
	}
}

func (s *scenario) checkOperatorLogSafety(t *testing.T) {
	t.Helper()
	pods, err := s.operatorPods(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := operatorSources(pods, s.opts.OperatorImage)
	if err != nil || !reflect.DeepEqual(sources, s.operatorSources) {
		t.Fatal("operator log safety check incomplete: unexpected log source change or container restart")
	}
	if len(s.operatorEpochs) == 0 {
		t.Fatal("operator log safety check has no recorded epoch")
	}
	instances, err := operatorLogInstances(pods, s.opts.OperatorImage)
	if err != nil || !reflect.DeepEqual(instances, s.operatorEpochs[len(s.operatorEpochs)-1].Instances) {
		t.Fatal("operator log safety check incomplete: controller container instance changed")
	}
	for _, epoch := range s.operatorEpochs[:len(s.operatorEpochs)-1] {
		if !epoch.Scanned || !epoch.Closed {
			t.Fatal("operator log safety check has an unverified prior epoch")
		}
	}
	for _, pod := range pods {
		// Keep the original start time, including when a new controller starts, so startup
		// output is inspected too. Closing an epoch only covers logs captured before shutdown.
		// Bound the client reader instead of PodLogOptions.LimitBytes: Kubernetes may
		// return fewer bytes than that server-side limit when it truncates the response.
		stream, err := s.kube.CoreV1().Pods(s.opts.OperatorNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "manager", SinceTime: &s.started}).Stream(s.ctx)
		if err != nil {
			t.Fatal("cannot inspect operator logs (details suppressed)")
		}
		err = inspectOperatorLogStream(stream, 8<<20, s.values)
		closeErr := stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal("cannot close operator log capture (details suppressed)")
		}
	}
	latestPods, err := s.operatorPods(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	latestSources, err := operatorSources(latestPods, s.opts.OperatorImage)
	if err != nil || !reflect.DeepEqual(latestSources, sources) {
		t.Fatal("operator log sources changed while their logs were being inspected")
	}
	latestInstances, err := operatorLogInstances(latestPods, s.opts.OperatorImage)
	if err != nil || !reflect.DeepEqual(latestInstances, instances) {
		t.Fatal("operator container instance changed while its logs were being inspected")
	}
	s.operatorEpochs[len(s.operatorEpochs)-1].Scanned = true
}

// deploymentForPods follows controller-owner UIDs instead of assuming a Helm release or
// Kustomize prefix. Refuse ambiguous installations and controllers owned by other workloads.
func (s *scenario) deploymentForPods(ctx context.Context, pods []corev1.Pod) (*appsv1.Deployment, error) {
	var deployment *appsv1.Deployment
	for _, pod := range pods {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "ReplicaSet" || owner.APIVersion != "apps/v1" {
			return nil, fmt.Errorf("operator pod is not controlled by a ReplicaSet")
		}
		rs, err := s.kube.AppsV1().ReplicaSets(s.opts.OperatorNamespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil || rs.UID != owner.UID {
			return nil, fmt.Errorf("cannot verify operator ReplicaSet owner identity")
		}
		owner = metav1.GetControllerOf(rs)
		if owner == nil || owner.Kind != "Deployment" || owner.APIVersion != "apps/v1" {
			return nil, fmt.Errorf("operator ReplicaSet is not controlled by a Deployment")
		}
		d, err := s.kube.AppsV1().Deployments(s.opts.OperatorNamespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil || d.UID != owner.UID {
			return nil, fmt.Errorf("cannot verify operator Deployment owner identity")
		}
		if deployment != nil && deployment.UID != d.UID {
			return nil, fmt.Errorf("operator selector matches more than one Deployment")
		}
		deployment = d
	}
	if deployment == nil {
		return nil, fmt.Errorf("operator Deployment is missing")
	}
	return deployment, nil
}

func deploymentPlan(d *appsv1.Deployment, image string, sources int) (*operatorDeployment, error) {
	if d.UID == "" || d.DeletionTimestamp != nil || d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || int(*d.Spec.Replicas) != sources {
		return nil, fmt.Errorf("operator Deployment must have a stable positive replica count")
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas != *d.Spec.Replicas || d.Status.ReadyReplicas != *d.Spec.Replicas || d.Status.Replicas != *d.Spec.Replicas {
		return nil, fmt.Errorf("operator Deployment rollout is incomplete")
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "manager" && c.Image == image {
			return &operatorDeployment{Namespace: d.Namespace, Name: d.Name, UID: d.UID, Replicas: *d.Spec.Replicas, Image: image}, nil
		}
	}
	return nil, fmt.Errorf("operator Deployment manager image does not match")
}

func (s *scenario) stopOperator(t *testing.T) *operatorDeployment {
	t.Helper()
	// Complete this scan while the old pods and logs still exist.
	s.checkPublishedSafety(t)
	pods, err := s.operatorPods(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := operatorSources(pods, s.opts.OperatorImage)
	if err != nil || !reflect.DeepEqual(sources, s.operatorSources) {
		t.Fatal("operator log sources changed between pre-stop scan and restart planning")
	}
	instances, err := operatorLogInstances(pods, s.opts.OperatorImage)
	if err != nil || !reflect.DeepEqual(instances, s.operatorEpochs[len(s.operatorEpochs)-1].Instances) {
		t.Fatal("operator container instance changed between pre-stop scan and restart planning")
	}
	d, err := s.deploymentForPods(s.ctx, pods)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deploymentPlan(d, s.opts.OperatorImage, len(s.operatorSources))
	if err != nil {
		t.Fatal(err)
	}
	// Register before mutating: a failed request may still have reached the API server.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		defer cancel()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := s.kube.AppsV1().Deployments(plan.Namespace).Get(ctx, plan.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if current.UID != plan.UID {
				return fmt.Errorf("operator Deployment identity changed")
			}
			if current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
				return nil
			}
			current.Spec.Replicas = &plan.Replicas
			_, err = s.kube.AppsV1().Deployments(plan.Namespace).Update(ctx, current, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			t.Errorf("cleanup: cannot restore stopped operator replicas (details suppressed)")
		}
	})
	zero := int32(0)
	d.Spec.Replicas = &zero
	if _, err := s.kube.AppsV1().Deployments(plan.Namespace).Update(s.ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal("cannot stop operator with original Deployment resourceVersion (details suppressed)")
	}
	err = wait.PollUntilContextTimeout(s.ctx, pollInterval, statusTimeout, true, func(ctx context.Context) (bool, error) {
		current, err := s.kube.AppsV1().Deployments(plan.Namespace).Get(ctx, plan.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if current.UID != plan.UID || current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
			return false, fmt.Errorf("operator Deployment changed during controlled stop")
		}
		pods, err := s.operatorPods(ctx)
		for _, pod := range pods {
			previous, exists := s.operatorSources[pod.UID]
			if !exists {
				return false, fmt.Errorf("unexpected controller pod appeared during controlled stop")
			}
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "manager" && status.RestartCount != previous {
					return false, fmt.Errorf("controller container restarted during controlled stop")
				}
				if status.Name == "manager" && status.ContainerID != "" && status.ContainerID != instances[pod.UID].ContainerID {
					return false, fmt.Errorf("controller container instance changed during controlled stop")
				}
			}
		}
		return err == nil && len(pods) == 0 && current.Status.ObservedGeneration >= current.Generation && current.Status.Replicas == 0, nil
	})
	if err != nil {
		t.Fatal("operator did not stop cleanly (details suppressed)")
	}
	s.operatorEpochs[len(s.operatorEpochs)-1].Closed = true
	t.Log("Operator stopped after its log safety scan; shutdown output after that scan requires retained cluster logs for complete audit evidence")
	return plan
}

func (s *scenario) startOperator(t *testing.T, plan *operatorDeployment, image string) {
	t.Helper()
	if image == "" || plan == nil || plan.Replicas < 1 {
		t.Fatal("operator start requires the captured Deployment and an explicit image")
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := s.kube.AppsV1().Deployments(plan.Namespace).Get(s.ctx, plan.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if d.UID != plan.UID || d.Spec.Replicas == nil || *d.Spec.Replicas != 0 || d.DeletionTimestamp != nil {
			return fmt.Errorf("operator Deployment is not the same stopped instance")
		}
		found := false
		for i := range d.Spec.Template.Spec.Containers {
			if d.Spec.Template.Spec.Containers[i].Name == "manager" {
				d.Spec.Template.Spec.Containers[i].Image, found = image, true
			}
		}
		if !found {
			return fmt.Errorf("operator manager container is missing")
		}
		d.Spec.Replicas = &plan.Replicas
		_, err = s.kube.AppsV1().Deployments(plan.Namespace).Update(s.ctx, d, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		t.Fatal("cannot start the stopped operator (details suppressed)")
	}
	startupInstances := map[types.UID]operatorLogInstance{}
	err = wait.PollUntilContextTimeout(s.ctx, pollInterval, statusTimeout, true, func(ctx context.Context) (bool, error) {
		d, err := s.kube.AppsV1().Deployments(plan.Namespace).Get(ctx, plan.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if d.UID != plan.UID || d.Spec.Replicas == nil || *d.Spec.Replicas != plan.Replicas {
			return false, fmt.Errorf("operator Deployment identity or replicas changed during startup")
		}
		pods, err := s.operatorPods(ctx)
		if err != nil {
			return false, nil
		}
		if err := observeOperatorStartup(pods, startupInstances, s.operatorSources, plan.Replicas); err != nil {
			return false, err
		}
		if _, err := operatorSources(pods, image); err != nil {
			return false, nil
		}
		if _, err := deploymentPlan(d, image, len(pods)); err != nil {
			return false, nil
		}
		owner, err := s.deploymentForPods(ctx, pods)
		return err == nil && owner.UID == plan.UID, nil
	})
	if err != nil {
		t.Fatal("operator startup readiness timed out (details suppressed)")
	}
	s.opts.OperatorImage = image
	s.beginOperatorLogEpoch(t, s.operatorSources)
	if !reflect.DeepEqual(startupInstances, s.operatorEpochs[len(s.operatorEpochs)-1].Instances) {
		t.Fatal("controller log instances changed between startup verification and epoch capture")
	}
}

func (s *scenario) restartScenarios(t *testing.T, f *fixture) {
	t.Helper()
	providers := []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure}
	before := map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus{}
	for _, provider := range providers {
		before[provider] = s.waitReady(t, provider)
	}
	children := s.children(t)
	operator := s.stopOperator(t)
	s.startOperator(t, operator, operator.Image)
	for _, provider := range providers {
		// Ready pods alone do not prove the restarted manager processed the CR. A new
		// Secret revision must appear in observedSecret while success remains unchanged.
		s.updateSecret(t, before[provider].ObservedSecret.Name, func(secret *corev1.Secret) {
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}
			secret.Annotations["e2e-restart"] = s.name
		})
		assertSameSuccess(t, before[provider], s.waitReady(t, provider))
	}
	s.assertChildrenStable(t, children)

	detached := map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus{}
	for _, provider := range providers {
		before[provider] = s.waitReady(t, provider)
		s.deleteSecret(t, before[provider].ObservedSecret.Name)
		detached[provider] = s.waitPhase(t, provider, ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply)
		assertSameSuccess(t, before[provider], detached[provider])
	}
	operator = s.stopOperator(t)
	s.startOperator(t, operator, operator.Image)
	// Region is informational, outside the credential binding/fingerprint. Changing it
	// advances generation and proves this restarted manager processed the detached CR.
	var originalRegion string
	s.updateCluster(t, func(c *ml.MarklogicCluster) {
		originalRegion = c.Spec.ObjectStorage.AWS.Region
		if originalRegion == "us-west-2" {
			c.Spec.ObjectStorage.AWS.Region = "us-east-1"
		} else {
			c.Spec.ObjectStorage.AWS.Region = "us-west-2"
		}
	})
	s.assertDetachedRestartState(t, detached)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.assertDetachedStableSnapshot(t, detached)
		select {
		case <-s.ctx.Done():
			t.Fatal("detached restart observation canceled")
		case <-time.After(pollInterval):
		}
	}
	s.updateCluster(t, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage.AWS.Region = originalRegion })
	s.assertDetachedRestartState(t, detached)
	for _, provider := range providers {
		values := f.AWS.Initial
		if provider == testutils.ObjectStorageAzure {
			values = f.Azure.Initial
		}
		s.createSecret(t, before[provider].ObservedSecret.Name, secretData(values))
		after := s.waitReady(t, provider)
		if before[provider].AppliedSecret.UID == after.AppliedSecret.UID || before[provider].AppliedFingerprint != after.AppliedFingerprint {
			t.Fatal("re-adoption after restart did not record a new Secret identity with unchanged material")
		}
	}
	s.assertChildrenStable(t, children)
}

// A stable-window assertion uses immediate snapshots; eventual waiters would hide a
// transient regression that recovered before their timeout.
func (s *scenario) assertDetachedStableSnapshot(t *testing.T, before map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus) {
	t.Helper()
	c := &ml.MarklogicCluster{}
	if s.client.Get(s.ctx, s.key(s.name), c) != nil || c.UID != s.clusterUID {
		t.Fatal("cannot read the same detached cluster during restart observation")
	}
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		after := providerStatus(c, provider)
		if after == nil || after.Phase != ml.ObjectStoragePhaseDetached || after.Reason != ml.ObjectStorageReasonSecretDeletedAfterApply || after.ObservedGeneration != c.Generation || after.DetachEligible == nil || !*after.DetachEligible {
			t.Fatal("detached status or eligibility changed during restart observation")
		}
		assertSameSuccess(t, before[provider], after)
		if !reflect.DeepEqual(before[provider].ObservedSecret, after.ObservedSecret) {
			t.Fatal("detached observed Secret identity/revision changed during restart observation")
		}
		if !apierrors.IsNotFound(s.client.Get(s.ctx, s.key(after.ObservedSecret.Name), &corev1.Secret{})) {
			t.Fatal("detached Secret absence changed during restart observation")
		}
		assertDetachedMessage(t, after.Message)
	}
}

func (s *scenario) assertDetachedRestartState(t *testing.T, before map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus) {
	t.Helper()
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		after := s.waitPhase(t, provider, ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply)
		assertSameSuccess(t, before[provider], after)
		if !reflect.DeepEqual(before[provider].ObservedSecret, after.ObservedSecret) {
			t.Fatal("detached restart changed retained observed Secret identity/revision")
		}
		assertDetachedMessage(t, after.Message)
	}
}
