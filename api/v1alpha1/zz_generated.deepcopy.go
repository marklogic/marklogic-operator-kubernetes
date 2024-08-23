//go:build !ignore_autogenerated

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

// Code generated by controller-gen. DO NOT EDIT.

package v1alpha1

import (
	"k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *AdminAuth) DeepCopyInto(out *AdminAuth) {
	*out = *in
	if in.AdminUsername != nil {
		in, out := &in.AdminUsername, &out.AdminUsername
		*out = new(string)
		**out = **in
	}
	if in.AdminPassword != nil {
		in, out := &in.AdminPassword, &out.AdminPassword
		*out = new(string)
		**out = **in
	}
	if in.WalletPassword != nil {
		in, out := &in.WalletPassword, &out.WalletPassword
		*out = new(string)
		**out = **in
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AdminAuth.
func (in *AdminAuth) DeepCopy() *AdminAuth {
	if in == nil {
		return nil
	}
	out := new(AdminAuth)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ContainerProbe) DeepCopyInto(out *ContainerProbe) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ContainerProbe.
func (in *ContainerProbe) DeepCopy() *ContainerProbe {
	if in == nil {
		return nil
	}
	out := new(ContainerProbe)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *GroupConfig) DeepCopyInto(out *GroupConfig) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new GroupConfig.
func (in *GroupConfig) DeepCopy() *GroupConfig {
	if in == nil {
		return nil
	}
	out := new(GroupConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *HugePages) DeepCopyInto(out *HugePages) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new HugePages.
func (in *HugePages) DeepCopy() *HugePages {
	if in == nil {
		return nil
	}
	out := new(HugePages)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *License) DeepCopyInto(out *License) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new License.
func (in *License) DeepCopy() *License {
	if in == nil {
		return nil
	}
	out := new(License)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *LogCollection) DeepCopyInto(out *LogCollection) {
	*out = *in
	if in.Resources != nil {
		in, out := &in.Resources, &out.Resources
		*out = new(v1.ResourceRequirements)
		(*in).DeepCopyInto(*out)
	}
	out.Files = in.Files
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new LogCollection.
func (in *LogCollection) DeepCopy() *LogCollection {
	if in == nil {
		return nil
	}
	out := new(LogCollection)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *LogFilesConfig) DeepCopyInto(out *LogFilesConfig) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new LogFilesConfig.
func (in *LogFilesConfig) DeepCopy() *LogFilesConfig {
	if in == nil {
		return nil
	}
	out := new(LogFilesConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicCluster) DeepCopyInto(out *MarklogicCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicCluster.
func (in *MarklogicCluster) DeepCopy() *MarklogicCluster {
	if in == nil {
		return nil
	}
	out := new(MarklogicCluster)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *MarklogicCluster) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicClusterList) DeepCopyInto(out *MarklogicClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]MarklogicCluster, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicClusterList.
func (in *MarklogicClusterList) DeepCopy() *MarklogicClusterList {
	if in == nil {
		return nil
	}
	out := new(MarklogicClusterList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *MarklogicClusterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicClusterSpec) DeepCopyInto(out *MarklogicClusterSpec) {
	*out = *in
	if in.MarkLogicGroups != nil {
		in, out := &in.MarkLogicGroups, &out.MarkLogicGroups
		*out = make([]*MarklogicGroups, len(*in))
		for i := range *in {
			if (*in)[i] != nil {
				in, out := &(*in)[i], &(*out)[i]
				*out = new(MarklogicGroups)
				(*in).DeepCopyInto(*out)
			}
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicClusterSpec.
func (in *MarklogicClusterSpec) DeepCopy() *MarklogicClusterSpec {
	if in == nil {
		return nil
	}
	out := new(MarklogicClusterSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicClusterStatus) DeepCopyInto(out *MarklogicClusterStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicClusterStatus.
func (in *MarklogicClusterStatus) DeepCopy() *MarklogicClusterStatus {
	if in == nil {
		return nil
	}
	out := new(MarklogicClusterStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicGroup) DeepCopyInto(out *MarklogicGroup) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicGroup.
func (in *MarklogicGroup) DeepCopy() *MarklogicGroup {
	if in == nil {
		return nil
	}
	out := new(MarklogicGroup)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *MarklogicGroup) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicGroupList) DeepCopyInto(out *MarklogicGroupList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]MarklogicGroup, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicGroupList.
func (in *MarklogicGroupList) DeepCopy() *MarklogicGroupList {
	if in == nil {
		return nil
	}
	out := new(MarklogicGroupList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *MarklogicGroupList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicGroupSpec) DeepCopyInto(out *MarklogicGroupSpec) {
	*out = *in
	if in.Replicas != nil {
		in, out := &in.Replicas, &out.Replicas
		*out = new(int32)
		**out = **in
	}
	if in.ImagePullSecrets != nil {
		in, out := &in.ImagePullSecrets, &out.ImagePullSecrets
		*out = make([]v1.LocalObjectReference, len(*in))
		copy(*out, *in)
	}
	if in.Auth != nil {
		in, out := &in.Auth, &out.Auth
		*out = new(AdminAuth)
		(*in).DeepCopyInto(*out)
	}
	if in.Storage != nil {
		in, out := &in.Storage, &out.Storage
		*out = new(Storage)
		(*in).DeepCopyInto(*out)
	}
	if in.Resources != nil {
		in, out := &in.Resources, &out.Resources
		*out = new(v1.ResourceRequirements)
		(*in).DeepCopyInto(*out)
	}
	if in.TerminationGracePeriodSeconds != nil {
		in, out := &in.TerminationGracePeriodSeconds, &out.TerminationGracePeriodSeconds
		*out = new(int64)
		**out = **in
	}
	if in.NetworkPolicy != nil {
		in, out := &in.NetworkPolicy, &out.NetworkPolicy
		*out = new(networkingv1.NetworkPolicy)
		(*in).DeepCopyInto(*out)
	}
	if in.PodSecurityContext != nil {
		in, out := &in.PodSecurityContext, &out.PodSecurityContext
		*out = new(v1.PodSecurityContext)
		(*in).DeepCopyInto(*out)
	}
	if in.ContainerSecurityContext != nil {
		in, out := &in.ContainerSecurityContext, &out.ContainerSecurityContext
		*out = new(v1.SecurityContext)
		(*in).DeepCopyInto(*out)
	}
	if in.Affinity != nil {
		in, out := &in.Affinity, &out.Affinity
		*out = new(v1.Affinity)
		(*in).DeepCopyInto(*out)
	}
	if in.NodeSelector != nil {
		in, out := &in.NodeSelector, &out.NodeSelector
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.TopologySpreadConstraints != nil {
		in, out := &in.TopologySpreadConstraints, &out.TopologySpreadConstraints
		*out = make([]v1.TopologySpreadConstraint, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.HugePages != nil {
		in, out := &in.HugePages, &out.HugePages
		*out = new(HugePages)
		**out = **in
	}
	out.LivenessProbe = in.LivenessProbe
	out.ReadinessProbe = in.ReadinessProbe
	if in.LogCollection != nil {
		in, out := &in.LogCollection, &out.LogCollection
		*out = new(LogCollection)
		(*in).DeepCopyInto(*out)
	}
	out.GroupConfig = in.GroupConfig
	if in.License != nil {
		in, out := &in.License, &out.License
		*out = new(License)
		**out = **in
	}
	if in.DoNotDelete != nil {
		in, out := &in.DoNotDelete, &out.DoNotDelete
		*out = new(bool)
		**out = **in
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicGroupSpec.
func (in *MarklogicGroupSpec) DeepCopy() *MarklogicGroupSpec {
	if in == nil {
		return nil
	}
	out := new(MarklogicGroupSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicGroupStatus) DeepCopyInto(out *MarklogicGroupStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.MarkLogicPods != nil {
		in, out := &in.MarkLogicPods, &out.MarkLogicPods
		*out = make([]v1.ObjectReference, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicGroupStatus.
func (in *MarklogicGroupStatus) DeepCopy() *MarklogicGroupStatus {
	if in == nil {
		return nil
	}
	out := new(MarklogicGroupStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *MarklogicGroups) DeepCopyInto(out *MarklogicGroups) {
	*out = *in
	if in.MarklogicGroupSpec != nil {
		in, out := &in.MarklogicGroupSpec, &out.MarklogicGroupSpec
		*out = new(MarklogicGroupSpec)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new MarklogicGroups.
func (in *MarklogicGroups) DeepCopy() *MarklogicGroups {
	if in == nil {
		return nil
	}
	out := new(MarklogicGroups)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Storage) DeepCopyInto(out *Storage) {
	*out = *in
	in.VolumeMount.DeepCopyInto(&out.VolumeMount)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Storage.
func (in *Storage) DeepCopy() *Storage {
	if in == nil {
		return nil
	}
	out := new(Storage)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VolumeMountWrapper) DeepCopyInto(out *VolumeMountWrapper) {
	*out = *in
	if in.Volume != nil {
		in, out := &in.Volume, &out.Volume
		*out = make([]v1.Volume, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.MountPath != nil {
		in, out := &in.MountPath, &out.MountPath
		*out = make([]v1.VolumeMount, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VolumeMountWrapper.
func (in *VolumeMountWrapper) DeepCopy() *VolumeMountWrapper {
	if in == nil {
		return nil
	}
	out := new(VolumeMountWrapper)
	in.DeepCopyInto(out)
	return out
}
