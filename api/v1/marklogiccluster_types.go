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

package v1

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

	Auth *AdminAuth `json:"auth,omitempty"`
	// +kubebuilder:default:={enabled: true, size: "10Gi"}
	Persistence                   *Persistence                 `json:"persistence,omitempty"`
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
	// +kubebuilder:default:={enabled: false, image: "fluent/fluent-bit:3.2.5", resources: {requests: {cpu: "100m", memory: "200Mi"}, limits: {cpu: "200m", memory: "500Mi"}}, files: {errorLogs: true, accessLogs: true, requestLogs: true}, outputs: "stdout"}
	LogCollection                  *LogCollection                  `json:"logCollection,omitempty"`
	HAProxy                        *HAProxy                        `json:"haproxy,omitempty"`
	Tls                            *Tls                            `json:"tls,omitempty"`
	AdditionalVolumes              *[]corev1.Volume                `json:"additionalVolumes,omitempty"`
	AdditionalVolumeMounts         *[]corev1.VolumeMount           `json:"additionalVolumeMounts,omitempty"`
	AdditionalVolumeClaimTemplates *[]corev1.PersistentVolumeClaim `json:"additionalVolumeClaimTemplates,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="size(self) == 1 || (size(self) == size(self.map(x, x.groupConfig.name).filter(y, self.map(x, x.groupConfig.name).filter(z, z == y).size() == 1)))", message="MarkLogicGroups must have unique groupConfig names"
	// +kubebuilder:validation:XValidation:rule="size(self) == 1 || (size(self) == size(self.map(x, x.name).filter(y, self.map(x, x.name).filter(z, z == y).size() == 1)))", message="MarkLogicGroups must have unique names"
	// +kubebuilder:validation:XValidation:rule="size(self) == size(self.map(x, x.name).filter(y, self.map(x, x.name).filter(z, z == y).size() == 1))", message="MarkLogicGroups must have unique names"
	// +kubebuilder:validation:XValidation:rule="self[0].name == oldSelf[0].name", message="Name of MarkLogikGroup must not be changed"
	// +kubebuilder:validation:XValidation:rule="size(self) >= 2 && size(oldSelf) >= 2 ? self[1].name == oldSelf[1].name : true", message="Name of MarkLogikGroup must not be changed"
	// +kubebuilder:validation:XValidation:rule="size(self) >= 3 && size(oldSelf) >= 3 ? self[2].name == oldSelf[2].name : true", message="Name of MarkLogikGroup must not be changed"
	// +kubebuilder:validation:XValidation:rule="size(self) >= 4 && size(oldSelf) >= 4 ? self[3].name == oldSelf[3].name : true", message="Name of MarkLogikGroup must not be changed"
	// +kubebuilder:validation:XValidation:rule="size(self) >= 5 && size(oldSelf) >= 5 ? self[4].name == oldSelf[4].name : true", message="Name of MarkLogikGroup must not be changed"
	// +kubebuilder:validation:XValidation:rule="size(self.filter(x, x.isBootstrap == true)) == 1", message="Exactly one MarkLogicGroup must have isBootstrap set to true"
	MarkLogicGroups []*MarklogicGroups `json:"markLogicGroups,omitempty"`
}

type MarklogicGroups struct {
	// +kubebuilder:default:=1
	Replicas *int32 `json:"replicas,omitempty"`
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
	// +kubebuilder:default:={name: "Default", enableXdqpSsl: true}
	GroupConfig               *GroupConfig                      `json:"groupConfig,omitempty"`
	Image                     string                            `json:"image,omitempty"`
	ImagePullPolicy           string                            `json:"imagePullPolicy,omitempty"`
	ImagePullSecrets          []corev1.LocalObjectReference     `json:"imagePullSecrets,omitempty"`
	Persistence               *Persistence                      `json:"persistence,omitempty"`
	Service                   Service                           `json:"service,omitempty"`
	Resources                 *corev1.ResourceRequirements      `json:"resources,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	PriorityClassName         string                            `json:"priorityClassName,omitempty"`
	HugePages                 *HugePages                        `json:"hugePages,omitempty"`
	LogCollection             *LogCollection                    `json:"logCollection,omitempty"`
	HAProxy                   *HAProxy                          `json:"haproxy,omitempty"`
	// +kubebuilder:default:=false
	IsBootstrap                    bool                            `json:"isBootstrap,omitempty"`
	Tls                            *Tls                            `json:"tls,omitempty"`
	AdditionalVolumes              *[]corev1.Volume                `json:"additionalVolumes,omitempty"`
	AdditionalVolumeMounts         *[]corev1.VolumeMount           `json:"additionalVolumeMounts,omitempty"`
	AdditionalVolumeClaimTemplates *[]corev1.PersistentVolumeClaim `json:"additionalVolumeClaimTemplates,omitempty"`
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
