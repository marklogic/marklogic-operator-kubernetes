package k8sutil

import (
	"context"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestImmutableMarklogicGroupSpecMismatch(t *testing.T) {
	t.Run("returns nil when isDynamic is unchanged", func(t *testing.T) {
		current := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: true},
		}
		desired := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: true},
		}

		if err := immutableMarklogicGroupSpecMismatch(current, desired); err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	})

	t.Run("returns actionable error when isDynamic changes", func(t *testing.T) {
		current := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: false},
		}
		desired := &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic", Namespace: "default"},
			Spec:       marklogicv1.MarklogicGroupSpec{IsDynamic: true},
		}

		err := immutableMarklogicGroupSpecMismatch(current, desired)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "cannot change isDynamic") {
			t.Fatalf("expected immutable field error, got %v", err)
		}
		if !strings.Contains(err.Error(), "delete the child MarklogicGroup") {
			t.Fatalf("expected actionable remediation in error, got %v", err)
		}
	})
}

func TestReadinessProbeFailsOnHTTPError(t *testing.T) {
	probe := getReadinessProbe(marklogicv1.ContainerProbe{})
	if probe.Exec == nil || len(probe.Exec.Command) < 3 {
		t.Fatal("readiness probe does not contain an exec command")
	}
	command := probe.Exec.Command[2]
	if !strings.Contains(command, "test -f /tmp/marklogic_ready") {
		t.Fatalf("readiness command %q does not gate on the wrapper ready marker", command)
	}
	if !strings.Contains(command, "curl -f ") {
		t.Fatalf("readiness command %q does not fail for HTTP error responses", command)
	}
	if !strings.Contains(command, `= "200"`) {
		t.Fatalf("readiness command %q does not require HTTP 200", command)
	}
}

func TestCredentialVolumesKeepAdminAndOperatorSecretsSeparate(t *testing.T) {
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "database",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "MarklogicCluster",
				Name: "search",
			}},
		},
		Spec: marklogicv1.MarklogicGroupSpec{
			Name:          "node",
			SecretName:    "search-admin",
			Auth:          &marklogicv1.AdminAuth{OperatorSecretName: stringPointer("search-operator")},
			HugePages:     &marklogicv1.HugePages{},
			LogCollection: &marklogicv1.LogCollection{},
		},
		Status: marklogicv1.MarklogicGroupStatus{CredentialSecretName: "search-operator"},
	}
	params := generateContainerParams(group)
	if params.SecretName != "search-admin" || params.OperatorSecretName != "search-operator" || !params.OperatorCredentialsActive {
		t.Fatalf("generated credential parameters = %+v, want separate admin/operator Secrets with active operator credentials", params)
	}

	volumes := generateVolumes("node", params)
	secretNames := map[string]string{}
	for _, volume := range volumes {
		if volume.Secret != nil {
			secretNames[volume.Name] = volume.Secret.SecretName
		}
	}
	if secretNames["mladmin-secrets"] != "search-admin" || secretNames["mloperator-secrets"] != "search-operator" {
		t.Fatalf("credential Secret volumes = %v, want admin=search-admin and operator=search-operator", secretNames)
	}
	for _, volume := range volumes {
		if volume.Name == "mloperator-secrets" && (volume.Secret.Optional == nil || !*volume.Secret.Optional) {
			t.Fatal("operator credential Secret volume must be optional while bootstrap uses admin credentials")
		}
	}
}

func TestMissingActiveOperatorSecretDoesNotFallBackToAdminSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := marklogicv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Marklogic scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	group := &marklogicv1.MarklogicGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "database"},
		Spec: marklogicv1.MarklogicGroupSpec{
			SecretName:    "search-admin",
			Auth:          &marklogicv1.AdminAuth{OperatorSecretName: stringPointer("new-operator")},
			HugePages:     &marklogicv1.HugePages{},
			LogCollection: &marklogicv1.LogCollection{},
		},
		Status: marklogicv1.MarklogicGroupStatus{CredentialSecretName: "deleted-operator"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&marklogicv1.MarklogicGroup{}).WithObjects(group).Build()
	oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group}
	params := generateContainerParams(group)

	_, err := oc.getStatefulSetCredentialSecret(&params)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get StatefulSet credential Secret error = %v, want NotFound", err)
	}
	updatedGroup := &marklogicv1.MarklogicGroup{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(group), updatedGroup); err != nil {
		t.Fatalf("get updated MarklogicGroup: %v", err)
	}
	if updatedGroup.Status.CredentialSecretName != "deleted-operator" {
		t.Fatalf("active credential Secret status = %q, want unchanged", updatedGroup.Status.CredentialSecretName)
	}
}

