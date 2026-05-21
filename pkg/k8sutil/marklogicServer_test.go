package k8sutil

import (
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
