/*
Copyright 2023.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MarklogicClusterSpec defines the desired state of MarklogicCluster
type MarklogicClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of MarklogicCluster. Edit marklogiccluster_types.go to remove/update
	MarkLogicGroups []*MarklogicGroups `json:"markLogicGroups,omitempty"`
}

type MarklogicGroups struct {
	*MarklogicGroupSpec `json:"spec,omitempty"`
	IsBootstrap         bool `json:"isBootstrap,omitempty"`
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

//
