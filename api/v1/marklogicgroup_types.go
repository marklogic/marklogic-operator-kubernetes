/*
Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

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

// +kubebuilder:validation:XValidation:rule="!has(self.dynamic) || self.isDynamic == true", message="dynamic can only be set when isDynamic is true"
// +kubebuilder:validation:XValidation:rule="!self.isDynamic || self.image.matches('^.+:(latest.*|((1[2-9]|[2-9][0-9])\\.[0-9]+\\.[0-9]+.*))$')", message="dynamic hosts require image tag latest or MarkLogic major version 12+"
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
	// +kubebuilder:default:={enabled: true, initialDelaySeconds: 10, timeoutSeconds: 5, periodSeconds: 30, successThreshold: 1, failureThreshold: 3}
	ReadinessProbe ContainerProbe `json:"readinessProbe,omitempty"`
	// +kubebuilder:default:={enabled: false, image: "fluent/fluent-bit:4.1.1", resources: {requests: {cpu: "100m", memory: "200Mi"}, limits: {cpu: "200m", memory: "500Mi"}}, files: {errorLogs: true, accessLogs: true, requestLogs: true}, outputs: "stdout"}
	LogCollection *LogCollection `json:"logCollection,omitempty"`
	// +kubebuilder:default:={name: "Default", enableXdqpSsl: true}
	GroupConfig *GroupConfig `json:"groupConfig,omitempty"`
	// +kubebuilder:default:=false
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="isDynamic is immutable after creation"
	IsDynamic bool `json:"isDynamic,omitempty"`
	// +optional
	Dynamic                        *DynamicGroupConfig             `json:"dynamic,omitempty"`
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

type VolumeResizePhase string

const (
	VolumeResizePhaseValidating               VolumeResizePhase = "Validating"
	VolumeResizePhaseResizingPVCs             VolumeResizePhase = "ResizingPVCs"
	VolumeResizePhaseWaitingForPVCResize      VolumeResizePhase = "WaitingForPVCResize"
	VolumeResizePhaseSynchronizingStatefulSet VolumeResizePhase = "SynchronizingStatefulSet"
	VolumeResizePhaseRestartingPods           VolumeResizePhase = "RestartingPods"
	VolumeResizePhaseWaitingForPodsReady      VolumeResizePhase = "WaitingForPodsReady"
	VolumeResizePhaseVerifyingResizeOutcome   VolumeResizePhase = "VerifyingResizeOutcome"
	VolumeResizePhaseCompleted                VolumeResizePhase = "Completed"
	VolumeResizePhaseStalled                  VolumeResizePhase = "Stalled"
	VolumeResizePhaseFailed                   VolumeResizePhase = "Failed"
)

type VolumeResizeReason string

const (
	VolumeResizeReasonResizeFailed               VolumeResizeReason = "ResizeFailed"
	VolumeResizeReasonPartialResizeFailure       VolumeResizeReason = "PartialResizeFailure"
	VolumeResizeReasonResizeRateLimited          VolumeResizeReason = "ResizeRateLimited"
	VolumeResizeReasonStorageQuotaExceeded       VolumeResizeReason = "StorageQuotaExceeded"
	VolumeResizeReasonResizeForbidden            VolumeResizeReason = "ResizeForbidden"
	VolumeResizeReasonInvalidResizeRequest       VolumeResizeReason = "InvalidResizeRequest"
	VolumeResizeReasonStorageClassNotExpandable  VolumeResizeReason = "StorageClassNotExpandable"
	VolumeResizeReasonShrinkNotSupported         VolumeResizeReason = "ShrinkNotSupported"
	VolumeResizeReasonPVCNotBound                VolumeResizeReason = "PVCNotBound"
	VolumeResizeReasonConcurrentResize           VolumeResizeReason = "ConcurrentResize"
	VolumeResizeReasonStatefulSetSyncFailed      VolumeResizeReason = "StatefulSetSyncFailed"
	VolumeResizeReasonPodRecoveryFailed          VolumeResizeReason = "PodRecoveryFailed"
	VolumeResizeReasonTemplateUpdateInterrupted  VolumeResizeReason = "TemplateUpdateInterrupted"
	VolumeResizeReasonMarkLogicHealthCheckFailed VolumeResizeReason = "MarkLogicHealthCheckFailed"
	VolumeResizeReasonPaused                     VolumeResizeReason = "Paused"
	VolumeResizeReasonMaxRetriesExceeded         VolumeResizeReason = "MaxRetriesExceeded"
	VolumeResizeReasonMaxOperationTimeExceeded   VolumeResizeReason = "MaxOperationTimeExceeded"
)

type PVCResizeState string

const (
	PVCResizeStatePending              PVCResizeState = "Pending"
	PVCResizeStateResizeSubmitted      PVCResizeState = "ResizeSubmitted"
	PVCResizeStateWaitingForCheckpoint PVCResizeState = "WaitingForCheckpoint"
	PVCResizeStateCheckpointed         PVCResizeState = "Checkpointed"
	PVCResizeStateRestartPending       PVCResizeState = "RestartPending"
	PVCResizeStateRestarted            PVCResizeState = "Restarted"
	PVCResizeStateFailed               PVCResizeState = "Failed"
)

type PVCResizeCheckpointType string

const (
	PVCResizeCheckpointTypeOnlineComplete  PVCResizeCheckpointType = "OnlineComplete"
	PVCResizeCheckpointTypeOfflinePending  PVCResizeCheckpointType = "OfflinePending"
	PVCResizeCheckpointTypeOfflineComplete PVCResizeCheckpointType = "OfflineComplete"
)

type PVCResizeStatus struct {
	Name             string `json:"name,omitempty"`
	PodName          string `json:"podName,omitempty"`
	RequestedSize    string `json:"requestedSize,omitempty"`
	ObservedCapacity string `json:"observedCapacity,omitempty"`
	// +kubebuilder:validation:Enum=Pending;ResizeSubmitted;WaitingForCheckpoint;Checkpointed;RestartPending;Restarted;Failed
	State PVCResizeState `json:"state,omitempty"`
	// +kubebuilder:validation:Enum=OnlineComplete;OfflinePending;OfflineComplete
	CheckpointType     PVCResizeCheckpointType `json:"checkpointType,omitempty"`
	RestartRequired    bool                    `json:"restartRequired,omitempty"`
	LastReason         string                  `json:"lastReason,omitempty"`
	LastMessage        string                  `json:"lastMessage,omitempty"`
	LastTransitionTime *metav1.Time            `json:"lastTransitionTime,omitempty"`
}

type FailedPVCStatus struct {
	Name    string `json:"name,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type VolumeResizeStatus struct {
	OperationID        string `json:"operationID,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=Validating;ResizingPVCs;WaitingForPVCResize;SynchronizingStatefulSet;RestartingPods;WaitingForPodsReady;VerifyingResizeOutcome;Completed;Stalled;Failed
	Phase   VolumeResizePhase `json:"phase,omitempty"`
	Message string            `json:"message,omitempty"`
	// +kubebuilder:validation:Enum=ResizeFailed;PartialResizeFailure;ResizeRateLimited;StorageQuotaExceeded;ResizeForbidden;InvalidResizeRequest;StorageClassNotExpandable;ShrinkNotSupported;PVCNotBound;ConcurrentResize;StatefulSetSyncFailed;PodRecoveryFailed;TemplateUpdateInterrupted;MarkLogicHealthCheckFailed;Paused;MaxRetriesExceeded;MaxOperationTimeExceeded
	Reason                     VolumeResizeReason `json:"reason,omitempty"`
	CurrentSize                string             `json:"currentSize,omitempty"`
	TargetSize                 string             `json:"targetSize,omitempty"`
	DeferredTargetSize         string             `json:"deferredTargetSize,omitempty"`
	DeferredObservedGeneration int64              `json:"deferredObservedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=parallel;sequential
	ResizeStrategy   VolumeResizeStrategy `json:"resizeStrategy,omitempty"`
	TotalPVCs        int32                `json:"totalPvcs,omitempty"`
	PVCsCheckpointed int32                `json:"pvcsCheckpointed,omitempty"`
	ActivePVC        string               `json:"activePVC,omitempty"`
	PVCStatuses      []PVCResizeStatus    `json:"pvcStatuses,omitempty"`
	FailedPVCs       []FailedPVCStatus    `json:"failedPVCs,omitempty"`
	// Internal crash-recovery workflow markers for resize reconciliation.
	Markers            []string     `json:"markers,omitempty"`
	Warnings           []string     `json:"warnings,omitempty"`
	RetryCount         int32        `json:"retryCount,omitempty"`
	NextRetryTime      *metav1.Time `json:"nextRetryTime,omitempty"`
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	FirstStartedTime   *metav1.Time `json:"firstStartedTime,omitempty"`
	CompletionTime     *metav1.Time `json:"completionTime,omitempty"`
}

// MarklogicGroupStatus defines the observed state of MarklogicGroup
type MarklogicGroupStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Conditions         []metav1.Condition       `json:"conditions,omitempty"`
	Stage              string                   `json:"stage,omitempty"`
	MarkLogicPods      []corev1.ObjectReference `json:"active,omitempty"`
	VolumeResizeStatus *VolumeResizeStatus      `json:"volumeResizeStatus,omitempty"`

	// +optional
	MarklogicGroupStatus InternalState `json:"markLogicGroupStatus,omitempty"`
	// +optional
	Dynamic *DynamicGroupStatus `json:"dynamic,omitempty"`
}

type DynamicGroupStatus struct {
	Phase               string              `json:"phase,omitempty"`
	Reason              string              `json:"reason,omitempty"`
	Message             string              `json:"message,omitempty"`
	LastTransitionTime  *metav1.Time        `json:"lastTransitionTime,omitempty"`
	BootstrapReady      bool                `json:"bootstrapReady,omitempty"`
	Configured          bool                `json:"configured,omitempty"`
	DynamicHostsEnabled bool                `json:"dynamicHostsEnabled,omitempty"`
	DesiredReplicas     int32               `json:"desiredReplicas,omitempty"`
	LocalReadyReplicas  int32               `json:"localReadyReplicas,omitempty"`
	ReadyReplicas       int32               `json:"readyReplicas,omitempty"`
	Hosts               []DynamicHostStatus `json:"hosts,omitempty"`
}

type DynamicHostStatus struct {
	PodName     string       `json:"podName,omitempty"`
	Hostname    string       `json:"hostname,omitempty"`
	HostID      string       `json:"hostId,omitempty"`
	State       string       `json:"state,omitempty"`
	Message     string       `json:"message,omitempty"`
	Attempts    int32        `json:"attempts,omitempty"`
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
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
//+kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
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