func TestManualCredentialRotationOnlyRunsForOnDelete(t *testing.T) {
	for _, strategy := range []appsv1.StatefulSetUpdateStrategyType{
		appsv1.RollingUpdateStatefulSetStrategyType,
		appsv1.OnDeleteStatefulSetStrategyType,
	} {
		t.Run(string(strategy), func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("add core scheme: %v", err)
			}
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add apps scheme: %v", err)
			}
			replicas := int32(1)
			group := &marklogicv1.MarklogicGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "database"},
				Spec:       marklogicv1.MarklogicGroupSpec{Name: "node", Replicas: &replicas, UpdateStrategy: strategy},
				Status:     marklogicv1.MarklogicGroupStatus{CredentialSecretName: "node-operator"},
			}
			labels := getSelectorLabelsByComponent("node", false)
			statefulSet := credentialRotationStatefulSet("node", "database", "new", replicas)
			pod := credentialRevisionPod("node-0", labels, "old")
			pod.Namespace = "database"
			setCredentialRevisionPodOwner(pod, statefulSet)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(statefulSet, pod).Build()
			oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group}

			rotated, err := oc.rotateCredentialPodsIfNeeded()
			if err != nil {
				t.Fatalf("rotate credential pods: %v", err)
			}
			wantRotation := strategy == appsv1.OnDeleteStatefulSetStrategyType
			if rotated != wantRotation {
				t.Fatalf("rotated=%t, want %t", rotated, wantRotation)
			}
			pods := &corev1.PodList{}
			if err := fakeClient.List(context.Background(), pods); err != nil {
				t.Fatalf("list pods: %v", err)
			}
			wantPods := 1
			if wantRotation {
				wantPods = 0
			}
			if len(pods.Items) != wantPods {
				t.Fatalf("pod count=%d, want %d", len(pods.Items), wantPods)
			}
		})
	}
}

func TestRotateCredentialPodsDeletesOnlyOneStalePod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	labels := getSelectorLabelsByComponent("group", false)
	desired := credentialRotationStatefulSet("group", "default", "new", 2)
	pods := []*corev1.Pod{
		credentialRevisionPod("group-0", labels, "old"),
		credentialRevisionPod("group-1", labels, "old"),
	}
	setCredentialRevisionPodOwner(pods[0], desired)
	setCredentialRevisionPodOwner(pods[1], desired)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desired, pods[0], pods[1]).Build()
	oc := &OperatorContext{
		Ctx: context.Background(), Client: fakeClient,
		MarklogicGroup: &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}},
	}

	rotated, err := oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"})
	if err != nil || !rotated {
		t.Fatalf("rotateCredentialPods() = (%v, %v), want (true, nil)", rotated, err)
	}
	remaining := &corev1.PodList{}
	if err := fakeClient.List(context.Background(), remaining, client.InNamespace("default")); err != nil {
		t.Fatalf("list remaining pods: %v", err)
	}
	if len(remaining.Items) != 1 {
		t.Fatalf("remaining pods = %d, want exactly one stale pod left for the next rotation", len(remaining.Items))
	}
	if remaining.Items[0].Name != "group-0" {
		t.Fatalf("remaining pod = %q, want deterministic highest-ordinal deletion", remaining.Items[0].Name)
	}
}

func TestRotateCredentialPodsWaitsForTerminatingPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	labels := getSelectorLabelsByComponent("group", true)
	desired := credentialRotationStatefulSet("group", "default", "new", 2)
	terminating := credentialRevisionPod("group-0", labels, "old")
	setCredentialRevisionPodOwner(terminating, desired)
	terminating.Finalizers = []string{dynamicHostCleanupFinalizer}
	deletionTimestamp := metav1.Now()
	terminating.DeletionTimestamp = &deletionTimestamp
	stalePeer := credentialRevisionPod("group-1", labels, "old")
	setCredentialRevisionPodOwner(stalePeer, desired)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desired, terminating, stalePeer).Build()
	oc := &OperatorContext{
		Ctx: context.Background(), Client: fakeClient,
		MarklogicGroup: &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}},
	}
	rotated, err := oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group", IsDynamic: true})
	if err != nil || !rotated {
		t.Fatalf("rotateCredentialPods() = (%v, %v), want rotation in progress", rotated, err)
	}
	currentPeer := &corev1.Pod{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stalePeer), currentPeer); err != nil {
		t.Fatalf("get stale peer: %v", err)
	}
	if currentPeer.DeletionTimestamp != nil {
		t.Fatal("expected stale peer to remain untouched while another pod is terminating")
	}
}

