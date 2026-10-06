package k8sutil

import (
	"context"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestMissingActiveOperatorSecretFallsBackToAdminSecret(t *testing.T) {
	scheme := runtime.NewScheme()
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
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "search-admin", Namespace: "database"},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("admin-password")},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(adminSecret).Build()
	oc := &OperatorContext{Ctx: context.Background(), Client: fakeClient, MarklogicGroup: group}
	params := generateContainerParams(group)

	secret, err := oc.getStatefulSetCredentialSecret(&params)
	if err != nil {
		t.Fatalf("get StatefulSet credential Secret: %v", err)
	}
	if secret.Name != "search-admin" || params.OperatorCredentialsActive {
		t.Fatalf("fallback Secret=%q, operator credentials active=%t; want admin Secret and inactive operator credentials", secret.Name, params.OperatorCredentialsActive)
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
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "database"},
				Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"marklogic.progress.com/credential-revision": "new"}},
				}},
			}
			pod := credentialRevisionPod("node-0", labels, "old")
			pod.Namespace = "database"
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
	labels := getSelectorLabelsByComponent("group", false)
	pods := []*corev1.Pod{
		credentialRevisionPod("group-0", labels, "old"),
		credentialRevisionPod("group-1", labels, "old"),
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pods[0], pods[1]).Build()
	oc := &OperatorContext{
		Ctx: context.Background(), Client: fakeClient,
		MarklogicGroup: &marklogicv1.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}},
	}
	desired := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"marklogic.progress.com/credential-revision": "new"}},
	}}}

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
}

func credentialRevisionPod(name string, labels map[string]string, revision string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: labels,
			Annotations: map[string]string{"marklogic.progress.com/credential-revision": revision},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}
