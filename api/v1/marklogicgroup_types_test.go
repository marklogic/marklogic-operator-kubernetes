package v1

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMarklogicGroupPreservesDisabledXdqpSSLOnWire(t *testing.T) {
	group := &MarklogicGroup{Spec: MarklogicGroupSpec{
		GroupConfig: &GroupConfig{Name: "dnode", EnableXdqpSsl: false},
	}}
	payload, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Spec struct {
			GroupConfig map[string]json.RawMessage `json:"groupConfig"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	value, present := wire.Spec.GroupConfig["enableXdqpSsl"]
	if !present || string(value) != "false" {
		t.Fatalf("explicit XDQP SSL disable must reach the API server as false, got %s (present=%t); omission re-enables the CRD default", value, present)
	}
}

func TestMarklogicGroupDeepCopyVolumeResizeStatus(t *testing.T) {
	now := metav1.NewTime(time.Now())

	group := &MarklogicGroup{
		Spec: MarklogicGroupSpec{
			Persistence: &Persistence{
				Enabled:        true,
				Size:           "20Gi",
				ResizeStrategy: VolumeResizeStrategySequential,
			},
		},
		Status: MarklogicGroupStatus{
			VolumeResizeStatus: &VolumeResizeStatus{
				OperationID:        "op-1",
				ObservedGeneration: 7,
				Phase:              VolumeResizePhaseWaitingForPVCResize,
				Reason:             VolumeResizeReasonPVCNotBound,
				CurrentSize:        "20Gi",
				TargetSize:         "50Gi",
				ResizeStrategy:     VolumeResizeStrategySequential,
				TotalPVCs:          2,
				PVCsCheckpointed:   1,
				PVCStatuses: []PVCResizeStatus{
					{
						Name:               "data-0",
						PodName:            "pod-0",
						RequestedSize:      "50Gi",
						ObservedCapacity:   "20Gi",
						State:              PVCResizeStateWaitingForCheckpoint,
						CheckpointType:     PVCResizeCheckpointTypeOfflinePending,
						RestartRequired:    true,
						LastReason:         "PVCNotBound",
						LastMessage:        "waiting for pvc to bind",
						LastTransitionTime: &now,
					},
				},
				FailedPVCs: []FailedPVCStatus{
					{Name: "data-1", Reason: "ResizeFailed", Message: "api rejected resize"},
				},
				Markers:            []string{"pr4.sync.started"},
				Warnings:           []string{"storage provider delay"},
				LastTransitionTime: &now,
			},
		},
	}

	copied := group.DeepCopy()
	if copied == group {
		t.Fatalf("expected DeepCopy to return a new instance")
	}

	if copied.Spec.Persistence == group.Spec.Persistence {
		t.Fatalf("expected persistence to be deeply copied")
	}

	if copied.Status.VolumeResizeStatus == group.Status.VolumeResizeStatus {
		t.Fatalf("expected volume resize status to be deeply copied")
	}

	if copied.Status.VolumeResizeStatus.LastTransitionTime == group.Status.VolumeResizeStatus.LastTransitionTime {
		t.Fatalf("expected resize timestamps to be deeply copied")
	}

	group.Spec.Persistence.ResizeStrategy = VolumeResizeStrategyParallel
	group.Status.VolumeResizeStatus.PVCStatuses[0].Name = "data-modified"
	group.Status.VolumeResizeStatus.FailedPVCs[0].Reason = "updated"
	group.Status.VolumeResizeStatus.Markers[0] = "updated marker"
	group.Status.VolumeResizeStatus.Warnings[0] = "updated warning"

	if copied.Spec.Persistence.ResizeStrategy != VolumeResizeStrategySequential {
		t.Fatalf("unexpected copied resize strategy: %s", copied.Spec.Persistence.ResizeStrategy)
	}

	if copied.Status.VolumeResizeStatus.PVCStatuses[0].Name != "data-0" {
		t.Fatalf("unexpected copied pvc status name: %s", copied.Status.VolumeResizeStatus.PVCStatuses[0].Name)
	}

	if copied.Status.VolumeResizeStatus.FailedPVCs[0].Reason != "ResizeFailed" {
		t.Fatalf("unexpected copied failed pvc reason: %s", copied.Status.VolumeResizeStatus.FailedPVCs[0].Reason)
	}

	if copied.Status.VolumeResizeStatus.Markers[0] != "pr4.sync.started" {
		t.Fatalf("unexpected copied marker: %s", copied.Status.VolumeResizeStatus.Markers[0])
	}

	if copied.Status.VolumeResizeStatus.Warnings[0] != "storage provider delay" {
		t.Fatalf("unexpected copied warning: %s", copied.Status.VolumeResizeStatus.Warnings[0])
	}
}
