// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package keycloak

import (
	"strings"
	"testing"
)

func TestBuildResources(t *testing.T) {
	resources, err := BuildResources(Config{
		Namespace:     "oauth-test",
		RedirectURI:   "https://oauth.example.test/callback",
		IssuerURL:     "https://keycloak.oauth-test.svc.cluster.local:8443",
		TLSSecretName: "keycloak-tls",
	})
	if err != nil {
		t.Fatalf("BuildResources returned an error: %v", err)
	}
	if resources.Deployment.Spec.Template.Spec.Containers[0].Image != Image {
		t.Fatalf("Keycloak image = %q, want %q", resources.Deployment.Spec.Template.Spec.Containers[0].Image, Image)
	}
	realm := resources.RealmConfigMap.Data[RealmConfigMapKey]
	if !strings.Contains(realm, "https://oauth.example.test/callback") {
		t.Fatalf("realm import does not include the redirect URI: %s", realm)
	}
	if !strings.Contains(realm, `"directAccessGrantsEnabled":true`) {
		t.Fatalf("realm import does not enable direct access grants: %s", realm)
	}
	if !strings.Contains(realm, `"emailVerified":true`) {
		t.Fatalf("realm import does not mark the disposable user as fully configured: %s", realm)
	}
	if !strings.Contains(realm, `"email":"oauth-test-user@example.test"`) {
		t.Fatalf("realm import does not provide the disposable user email: %s", realm)
	}
	if !strings.Contains(realm, `"firstName":"OAuth"`) || !strings.Contains(realm, `"lastName":"Test"`) {
		t.Fatalf("realm import does not provide the disposable user name: %s", realm)
	}
	container := resources.Deployment.Spec.Template.Spec.Containers[0]
	if !contains(container.Args, "--http-enabled=false") || !contains(container.Args, "--https-port=8443") {
		t.Fatalf("Keycloak args = %#v, want HTTPS-only configuration", container.Args)
	}
	if !contains(container.Args, "--hostname=https://keycloak.oauth-test.svc.cluster.local:8443") {
		t.Fatalf("Keycloak args = %#v, want issuer URL hostname", container.Args)
	}
	if len(container.VolumeMounts) != 2 || container.VolumeMounts[1].MountPath != "/etc/x509/https" {
		t.Fatalf("Keycloak volume mounts = %#v, want realm import and TLS Secret", container.VolumeMounts)
	}
	if resources.Service.Spec.Ports[0].Port != 8443 || resources.Service.Spec.Ports[0].TargetPort.IntValue() != 8443 {
		t.Fatalf("Keycloak Service port = %#v, want HTTPS port 8443", resources.Service.Spec.Ports)
	}
}

func TestBuildResourcesRequiresNamespaceIssuerAndTLSSecret(t *testing.T) {
	validConfig := Config{
		Namespace:     "oauth-test",
		RedirectURI:   "https://oauth.example.test/callback",
		IssuerURL:     "https://keycloak.oauth-test.svc.cluster.local:8443",
		TLSSecretName: "keycloak-tls",
	}
	if _, err := BuildResources(Config{RedirectURI: validConfig.RedirectURI, IssuerURL: validConfig.IssuerURL, TLSSecretName: validConfig.TLSSecretName}); err == nil {
		t.Fatal("BuildResources accepted an empty namespace")
	}
	resources, err := BuildResources(Config{Namespace: validConfig.Namespace, IssuerURL: validConfig.IssuerURL, TLSSecretName: validConfig.TLSSecretName})
	if err != nil {
		t.Fatalf("BuildResources rejected a resource-server client without a redirect URI: %v", err)
	}
	if realm := resources.RealmConfigMap.Data[RealmConfigMapKey]; strings.Contains(realm, "redirectUris") {
		t.Fatalf("resource-server realm client unexpectedly includes redirect URIs: %s", realm)
	}
	if _, err := BuildResources(Config{Namespace: validConfig.Namespace, TLSSecretName: validConfig.TLSSecretName}); err == nil {
		t.Fatal("BuildResources accepted an empty issuer URL")
	}
	if _, err := BuildResources(Config{Namespace: validConfig.Namespace, IssuerURL: validConfig.IssuerURL}); err == nil {
		t.Fatal("BuildResources accepted an empty TLS Secret name")
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
