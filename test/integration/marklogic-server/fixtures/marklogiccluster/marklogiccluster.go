// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package marklogiccluster

import (
	"fmt"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultImage = "progressofficial/marklogic-db:12.0.3-ubi9-rootless-2.2.6"
	DefaultName  = "marklogic"
)

type Config struct {
	Namespace              string
	Name                   string
	Image                  string
	AdminUsername          string
	AdminPassword          string
	CASecretName           string
	CertificateSecretNames []string
}

// Build creates a two-node TLS-enabled MarklogicCluster with operator-managed HAProxy.
func Build(config Config) (*marklogicv1.MarklogicCluster, error) {
	if config.Namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if config.AdminUsername == "" || config.AdminPassword == "" {
		return nil, fmt.Errorf("admin username and password are required")
	}
	if config.CASecretName == "" {
		return nil, fmt.Errorf("CA Secret name is required")
	}
	if len(config.CertificateSecretNames) != 2 {
		return nil, fmt.Errorf("exactly two certificate Secret names are required")
	}
	name := config.Name
	if name == "" {
		name = DefaultName
	}
	image := config.Image
	if image == "" {
		image = DefaultImage
	}
	replicas := int32(2)

	return &marklogicv1.MarklogicCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "marklogic.progress.com/v1", Kind: "MarklogicCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace},
		Spec: marklogicv1.MarklogicClusterSpec{
			Image: image,
			Auth: &marklogicv1.AdminAuth{
				AdminUsername: &config.AdminUsername,
				AdminPassword: &config.AdminPassword,
			},
			MarkLogicGroups: []*marklogicv1.MarklogicGroups{{
				Name:        name,
				Replicas:    &replicas,
				IsBootstrap: true,
			}},
			Tls: &marklogicv1.Tls{
				EnableOnDefaultAppServers: true,
				CaSecretName:              config.CASecretName,
				CertSecretNames:           config.CertificateSecretNames,
			},
			HAProxy: &marklogicv1.HAProxy{
				Enabled:      true,
				ReplicaCount: 1,
				FrontendPort: 8000,
				AppServers: []marklogicv1.AppServers{
					{Name: "app-services", Port: 8000},
					{Name: "manage", Port: 8002},
				},
			},
		},
	}, nil
}
