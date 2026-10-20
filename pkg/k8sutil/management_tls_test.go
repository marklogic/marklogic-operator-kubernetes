// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagementTLSOptionsUsesTrustedCA(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(nil)
	defer server.Close()
	caCertificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "trusted-ca", Namespace: "test"},
		Data:       map[string][]byte{markLogicCACertificateKey: caCertificate},
	}
	operatorContext := &OperatorContext{
		Ctx:    context.Background(),
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(caSecret).Build(),
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "test"},
			Spec: marklogicv1.MarklogicGroupSpec{Tls: &marklogicv1.Tls{
				EnableOnDefaultAppServers: true,
				CaSecretName:              caSecret.Name,
			}},
		},
	}

	useTLS, insecureSkipVerify, actualCA, err := operatorContext.managementTLSOptions()
	if err != nil {
		t.Fatalf("managementTLSOptions: %v", err)
	}
	if !useTLS || insecureSkipVerify {
		t.Fatalf("useTLS = %t, insecureSkipVerify = %t; want true, false", useTLS, insecureSkipVerify)
	}
	if string(actualCA) != string(caCertificate) {
		t.Fatal("managementTLSOptions did not return the configured CA")
	}
}

func TestManagementTLSOptionsRejectsInvalidCA(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-ca", Namespace: "test"},
		Data:       map[string][]byte{markLogicCACertificateKey: []byte("not a certificate")},
	}
	operatorContext := &OperatorContext{
		Ctx:    context.Background(),
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(caSecret).Build(),
		MarklogicGroup: &marklogicv1.MarklogicGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: "test"},
			Spec: marklogicv1.MarklogicGroupSpec{Tls: &marklogicv1.Tls{
				EnableOnDefaultAppServers: true,
				CaSecretName:              caSecret.Name,
			}},
		},
	}

	_, _, _, err := operatorContext.managementTLSOptions()
	if err == nil || !strings.Contains(err.Error(), "valid PEM certificate") {
		t.Fatalf("managementTLSOptions error = %v, want invalid PEM error", err)
	}
}
