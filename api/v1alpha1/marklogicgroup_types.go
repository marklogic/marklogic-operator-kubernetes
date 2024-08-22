/*
Copyright 2024.

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

package v1alpha1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MarklogicGroupSpec defines the desired state of MarklogicGroup
type MarklogicGroupSpec struct {
	// +kubebuilder:default:=1
	Replicas *int32 `json:"replicas,omitempty"`
	Name     string `json:"name,omitempty"`

	// +kubebuilder:default:="cluster.local"
	ClusterDomain string `json:"clusterDomain,omitempty"`

	// +kubebuilder:default:="progressofficial/marklogic-db:11.3.0-ubi-rootless"
	Image string `json:"image"`
	// +kubebuilder:default:="IfNotPresent"
	ImagePullPolicy  string                        `json:"imagePullPolicy,omitempty"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	Auth                          *AdminAuth                   `json:"auth,omitempty"`
	Storage                       *Storage                     `json:"storage,omitempty"`
	Resources                     *corev1.ResourceRequirements `json:"resources,omitempty"`
	TerminationGracePeriodSeconds *int64                       `json:"terminationGracePeriodSeconds,omitempty"`
	// +kubebuilder:validation:Enum=OnDelete;RollingUpdate
	// +kubebuilder:default:="OnDelete"
	UpdateStrategy           appsv1.StatefulSetUpdateStrategyType `json:"updateStrategy,omitempty"`
	NetworkPolicy            *networkingv1.NetworkPolicy          `json:"networkPolicy,omitempty"`
	PodSecurityContext       *corev1.PodSecurityContext           `json:"securityContext,omitempty"`
	ContainerSecurityContext *corev1.SecurityContext              `json:"containerSecurityContext,omitempty"`

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

	// +kubebuilder:default:={enabled: false, image: "fluent/fluent-bit:3.1.1", resources: {requests: {cpu: "100m", memory: "200Mi"}, limits: {cpu: "200m", memory: "500Mi"}}, files: {errorLogs: true, accessLogs: true, requestLogs: true}, outputs: "stdout"}
	LogCollection *LogCollection `json:"logCollection,omitempty"`

	// +kubebuilder:default:={name: "Default", enableXdqpSsl: true}
	GroupConfig      GroupConfig `json:"groupConfig,omitempty"`
	License          *License    `json:"license,omitempty"`
	EnableConverters bool        `json:"enableConverters,omitempty"`

	BootstrapHost string `json:"bootstrapHost,omitempty"`

	DoNotDelete *bool `json:"doNotDelete,omitempty"`
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
	Name          string `json:"name,omitempty"`
	EnableXdqpSsl bool   `json:"enableXdqpSsl,omitempty"`
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
