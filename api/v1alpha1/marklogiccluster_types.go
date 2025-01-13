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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MarklogicClusterSpec defines the desired state of MarklogicCluster

// +kubebuilder:validation:XValidation:rule="!(self.haproxy.enabled == true && self.haproxy.pathBasedRouting == true) || int(self.image.split(':')[1].split('.')[0] + self.image.split(':')[1].split('.')[1]) >= 111", message="HAProxy and Pathbased Routing is enabled. PathBasedRouting is only supported for MarkLogic 11.1 and above"
type MarklogicClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

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
	UpdateStrategy            appsv1.StatefulSetUpdateStrategyType `json:"updateStrategy,omitempty"`
	NetworkPolicy             NetworkPolicy                        `json:"networkPolicy,omitempty"`
	PodSecurityContext        *corev1.PodSecurityContext           `json:"podSecurityContext,omitempty"`
	ContainerSecurityContext  *corev1.SecurityContext              `json:"securityContext,omitempty"`
	Affinity                  *corev1.Affinity                     `json:"affinity,omitempty"`
	NodeSelector              map[string]string                    `json:"nodeSelector,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint    `json:"topologySpreadConstraints,omitempty"`
	PriorityClassName         string                               `json:"priorityClassName,omitempty"`
	License                   *License                             `json:"license,omitempty"`
	EnableConverters          bool                                 `json:"enableConverters,omitempty"`
	// +kubebuilder:default:={enabled: false, mountPath: "/dev/hugepages"}
	HugePages *HugePages `json:"hugePages,omitempty"`
	// +kubebuilder:default:={enabled: false, image: "fluent/fluent-bit:3.1.1", resources: {requests: {cpu: "100m", memory: "200Mi"}, limits: {cpu: "200m", memory: "500Mi"}}, files: {errorLogs: true, accessLogs: true, requestLogs: true}, outputs: "stdout"}
	LogCollection *LogCollection `json:"logCollection,omitempty"`
	HAProxy *HAProxy `json:"haproxy,omitempty"`
	Tls     *Tls     `json:"tls,omitempty"`

	MarkLogicGroups []*MarklogicGroups `json:"markLogicGroups,omitempty"`
}

type MarklogicGroups struct {
	Replicas                  *int32                            `json:"replicas,omitempty"`
	Name                      string                            `json:"name,omitempty"`
	GroupConfig               *GroupConfig                      `json:"groupConfig,omitempty"`
	Image                     string                            `json:"image,omitempty"`
	ImagePullPolicy           string                            `json:"imagePullPolicy,omitempty"`
	ImagePullSecrets          []corev1.LocalObjectReference     `json:"imagePullSecrets,omitempty"`
	Storage                   *Storage                          `json:"storage,omitempty"`
	Resources                 *corev1.ResourceRequirements      `json:"resources,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	PriorityClassName         string                            `json:"priorityClassName,omitempty"`
	HugePages                 *HugePages                        `json:"hugePages,omitempty"`
	LogCollection             *LogCollection                    `json:"logCollection,omitempty"`
	HAProxy                   *HAProxy                          `json:"haproxy,omitempty"`
	IsBootstrap               bool                              `json:"isBootstrap,omitempty"`
	Tls                       *Tls                              `json:"tls,omitempty"`
}

type Tls struct {
	// +kubebuilder:default:=false
	EnableOnDefaultAppServers bool     `json:"enableOnDefaultAppServers,omitempty"`
	CertSecretNames           []string `json:"certSecretNames,omitempty"`
	CaSecretName              string   `json:"caSecretName,omitempty"`
}

// MarklogicClusterStatus defines the observed state of MarklogicCluster
type MarklogicClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// MarklogicCluster is the Schema for the marklogicclusters API
type MarklogicCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarklogicClusterSpec   `json:"spec,omitempty"`
	Status MarklogicClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MarklogicClusterList contains a list of MarklogicCluster
type MarklogicClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MarklogicCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MarklogicCluster{}, &MarklogicClusterList{})
}

// Observed State for MarkLogic Cluster
const (
	ClusterReady        MarkLogicConditionType = "Ready"
	ClusterInitialized  MarkLogicConditionType = "Initialized"
	ClusterScalingUp    MarkLogicConditionType = "Stopped"
	ClusterScalingDown  MarkLogicConditionType = "Resuming"
	ClusterDecommission MarkLogicConditionType = "Decommission"
	ClusterUpdating     MarkLogicConditionType = "Updating"
)
