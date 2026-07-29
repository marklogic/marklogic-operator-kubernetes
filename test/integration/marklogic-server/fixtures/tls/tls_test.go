// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package tls

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildResourcesCreatesTrustedServerCertificate(t *testing.T) {
	resources, err := BuildResources(Config{
		Namespace:     "oauth-test",
		CASecretName:  "test-ca",
		TLSSecretName: "oauth-server-tls",
		DNSNames:      []string{"oauth.example.test", "oauth"},
	})
	if err != nil {
		t.Fatalf("BuildResources returned an error: %v", err)
	}
	if resources.TLSSecret.Type != corev1.SecretTypeTLS {
		t.Fatalf("TLS Secret type = %q, want %q", resources.TLSSecret.Type, corev1.SecretTypeTLS)
	}
	certificateBlock, _ := pem.Decode(resources.TLSSecret.Data[corev1.TLSCertKey])
	if certificateBlock == nil {
		t.Fatal("TLS Secret does not contain a PEM certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate returned an error: %v", err)
	}
	if err := certificate.VerifyHostname("oauth.example.test"); err != nil {
		t.Fatalf("server certificate does not trust oauth.example.test: %v", err)
	}
	if len(resources.CASecret.Data[caCertificateKey]) == 0 {
		t.Fatal("CA Secret does not contain a CA certificate")
	}
}

func TestBuildResourcesRequiresTLSConfiguration(t *testing.T) {
	validConfig := Config{Namespace: "oauth-test", CASecretName: "test-ca", TLSSecretName: "server-tls", DNSNames: []string{"oauth"}}
	for name, config := range map[string]Config{
		"namespace":  {CASecretName: validConfig.CASecretName, TLSSecretName: validConfig.TLSSecretName, DNSNames: validConfig.DNSNames},
		"CA Secret":  {Namespace: validConfig.Namespace, TLSSecretName: validConfig.TLSSecretName, DNSNames: validConfig.DNSNames},
		"TLS Secret": {Namespace: validConfig.Namespace, CASecretName: validConfig.CASecretName, DNSNames: validConfig.DNSNames},
		"DNS name":   {Namespace: validConfig.Namespace, CASecretName: validConfig.CASecretName, TLSSecretName: validConfig.TLSSecretName},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildResources(config); err == nil {
				t.Fatal("BuildResources accepted an incomplete TLS configuration")
			}
		})
	}
}

func TestBuildClusterResourcesIssuesAllCertificatesFromOneCA(t *testing.T) {
	resources, err := BuildClusterResources(ClusterConfig{
		Namespace:    "oauth-test",
		CASecretName: "test-ca",
		Servers: []ServerConfig{
			{TLSSecretName: "marklogic-0-tls", DNSNames: []string{"marklogic-0"}},
			{TLSSecretName: "marklogic-1-tls", DNSNames: []string{"marklogic-1"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildClusterResources returned an error: %v", err)
	}
	if len(resources.TLSSecrets) != 2 {
		t.Fatalf("TLS Secret count = %d, want 2", len(resources.TLSSecrets))
	}
	caBlock, _ := pem.Decode(resources.CASecret.Data[caCertificateKey])
	if caBlock == nil {
		t.Fatal("CA Secret does not contain a PEM certificate")
	}
	caCertificate, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate for CA returned an error: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	for index, secret := range resources.TLSSecrets {
		certificateBlock, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
		certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
		if err != nil {
			t.Fatalf("ParseCertificate for server %d returned an error: %v", index, err)
		}
		if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: certificate.DNSNames[0]}); err != nil {
			t.Fatalf("server certificate %d is not trusted by the generated CA: %v", index, err)
		}
	}
}
