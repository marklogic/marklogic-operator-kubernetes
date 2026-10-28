package utils

import (
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVACOptionsFromEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, key, value string
		wantError        bool
	}{
		{name: "defaults"},
		{name: "custom timeout", key: "E2E_VAC_TIMEOUT", value: "45m"},
		{name: "invalid timeout", key: "E2E_VAC_TIMEOUT", value: "invalid", wantError: true},
		{name: "negative timeout", key: "E2E_VAC_TIMEOUT", value: "-1m", wantError: true},
		{name: "missing storage class", key: "E2E_VAC_STORAGE_CLASS", wantError: true},
		{name: "identical classes", key: "E2E_VAC_PERFORMANCE_CLASS", value: "standard", wantError: true},
		{name: "invalid size", key: "E2E_VAC_SIZE", value: "invalid", wantError: true},
		{name: "zero size", key: "E2E_VAC_SIZE", value: "0", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("E2E_VAC_STORAGE_CLASS", "gp3")
			t.Setenv("E2E_VAC_STANDARD_CLASS", "standard")
			t.Setenv("E2E_VAC_PERFORMANCE_CLASS", "performance")
			t.Setenv("E2E_VAC_SIZE", "")
			t.Setenv("E2E_VAC_TIMEOUT", "")
			if test.key != "" {
				t.Setenv(test.key, test.value)
			}
			options, err := VACOptionsFromEnvironment("vac-test", "marklogic:test", "admin", "test-password")
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %v", err, test.wantError)
			}
			if !test.wantError && test.key == "" && (options.Size != "20Gi" || options.Timeout != 15*time.Minute) {
				t.Fatalf("unexpected defaults: size=%s timeout=%s", options.Size, options.Timeout)
			}
		})
	}
}

func TestVACProgressRequiresCurrentGeneration(t *testing.T) {
	for _, test := range []struct {
		name                string
		progressGeneration  int64
		conditionGeneration int64
		state               marklogicv1.VolumeAttributesClassState
		conditionStatus     metav1.ConditionStatus
		wantError           bool
	}{
		{"current success", 2, 2, marklogicv1.VolumeAttributesClassCompleted, metav1.ConditionTrue, false},
		{"stale progress", 1, 2, marklogicv1.VolumeAttributesClassCompleted, metav1.ConditionTrue, true},
		{"stale condition", 2, 1, marklogicv1.VolumeAttributesClassCompleted, metav1.ConditionTrue, true},
		{"pending", 2, 2, marklogicv1.VolumeAttributesClassPending, metav1.ConditionTrue, true},
		{"failed condition", 2, 2, marklogicv1.VolumeAttributesClassCompleted, metav1.ConditionFalse, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := &marklogicv1.StorageStatus{VolumeAttributesClass: &marklogicv1.VolumeAttributesClassStatus{ObservedGeneration: test.progressGeneration, State: test.state}}
			conditions := []metav1.Condition{{Type: "StorageAttributesSynced", ObservedGeneration: test.conditionGeneration, Status: test.conditionStatus}}
			if err := vacProgressCurrent(2, storage, conditions); (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %v", err, test.wantError)
			}
		})
	}
	if err := vacProgressCurrent(2, nil, nil); err == nil {
		t.Fatal("missing progress was accepted as completion")
	}
}

func TestVACSnapshotsStable(t *testing.T) {
	makeSnapshot := func() vacSnapshot {
		return vacSnapshot{
			set: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{UID: "set"}},
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "marklogic-server"}}}},
			claims: map[string]*corev1.PersistentVolumeClaim{"datadir": {
				ObjectMeta: metav1.ObjectMeta{UID: "claim"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "volume"},
				Status: corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}},
			}},
			volumes: map[string]*corev1.PersistentVolume{"datadir": {ObjectMeta: metav1.ObjectMeta{UID: "volume"}}},
		}
	}
	for _, test := range []struct {
		name      string
		mutate    func(*vacSnapshot)
		untouched bool
		wantError bool
	}{
		{name: "unchanged", mutate: func(*vacSnapshot) {}},
		{name: "operation token allowed", mutate: func(snapshot *vacSnapshot) {
			snapshot.claims["datadir"].Annotations = map[string]string{"marklogic.progress.com/storage-operation-id": "operation"}
		}},
		{name: "VAC change allowed", mutate: func(snapshot *vacSnapshot) {
			value := "performance"
			snapshot.claims["datadir"].Spec.VolumeAttributesClassName = &value
		}},
		{name: "untargeted VAC change rejected", untouched: true, wantError: true, mutate: func(snapshot *vacSnapshot) {
			value := "performance"
			snapshot.claims["datadir"].Spec.VolumeAttributesClassName = &value
		}},
		{name: "Pod replacement", wantError: true, mutate: func(snapshot *vacSnapshot) { snapshot.pod.UID = "replacement" }},
		{name: "container restart", wantError: true, mutate: func(snapshot *vacSnapshot) { snapshot.pod.Status.ContainerStatuses[0].RestartCount++ }},
		{name: "claim replacement", wantError: true, mutate: func(snapshot *vacSnapshot) { snapshot.claims["datadir"].UID = "replacement" }},
		{name: "capacity change", wantError: true, mutate: func(snapshot *vacSnapshot) {
			snapshot.claims["datadir"].Status.Capacity[corev1.ResourceStorage] = resource.MustParse("3Gi")
		}},
		{name: "unrelated annotation", wantError: true, mutate: func(snapshot *vacSnapshot) {
			snapshot.claims["datadir"].Annotations = map[string]string{"unexpected": "change"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, after := makeSnapshot(), makeSnapshot()
			test.mutate(&after)
			var untouched []string
			if test.untouched {
				untouched = []string{"datadir"}
			}
			if err := vacSnapshotsStable(before, after, untouched...); (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %v", err, test.wantError)
			}
		})
	}
}

func TestVACCoreFeatures(t *testing.T) {
	features := VACCoreFeatures(VACOptions{})
	if len(features) != 11 {
		t.Fatalf("got %d VAC scenarios, expected 11", len(features))
	}
	names := map[string]bool{}
	for _, feature := range features {
		if names[feature.Name()] {
			t.Fatalf("duplicate VAC scenario: %s", feature.Name())
		}
		names[feature.Name()] = true
	}
}
