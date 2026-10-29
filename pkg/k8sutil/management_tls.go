// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const markLogicCACertificateKey = "cacert.pem"

func (oc *OperatorContext) managementTLSOptions() (bool, bool, []byte, error) {
	return managementTLSOptionsForConfig(oc.Ctx, oc.Client, oc.MarklogicGroup.Namespace, oc.MarklogicGroup.Spec.Tls)
}

func (oc *OperatorContext) managementTLSOptionsForEndpoint(endpoint string) (bool, bool, []byte, error) {
	tlsConfig, err := oc.managementTLSConfigForEndpoint(endpoint)
	if err != nil {
		return false, false, nil, err
	}
	return managementTLSOptionsForConfig(oc.Ctx, oc.Client, oc.MarklogicGroup.Namespace, tlsConfig)
}

func (oc *OperatorContext) managementTLSConfigForEndpoint(endpoint string) (*marklogicv1.Tls, error) {
	endpointGroupName := managementEndpointGroupName(endpoint)
	if endpointGroupName == "" {
		return nil, fmt.Errorf("resolve Management API TLS for endpoint %q: group service name is not present", endpoint)
	}

	currentGroupName := strings.TrimSpace(oc.MarklogicGroup.Spec.Name)
	if currentGroupName == "" {
		currentGroupName = oc.MarklogicGroup.Name
	}
	if strings.EqualFold(endpointGroupName, currentGroupName) {
		return oc.MarklogicGroup.Spec.Tls, nil
	}

	clusterName, err := oc.getOwningClusterName()
	if err != nil {
		return nil, fmt.Errorf("resolve Management API TLS for endpoint %q: %w", endpoint, err)
	}
	cluster := &marklogicv1.MarklogicCluster{}
	if err := oc.Client.Get(oc.Ctx, client.ObjectKey{Namespace: oc.MarklogicGroup.Namespace, Name: clusterName}, cluster); err != nil {
		return nil, fmt.Errorf("read owning MarklogicCluster %q for Management API endpoint %q: %w", clusterName, endpoint, err)
	}
	for _, group := range cluster.Spec.MarkLogicGroups {
		if group == nil || !strings.EqualFold(strings.TrimSpace(group.Name), endpointGroupName) {
			continue
		}
		if group.Tls != nil {
			return group.Tls, nil
		}
		return cluster.Spec.Tls, nil
	}

	return nil, fmt.Errorf("resolve Management API TLS for endpoint %q: group %q is not defined in MarklogicCluster %q", endpoint, endpointGroupName, clusterName)
}

func managementEndpointGroupName(endpoint string) string {
	host := strings.TrimSpace(endpoint)
	if separator := strings.LastIndex(host, ":"); separator >= 0 && !strings.Contains(host[separator+1:], ".") {
		host = host[:separator]
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(labels) < 2 {
		return ""
	}
	return labels[1]
}

func managementTLSOptionsForConfig(ctx context.Context, reader client.Reader, namespace string, tlsConfig *marklogicv1.Tls) (bool, bool, []byte, error) {
	if tlsConfig == nil || !tlsConfig.EnableOnDefaultAppServers {
		return false, false, nil, nil
	}
	caSecretName := strings.TrimSpace(tlsConfig.CaSecretName)
	if caSecretName == "" {
		return true, true, nil, nil
	}
	caSecret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: caSecretName}, caSecret); err != nil {
		return false, false, nil, fmt.Errorf("read trusted CA Secret %q: %w", caSecretName, err)
	}
	caCertificate := caSecret.Data[markLogicCACertificateKey]
	if len(caCertificate) == 0 {
		return false, false, nil, fmt.Errorf("trusted CA Secret %q is missing %q", caSecretName, markLogicCACertificateKey)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(caCertificate) {
		return false, false, nil, fmt.Errorf("trusted CA Secret %q key %q does not contain a valid PEM certificate", caSecretName, markLogicCACertificateKey)
	}
	return true, false, caCertificate, nil
}
