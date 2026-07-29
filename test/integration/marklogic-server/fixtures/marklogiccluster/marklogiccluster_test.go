// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package marklogiccluster

import "testing"

func TestBuildCreatesTwoNodeTLSHAProxyTopology(t *testing.T) {
	cluster, err := Build(Config{
		Namespace:              "oauth-test",
		AdminUsername:          "admin",
		AdminPassword:          "Admin@8001",
		CASecretName:           "test-ca",
		CertificateSecretNames: []string{"marklogic-0-tls", "marklogic-1-tls"},
	})
	if err != nil {
		t.Fatalf("Build returned an error: %v", err)
	}
	if cluster.Spec.Image != DefaultImage {
		t.Fatalf("Image = %q, want %q", cluster.Spec.Image, DefaultImage)
	}
	if len(cluster.Spec.MarkLogicGroups) != 1 || cluster.Spec.MarkLogicGroups[0].Replicas == nil || *cluster.Spec.MarkLogicGroups[0].Replicas != 2 {
		t.Fatalf("MarkLogic groups = %#v, want one group with two replicas", cluster.Spec.MarkLogicGroups)
	}
	if !cluster.Spec.MarkLogicGroups[0].IsBootstrap {
		t.Fatal("MarkLogic group is not the bootstrap group")
	}
	if cluster.Spec.Tls == nil || cluster.Spec.Tls.CaSecretName != "test-ca" || len(cluster.Spec.Tls.CertSecretNames) != 2 {
		t.Fatalf("TLS configuration = %#v, want configured CA and two certificate Secrets", cluster.Spec.Tls)
	}
	if cluster.Spec.HAProxy == nil || !cluster.Spec.HAProxy.Enabled || cluster.Spec.HAProxy.FrontendPort != 8000 {
		t.Fatalf("HAProxy configuration = %#v, want enabled HAProxy on port 8000", cluster.Spec.HAProxy)
	}
}

func TestBuildRequiresTopologyConfiguration(t *testing.T) {
	validConfig := Config{
		Namespace:              "oauth-test",
		AdminUsername:          "admin",
		AdminPassword:          "Admin@8001",
		CASecretName:           "test-ca",
		CertificateSecretNames: []string{"marklogic-0-tls", "marklogic-1-tls"},
	}
	for name, config := range map[string]Config{
		"namespace":                {AdminUsername: validConfig.AdminUsername, AdminPassword: validConfig.AdminPassword, CASecretName: validConfig.CASecretName, CertificateSecretNames: validConfig.CertificateSecretNames},
		"admin credentials":        {Namespace: validConfig.Namespace, CASecretName: validConfig.CASecretName, CertificateSecretNames: validConfig.CertificateSecretNames},
		"CA Secret":                {Namespace: validConfig.Namespace, AdminUsername: validConfig.AdminUsername, AdminPassword: validConfig.AdminPassword, CertificateSecretNames: validConfig.CertificateSecretNames},
		"certificate Secret names": {Namespace: validConfig.Namespace, AdminUsername: validConfig.AdminUsername, AdminPassword: validConfig.AdminPassword, CASecretName: validConfig.CASecretName},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Build(config); err == nil {
				t.Fatal("Build accepted an incomplete topology configuration")
			}
		})
	}
}
