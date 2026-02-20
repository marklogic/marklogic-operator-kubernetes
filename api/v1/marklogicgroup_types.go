/*
Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MarklogicGroupSpec defines the desired state of MarklogicGroup
type MarklogicGroupSpec struct {
	// +kubebuilder:default:=1
	Replicas    *int32            `json:"replicas,omitempty"`
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// +kubebuilder:default:="cluster.local"
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// +kubebuilder:default:="progressofficial/marklogic-db:12.0.0-ubi9-rootless-2.2.2"
	Image string `json:"image"`
	// +kubebuilder:default:="IfNotPresent"
	ImagePullPolicy    string                        `json:"imagePullPolicy,omitempty"`
	ImagePullSecrets   []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	Auth               *AdminAuth                    `json:"auth,omitempty"`
	ServiceAccountName string                        `json:"serviceAccountName,omitempty"`
	// +kubebuilder:default:=false
	AutomountServiceAccountToken  *bool                        `json:"automountServiceAccountToken,omitempty"`
	Persistence                   *Persistence                 `json:"persistence,omitempty"`
	Resources                     *corev1.ResourceRequirements `json:"resources,omitempty"`
	TerminationGracePeriodSeconds *int64                       `json:"terminationGracePeriodSeconds,omitempty"`
	// +kubebuilder:validation:Enum=OnDelete;RollingUpdate
	// +kubebuilder:default:="OnDelete"
	UpdateStrategy appsv1.StatefulSetUpdateStrategyType `json:"updateStrategy,omitempty"`
	NetworkPolicy  NetworkPolicy                        `json:"networkPolicy,omitempty"`
	// +kubebuilder:default:={fsGroup: 2, fsGroupChangePolicy: "OnRootMismatch"}
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	// +kubebuilder:default:={runAsUser: 1000, runAsNonRoot: true, allowPrivilegeEscalation: false}
	ContainerSecurityContext  *corev1.SecurityContext           `json:"securityContext,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	PriorityClassName         string                            `json:"priorityClassName,omitempty"`
	// +kubebuilder:default:={enabled: false, mountPath: "/dev/hugepages"}
	HugePages *HugePages `json:"hugePages,omitempty"`
	// +kubebuilder:default:={enabled: true, initialDelaySeconds: 30, timeoutSeconds: 5, periodSeconds: 30, successThreshold: 1, failureThreshold: 3}
	LivenessProbe ContainerProbe `json:"livenessProbe,omitempty"`
	// +kubebuilder:default:={enabled: false, initialDelaySeconds: 10, timeoutSeconds: 5, periodSeconds: 30, successThreshold: 1, failureThreshold: 3}
	ReadinessProbe ContainerProbe `json:"readinessProbe,omitempty"`
	// +kubebuilder:default:={enabled: false, image: "fluent/fluent-bit:4.1.1", resources: {requests: {cpu: "100m", memory: "200Mi"}, limits: {cpu: "200m", memory: "500Mi"}}, files: {errorLogs: true, accessLogs: true, requestLogs: true}, outputs: "stdout"}
	LogCollection *LogCollection `json:"logCollection,omitempty"`
	// +kubebuilder:default:={name: "Default", enableXdqpSsl: true}
	GroupConfig                    *GroupConfig                    `json:"groupConfig,omitempty"`
	License                        *License                        `json:"license,omitempty"`
	EnableConverters               bool                            `json:"enableConverters,omitempty"`
	BootstrapHost                  string                          `json:"bootstrapHost,omitempty"`
	DoNotDelete                    *bool                           `json:"doNotDelete,omitempty"`
	Service                        Service                         `json:"service,omitempty"`
	PathBasedRouting               bool                            `json:"pathBasedRouting,omitempty"`
	AdditionalVolumes              *[]corev1.Volume                `json:"additionalVolumes,omitempty"`
	AdditionalVolumeMounts         *[]corev1.VolumeMount           `json:"additionalVolumeMounts,omitempty"`
	AdditionalVolumeClaimTemplates *[]corev1.PersistentVolumeClaim `json:"additionalVolumeClaimTemplates,omitempty"`
	SecretName                     string                          `json:"secretName,omitempty"`
	Tls                            *Tls                            `json:"tls,omitempty"`
}

// InternalState defines the observed state of MarklogicGroup
type InternalState string

// MarklogicGroupStatus defines the observed state of MarklogicGroup
type MarklogicGroupStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Conditions    []metav1.Condition       `json:"conditions,omitempty"`
	Stage         string                   `json:"stage,omitempty"`
	MarkLogicPods []corev1.ObjectReference `json:"active,omitempty"`

	// +optional
	MarklogicGroupStatus InternalState `json:"markLogicGroupStatus,omitempty"`

	// VolumeResizeStatus holds the status of ongoing volume resize operation
	// +optional
	VolumeResizeStatus *VolumeResizeStatus `json:"volumeResizeStatus,omitempty"`
}

// VolumeResizePhase represents the current phase of volume resize operation
type VolumeResizePhase string

const (
	// VolumeResizePhaseNone indicates no resize operation is in progress
	VolumeResizePhaseNone VolumeResizePhase = ""
	// VolumeResizePhaseValidating indicates resize prerequisites are being validated
	VolumeResizePhaseValidating VolumeResizePhase = "Validating"
	// VolumeResizePhaseResizingPVCs indicates PVCs are being resized
	VolumeResizePhaseResizingPVCs VolumeResizePhase = "ResizingPVCs"
	// VolumeResizePhaseWaitingForPVCResize indicates waiting for PVC resize to complete
	VolumeResizePhaseWaitingForPVCResize VolumeResizePhase = "WaitingForPVCResize"
	// VolumeResizePhaseVerifyingHealth indicates pod and MarkLogic health is being verified
	VolumeResizePhaseVerifyingHealth VolumeResizePhase = "VerifyingHealth"
	// VolumeResizePhaseBackingUpStatefulSet indicates StatefulSet spec is being backed up
	VolumeResizePhaseBackingUpStatefulSet VolumeResizePhase = "BackingUpStatefulSet"
	// VolumeResizePhaseDeletingStatefulSet indicates StatefulSet is being deleted with orphan policy
	VolumeResizePhaseDeletingStatefulSet VolumeResizePhase = "DeletingStatefulSet"
	// VolumeResizePhaseRecreatingStatefulSet indicates StatefulSet is being recreated
	VolumeResizePhaseRecreatingStatefulSet VolumeResizePhase = "RecreatingStatefulSet"
	// VolumeResizePhaseVerifyingPodsRunning indicates pods are being verified as running after StatefulSet recreation
	VolumeResizePhaseVerifyingPodsRunning VolumeResizePhase = "VerifyingPodsRunning"
	// VolumeResizePhaseRestartingPods indicates pods are being restarted for filesystem resize
	VolumeResizePhaseRestartingPods VolumeResizePhase = "RestartingPods"
	// VolumeResizePhaseCompleted indicates resize operation completed successfully
	VolumeResizePhaseCompleted VolumeResizePhase = "Completed"
	// VolumeResizePhaseStalled indicates resize operation is temporarily blocked
	VolumeResizePhaseStalled VolumeResizePhase = "Stalled"
	// VolumeResizePhaseFailed indicates resize operation failed permanently
	VolumeResizePhaseFailed VolumeResizePhase = "Failed"
)

// VolumeResizeReason provides specific reasons for stalled or failed states
type VolumeResizeReason string

const (
	// ResizeReasonNone indicates no specific reason
	ResizeReasonNone VolumeResizeReason = ""
	// ResizeReasonResizeFailed indicates total failure (no PVCs resized)
	ResizeReasonResizeFailed VolumeResizeReason = "ResizeFailed"
	// ResizeReasonPartialResizeFailure indicates some PVCs failed to resize
	ResizeReasonPartialResizeFailure VolumeResizeReason = "PartialResizeFailure"
	// ResizeReasonResizeRateLimited indicates API rate limiting
	ResizeReasonResizeRateLimited VolumeResizeReason = "ResizeRateLimited"
	// ResizeReasonStorageQuotaExceeded indicates storage quota limit hit
	ResizeReasonStorageQuotaExceeded VolumeResizeReason = "StorageQuotaExceeded"
	// ResizeReasonResizeForbidden indicates RBAC/permission issue
	ResizeReasonResizeForbidden VolumeResizeReason = "ResizeForbidden"
	// ResizeReasonInvalidResizeRequest indicates invalid size specification
	ResizeReasonInvalidResizeRequest VolumeResizeReason = "InvalidResizeRequest"
	// ResizeReasonStorageClassNotExpandable indicates StorageClass doesn't allow expansion
	ResizeReasonStorageClassNotExpandable VolumeResizeReason = "StorageClassNotExpandable"
	// ResizeReasonEBSCooldownPeriod indicates AWS EBS 6-hour modification limit
	ResizeReasonEBSCooldownPeriod VolumeResizeReason = "EBSCooldownPeriod"
	// ResizeReasonEBSModificationLimit indicates AWS EBS 4 modifications in 24-hour rolling window limit
	ResizeReasonEBSModificationLimit VolumeResizeReason = "EBSModificationLimit"
	// ResizeReasonMarkLogicHealthCheckFailed indicates MarkLogic health check failed
	ResizeReasonMarkLogicHealthCheckFailed VolumeResizeReason = "MarkLogicHealthCheckFailed"
	// ResizeReasonTimeout indicates operation timed out
	ResizeReasonTimeout VolumeResizeReason = "Timeout"
	// ResizeReasonShrinkNotSupported indicates shrinking volumes is not supported
	ResizeReasonShrinkNotSupported VolumeResizeReason = "ShrinkNotSupported"
	// ResizeReasonNoResizeNeeded indicates target size matches current size
	ResizeReasonNoResizeNeeded VolumeResizeReason = "NoResizeNeeded"
	// ResizeReasonPVCNotBound indicates PVC is not in Bound phase
	ResizeReasonPVCNotBound VolumeResizeReason = "PVCNotBound"
	// ResizeReasonMultipleVolumeClaimTemplates indicates STS has multiple VCTs requiring explicit PVC name
	ResizeReasonMultipleVolumeClaimTemplates VolumeResizeReason = "MultipleVolumeClaimTemplates"
	// ResizeReasonStatefulSetNotFound indicates StatefulSet not found for PVC
	ResizeReasonStatefulSetNotFound VolumeResizeReason = "StatefulSetNotFound"
	// ResizeReasonConcurrentResize indicates another resize is in progress
	ResizeReasonConcurrentResize VolumeResizeReason = "ConcurrentResize"
	// ResizeReasonStatefulSetDeleteFailed indicates orphan delete of StatefulSet failed
	ResizeReasonStatefulSetDeleteFailed VolumeResizeReason = "StatefulSetDeleteFailed"
	// ResizeReasonStatefulSetRecreateFailed indicates StatefulSet recreation failed with orphaned pods
	ResizeReasonStatefulSetRecreateFailed VolumeResizeReason = "StatefulSetRecreateFailed"
	// ResizeReasonPodSchedulingFailed indicates pods stuck in pending after restart
	ResizeReasonPodSchedulingFailed VolumeResizeReason = "PodSchedulingFailed"
	// ResizeReasonTemplateUpdateInterrupted indicates operator crashed during STS delete/recreate
	ResizeReasonTemplateUpdateInterrupted VolumeResizeReason = "TemplateUpdateInterrupted"
)

// FailedPVCInfo contains details about a PVC that failed to resize
type FailedPVCInfo struct {
	// Name is the name of the failed PVC
	Name string `json:"name"`
	// Reason is the reason code for the failure
	Reason VolumeResizeReason `json:"reason"`
	// Message provides detailed error information
	Message string `json:"message"`
}

// ResizeProgress tracks the overall progress of the resize operation
type ResizeProgress struct {
	// Phase is the current phase of the volume resize operation
	Phase VolumeResizePhase `json:"phase,omitempty"`

	// StartTime is when the resize operation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the resize operation completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// CurrentSize is the current/original size of the volumes
	CurrentSize string `json:"currentSize,omitempty"`

	// TargetSize is the desired size for the volumes
	TargetSize string `json:"targetSize,omitempty"`

	// OriginalSize is the size before resize (alias for CurrentSize, kept for compatibility)
	OriginalSize string `json:"originalSize,omitempty"`

	// PVCsResized is the number of PVCs that have been resized
	PVCsResized int32 `json:"pvcsResized,omitempty"`

	// TotalPVCs is the total number of PVCs to resize
	TotalPVCs int32 `json:"totalPvcs,omitempty"`

	// CurrentPVCIndex tracks which PVC is being processed in sequential mode
	// +optional
	CurrentPVCIndex int32 `json:"currentPVCIndex,omitempty"`

	// LastResizedPodIndex tracks which pod was last restarted during filesystem resize
	LastResizedPodIndex int32 `json:"lastResizedPodIndex,omitempty"`
}

// ResizeRetryInfo tracks retry and error information for the resize operation
type ResizeRetryInfo struct {
	// Reason provides specific reason for stalled or failed states
	// +optional
	Reason VolumeResizeReason `json:"reason,omitempty"`

	// NextRetryTime indicates when the next retry will be attempted (for rate-limited states)
	// +optional
	NextRetryTime *metav1.Time `json:"nextRetryTime,omitempty"`

	// RetryCount tracks the number of retries for failed operations
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// LastPhaseBeforeStall records the phase before entering stalled state for recovery
	// +optional
	LastPhaseBeforeStall VolumeResizePhase `json:"lastPhaseBeforeStall,omitempty"`

	// FailedPVCs contains details about PVCs that failed to resize
	// +optional
	FailedPVCs []FailedPVCInfo `json:"failedPVCs,omitempty"`
}

// ResizeHealthInfo tracks health and validation status
type ResizeHealthInfo struct {
	// MarkLogicHealthPassed indicates if MarkLogic health checks passed
	// +optional
	MarkLogicHealthPassed *bool `json:"markLogicHealthPassed,omitempty"`

	// FileSystemResizePending indicates if pods need restart for filesystem resize
	FileSystemResizePending bool `json:"fileSystemResizePending,omitempty"`
}

// ResizeRecoveryInfo tracks recovery and edge case handling
type ResizeRecoveryInfo struct {
	// StatefulSetBackup stores the StatefulSet spec for recovery
	// +optional
	StatefulSetBackup string `json:"statefulSetBackup,omitempty"`

	// OrphanedPods tracks pods that may be orphaned during STS delete/recreate
	// +optional
	OrphanedPods []string `json:"orphanedPods,omitempty"`

	// StatefulSetDeletionTimestamp records when STS was deleted (for interrupted template update recovery)
	// +optional
	StatefulSetDeletionTimestamp *metav1.Time `json:"statefulSetDeletionTimestamp,omitempty"`

	// PodPendingStartTime tracks when a pod first entered pending state for scheduling issues
	// +optional
	PodPendingStartTime *metav1.Time `json:"podPendingStartTime,omitempty"`
}

// ResizeMetaInfo tracks metadata about the resize operation
type ResizeMetaInfo struct {
	// Message provides additional information about the current status
	Message string `json:"message,omitempty"`

	// Warnings contains any warnings during the resize operation
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// ResizeStrategy used for this resize operation
	// +optional
	ResizeStrategy string `json:"resizeStrategy,omitempty"`

	// ResizeInProgressAnnotation indicates the resize operation identifier for concurrent resize detection
	// +optional
	ResizeInProgressAnnotation string `json:"resizeInProgressAnnotation,omitempty"`
}

// VolumeResizeStatus holds the status of a volume resize operation
// Fields are organized into logical sub-structs using inline embedding for backward compatibility
type VolumeResizeStatus struct {
	// Progress tracking (phase, sizes, PVC counts)
	ResizeProgress `json:",inline"`

	// Retry and error handling
	ResizeRetryInfo `json:",inline"`

	// Health and validation status
	ResizeHealthInfo `json:",inline"`

	// Recovery and edge case handling
	ResizeRecoveryInfo `json:",inline"`

	// Metadata about the operation
	ResizeMetaInfo `json:",inline"`
}

func (status *MarklogicGroupStatus) SetCondition(condition metav1.Condition) {
	conditions := status.Conditions
	exist := false
	for i := range status.Conditions {
		if status.Conditions[i].Type == condition.Type {
			status.Conditions[i] = condition
			exist = true
		}
	}

	if !exist {
		conditions = append(conditions, condition)
	}

	status.Conditions = conditions
}

func (group *MarklogicGroup) SetCondition(condition metav1.Condition) {
	(&group.Status).SetCondition(condition)
}

func (status *MarklogicGroupStatus) GetConditionStatus(conditionType string) metav1.ConditionStatus {
	for _, condition := range status.Conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}

type GroupConfig struct {
	// +kubebuilder:default:="Default"
	Name string `json:"name,omitempty"`
	// +kubebuilder:default:=true
	EnableXdqpSsl bool `json:"enableXdqpSsl,omitempty"`
}

type License struct {
	Key      string `json:"key,omitempty"`
	Licensee string `json:"licensee,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// MarklogicGroup is the Schema for the marklogicgroup API
type MarklogicGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarklogicGroupSpec   `json:"spec,omitempty"`
	Status MarklogicGroupStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MarklogicGroupList contains a list of MarklogicGroup
type MarklogicGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MarklogicGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MarklogicGroup{}, &MarklogicGroupList{})
}

type MarkLogicConditionType string

// Observed State for MarkLogic Server
const (
	GroupReady         MarkLogicConditionType = "Ready"
	ServerInitialized  MarkLogicConditionType = "Initialized"
	ServerStopped      MarkLogicConditionType = "Stopped"
	ServerResuming     MarkLogicConditionType = "Resuming"
	ServerDecommission MarkLogicConditionType = "Decommission"
	ServerUpdating     MarkLogicConditionType = "Updating"
)

// Internal State for MarkLogic Server
const (
	StateStarting    InternalState = "Starting"
	StateConfiguring InternalState = "Configuring"
	StateReady       InternalState = "Ready"
	StateFailed      InternalState = "Failed"
)
