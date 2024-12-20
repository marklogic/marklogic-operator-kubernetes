package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	Size         string             `json:"size,omitempty"`
	VolumeMount  VolumeMountWrapper `json:"volumeMount,omitempty"`
	StorageClass string             `json:"storageClass,omitempty"`
}

type HugePages struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default:="/dev/hugepages"
	MountPath string `json:"mountPath,omitempty"`
}

type Service struct {
	// +kubebuilder:default:= ClusterIP
	Type            corev1.ServiceType   `json:"type,omitempty"`
	AdditionalPorts []corev1.ServicePort `json:"additionalPorts,omitempty"`
	Annotations     map[string]string    `json:"annotations,omitempty"`
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
	Files     LogFilesConfig               `json:"files,omitempty"`
	Outputs   string                       `json:"outputs,omitempty"`
}

type LogFilesConfig struct {
	ErrorLogs   bool `json:"errorLogs,omitempty"`
	AccessLogs  bool `json:"accessLogs,omitempty"`
	RequestLogs bool `json:"requestLogs,omitempty"`
	CrashLogs   bool `json:"crashLogs,omitempty"`
	AuditLogs   bool `json:"auditLogs,omitempty"`
}

type NetworkPolicy struct {
	Enabled     bool                                    `json:"enabled,omitempty"`
	PolicyTypes []networkingv1.PolicyType               `json:"policyTypes,omitempty"`
	PodSelector metav1.LabelSelector                    `json:"podSelector,omitempty"`
	Ingress     []networkingv1.NetworkPolicyIngressRule `json:"ingress,omitempty"`
	Egress      []networkingv1.NetworkPolicyEgressRule  `json:"egress,omitempty"`
}
type HAProxy struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default:="haproxytech/haproxy-alpine:3.1"
	Image string `json:"image,omitempty"`
	// +kubebuilder:default:=1
	ReplicaCount int32 `json:"replicas,omitempty"`
	// +kubebuilder:default:=80
	FrontendPort int32 `json:"frontendPort,omitempty"`
	// +kubebuilder:default:={{name: "AppServices", type: "http", port: 8000, targetPort: 8000, path: "/console"}, {name: "Admin", type: "http", port: 8001, targetPort: 8001, path: "/adminUI"}, {name: "Manage", type: "http", port: 8002, targetPort: 8002, path: "/manage"}}
	AppServers []AppServers `json:"appServers,omitempty"`
	// +kubebuilder:default:=true
	PathBasedRouting   bool `json:"pathBasedRouting,omitempty"`
	RestartWhenUpgrade bool `json:"restartWhenUpgrade,omitempty"`
	// +kubebuilder:default:={type: ClusterIP}
	Service ServiceForHAProxy `json:"service,omitempty"`
	// +kubebuilder:default:={enabled: false}
	TcpPorts Tcpports `json:"tcpPorts,omitempty"`
	// +kubebuilder:default:={client: 600, connect: 600, server: 600}
	Timeout Timeout `json:"timeout,omitempty"`
	// +kubebuilder:default:={enabled: false, secretName: "", certFileName: ""}
	Tls *TlsForHAProxy `json:"tls,omitempty"`
	// +kubebuilder:default:={enabled: false, port: 1024, auth: {enabled: false, username: "", password: ""}}
	Stats        Stats                       `json:"stats,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
	Affinity     *corev1.Affinity            `json:"affinity,omitempty"`
	NodeSelector map[string]string           `json:"nodeSelector,omitempty"`
	Ingress      Ingress                     `json:"ingress,omitempty"`
}

type AppServers struct {
	Name       string `json:"name,omitempty"`
	Type       string `json:"type,omitempty"`
	Port       int32  `json:"port,omitempty"`
	TargetPort int32  `json:"targetPort,omitempty"`
	Path       string `json:"path,omitempty"`
}

type Stats struct {
	Enabled bool      `json:"enabled,omitempty"`
	Port    int32     `json:"port,omitempty"`
	Auth    StatsAuth `json:"auth,omitempty"`
}

type StatsAuth struct {
	Enabled  bool   `json:"enabled,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type ServiceForHAProxy struct {
	Type corev1.ServiceType `json:"type,omitempty"`
}

type Tcpports struct {
	Enabled bool      `json:"enabled,omitempty"`
	Ports   []TcpPort `json:"ports,omitempty"`
}

type TcpPort struct {
	Port int32  `json:"port,omitempty"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
}

type Timeout struct {
	Client  int32 `json:"client,omitempty"`
	Connect int32 `json:"connect,omitempty"`
	Server  int32 `json:"server,omitempty"`
}

type TlsForHAProxy struct {
	Enabled      bool   `json:"enabled,omitempty"`
	SecretName   string `json:"secretName,omitempty"`
	CertFileName string `json:"certFileName,omitempty"`
}

type Ingress struct {
	// +kubebuilder:default:=false
	Enabled          bool                       `json:"enabled,omitempty"`
	IngressClassName string                     `json:"ingressClassName,omitempty"`
	Labels           map[string]string          `json:"labels,omitempty"`
	Annotations      map[string]string          `json:"annotations,omitempty"`
	Host             string                     `json:"host,omitempty"`
	TLS              []networkingv1.IngressTLS  `json:"tls,omitempty"`
	AdditionalHosts  []networkingv1.IngressRule `json:"additionalHosts,omitempty"`
}
