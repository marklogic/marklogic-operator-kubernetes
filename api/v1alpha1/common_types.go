package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

type ContainerProbe struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:validation:Minimum=0
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	// +kubebuilder:validation:Minimum=0
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=0
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=0
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
	// +kubebuilder:validation:Minimum=0
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// Storage is the inteface to add pvc and pv support in marklogic
type Storage struct {
	Size        string             `json:"size,omitempty"`
	VolumeMount VolumeMountWrapper `json:"volumeMount,omitempty"`
}

type HugePages struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default:="/dev/hugepages"
	MountPath string `json:"mountPath,omitempty"`
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

type LogCollection struct {
	Enabled   bool                         `json:"enabled,omitempty"`
	Image     string                       `json:"image,omitempty"`
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	Files     LogFiles                     `json:"files,omitempty"`
	Outputs   string                       `json:"outputs,omitempty"`
}

type LogFiles struct {
	ErrorLogs   bool `json:"errorLogs,omitempty"`
	AccessLogs  bool `json:"accessLogs,omitempty"`
	RequestLogs bool `json:"requestLogs,omitempty"`
	CrashLogs   bool `json:"crashLogs,omitempty"`
	AuditLogs   bool `json:"auditLogs,omitempty"`
}
