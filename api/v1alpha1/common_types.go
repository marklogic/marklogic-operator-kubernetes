package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

type Probe struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// Storage is the inteface to add pvc and pv support in marklogic

type Storage struct {
	Size        string             `json:"size,omitempty"`
	VolumeMount VolumeMountWrapper `json:"volumeMount,omitempty"`
}

type VolumeMountWrapper struct {
	Volume    []corev1.Volume      `json:"volume,omitempty"`
	MountPath []corev1.VolumeMount `json:"mountPath,omitempty"`
}

type AdminAuth struct {
	AdminUsername  *string `json:"adminUsername,omitempty"`
	AdminPassword  *string `json:"adminPassword,omitempty"`
	WalletPassword *string `json:"walletPassword,omitempty"`
}
