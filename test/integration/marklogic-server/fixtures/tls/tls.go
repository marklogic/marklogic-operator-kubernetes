// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package tls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const caCertificateKey = "cacert.pem"

type Config struct {
	Namespace     string
	CASecretName  string
	TLSSecretName string
	DNSNames      []string
}

type Resources struct {
	CASecret  *corev1.Secret
	TLSSecret *corev1.Secret
}

type ClusterConfig struct {
	Namespace    string
	CASecretName string
	Servers      []ServerConfig
}

type ServerConfig struct {
	TLSSecretName string
	DNSNames      []string
	// PEMFileName, when set, produces an Opaque Secret whose single data key is
	// PEMFileName and whose value is the concatenated certificate and private
	// key PEM. HAProxy's `crt` directive expects this combined-PEM form.
	PEMFileName string
}

type ClusterResources struct {
	CASecret   *corev1.Secret
	TLSSecrets []*corev1.Secret
}

// BuildResources creates a short-lived test CA and a TLS server certificate for the supplied DNS names.
func BuildResources(config Config) (Resources, error) {
	clusterResources, err := BuildClusterResources(ClusterConfig{
		Namespace:    config.Namespace,
		CASecretName: config.CASecretName,
		Servers:      []ServerConfig{{TLSSecretName: config.TLSSecretName, DNSNames: config.DNSNames}},
	})
	if err != nil {
		return Resources{}, err
	}
	return Resources{CASecret: clusterResources.CASecret, TLSSecret: clusterResources.TLSSecrets[0]}, nil
}

// BuildClusterResources creates a test CA and TLS Secrets for multiple servers signed by that CA.
func BuildClusterResources(config ClusterConfig) (ClusterResources, error) {
	if config.Namespace == "" {
		return ClusterResources{}, fmt.Errorf("namespace is required")
	}
	if config.CASecretName == "" {
		return ClusterResources{}, fmt.Errorf("CA Secret name is required")
	}
	if len(config.Servers) == 0 {
		return ClusterResources{}, fmt.Errorf("at least one server is required")
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return ClusterResources{}, fmt.Errorf("generate CA key: %w", err)
	}
	caTemplate, err := certificateTemplate("MarkLogic Server integration test CA", true)
	if err != nil {
		return ClusterResources{}, err
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return ClusterResources{}, fmt.Errorf("create CA certificate: %w", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		return ClusterResources{}, fmt.Errorf("parse CA certificate: %w", err)
	}

	resources := ClusterResources{
		CASecret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: config.CASecretName, Namespace: config.Namespace},
			Data:       map[string][]byte{caCertificateKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})},
		},
	}
	for _, server := range config.Servers {
		secret, err := serverSecret(config.Namespace, server, caCertificate, caKey)
		if err != nil {
			return ClusterResources{}, err
		}
		resources.TLSSecrets = append(resources.TLSSecrets, secret)
	}
	return resources, nil
}

func serverSecret(namespace string, config ServerConfig, caCertificate *x509.Certificate, caKey *rsa.PrivateKey) (*corev1.Secret, error) {
	if config.TLSSecretName == "" {
		return nil, fmt.Errorf("TLS Secret name is required")
	}
	if len(config.DNSNames) == 0 {
		return nil, fmt.Errorf("at least one DNS name is required")
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}
	serverTemplate, err := certificateTemplate(config.DNSNames[0], false)
	if err != nil {
		return nil, err
	}
	serverTemplate.DNSNames = config.DNSNames
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create server certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	if config.PEMFileName != "" {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: config.TLSSecretName, Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{config.PEMFileName: append(append([]byte{}, certPEM...), keyPEM...)},
		}, nil
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: config.TLSSecretName, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}, nil
}

func certificateTemplate(commonName string, isCA bool) (*x509.Certificate, error) {
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial number: %w", err)
	}
	return &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  isCA,
		BasicConstraintsValid: true,
	}, nil
}
