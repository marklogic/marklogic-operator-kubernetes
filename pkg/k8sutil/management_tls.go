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
