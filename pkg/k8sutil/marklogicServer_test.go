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