func TestRotateCredentialPodsWaitsForVerifiedReplacement(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	labels := getSelectorLabelsByComponent("group", false)
	desired := credentialRotationStatefulSet("group", "default", "new", 2)
	first := credentialRevisionPod("group-0", labels, "old")
	second := credentialRevisionPod("group-1", labels, "old")
	setCredentialRevisionPodOwner(first, desired)
	setCredentialRevisionPodOwner(second, desired)
	deletedUID := second.UID
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desired, first, second).Build()
	oc := &OperatorContext{
		Ctx: context.Background(), Client: fakeClient,
		MarklogicGroup: &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}},
	}

	rotated, err := oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"})
	if err != nil || !rotated {
		t.Fatalf("initial rotation = (%v, %v), want deletion in progress", rotated, err)
	}
	for reconcileCount := 0; reconcileCount < 2; reconcileCount++ {
		rotated, err = oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"})
		if err != nil || !rotated {
			t.Fatalf("missing replacement reconcile %d = (%v, %v), want rotation in progress", reconcileCount+1, rotated, err)
		}
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(first), &corev1.Pod{}); err != nil {
		t.Fatalf("expected lower ordinal pod to remain while replacement is missing: %v", err)
	}

	replacement := credentialRevisionPod("group-1", labels, "new")
	setCredentialRevisionPodOwner(replacement, desired)
	replacement.UID = deletedUID
	if err := fakeClient.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create same-UID replacement: %v", err)
	}
	if rotated, err = oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"}); err != nil || !rotated {
		t.Fatalf("same-UID replacement reconcile = (%v, %v), want rotation paused", rotated, err)
	}
	if err := fakeClient.Delete(context.Background(), replacement); err != nil {
		t.Fatalf("delete same-UID replacement: %v", err)
	}

	replacement = credentialRevisionPod("group-1", labels, "old")
	setCredentialRevisionPodOwner(replacement, desired)
	replacement.UID = types.UID("group-1-replacement")
	if err := fakeClient.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create stale-revision replacement: %v", err)
	}
	if rotated, err = oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"}); err != nil || !rotated {
		t.Fatalf("stale-revision replacement reconcile = (%v, %v), want rotation paused", rotated, err)
	}
	if err := fakeClient.Delete(context.Background(), replacement); err != nil {
		t.Fatalf("delete stale-revision replacement: %v", err)
	}

	replacement = credentialRevisionPod("group-1", labels, "new")
	setCredentialRevisionPodOwner(replacement, desired)
	replacement.UID = types.UID("group-1-replacement")
	if err := fakeClient.Create(context.Background(), replacement); err != nil {
		t.Fatalf("create verified replacement: %v", err)
	}
	if rotated, err = oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"}); err != nil || !rotated {
		t.Fatalf("verified replacement reconcile = (%v, %v), want tracker completion", rotated, err)
	}
	if rotated, err = oc.rotateCredentialPods(desired, statefulSetParameters{Name: "group"}); err != nil || !rotated {
		t.Fatalf("next ordinal rotation = (%v, %v), want deletion in progress", rotated, err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(first), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected lower ordinal pod deletion after verified replacement, got err=%v", err)
	}
}

func credentialRotationStatefulSet(name, namespace, revision string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-statefulset")},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				credentialRevisionAnnotation: revision,
			}}},
		},
	}
}

func setCredentialRevisionPodOwner(pod *corev1.Pod, statefulSet *appsv1.StatefulSet) {
	pod.UID = types.UID(pod.Name + "-original")
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(statefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
}

func credentialRevisionPod(name string, labels map[string]string, revision string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: labels,
			Annotations: map[string]string{credentialRevisionAnnotation: revision},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}
