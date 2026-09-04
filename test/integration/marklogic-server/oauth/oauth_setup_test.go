// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestBuildInfrastructureCreatesTLSHAProxyTopology(t *testing.T) {
	callbackURI := "https://oauth.example.test/callback"
	resources, err := BuildInfrastructure(InfrastructureConfig{
		Namespace:   oauthSessionAffinityNamespace,
		RedirectURI: callbackURI,
	})
	if err != nil {
		t.Fatalf("BuildInfrastructure returned an error: %v", err)
	}
	if resources.Cluster.Namespace != oauthSessionAffinityNamespace {
		t.Fatalf("cluster namespace = %q, want %q", resources.Cluster.Namespace, oauthSessionAffinityNamespace)
	}
	if appServers := resources.Cluster.Spec.HAProxy.AppServers; len(appServers) != 3 || appServers[2].Name != oauthAppServerName || appServers[2].Port != oauthAppServerPort {
		t.Fatalf("HAProxy App Servers = %#v, want OAuth App Server %q on port %d", appServers, oauthAppServerName, oauthAppServerPort)
	}
	if len(resources.TLS.TLSSecrets) != 4 {
		t.Fatalf("TLS Secret count = %d, want 4", len(resources.TLS.TLSSecrets))
	}
	for index, secret := range resources.TLS.TLSSecrets[:2] {
		certificateBlock, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
		if certificateBlock == nil {
			t.Fatalf("TLS Secret %d does not contain tls.crt", index)
		}
		certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
		if err != nil {
			t.Fatalf("parse TLS certificate %d: %v", index, err)
		}
		for _, hostname := range []string{
			marklogicServerDNSNames(marklogicClusterName(resources), oauthSessionAffinityNamespace, index)[0],
			haproxyServiceName + "." + oauthSessionAffinityNamespace + ".svc.cluster.local",
			"oauth.example.test",
		} {
			if err := certificate.VerifyHostname(hostname); err != nil {
				t.Fatalf("TLS certificate %d does not trust %q: %v", index, hostname, err)
			}
		}
	}
	keycloakCertificate := decodeCertificate(t, resources.TLS.TLSSecrets[2])
	if !containsString(keycloakCertificate.DNSNames, "keycloak."+oauthSessionAffinityNamespace+".svc.cluster.local") {
		t.Fatalf("Keycloak certificate DNS names = %#v, want cluster-local service hostname", keycloakCertificate.DNSNames)
	}
	haproxySecret := resources.TLS.TLSSecrets[3]
	if haproxySecret.Name != haproxyCertificate {
		t.Fatalf("HAProxy TLS Secret name = %q, want %q", haproxySecret.Name, haproxyCertificate)
	}
	haproxyPEM, ok := haproxySecret.Data[haproxyCertFileName]
	if !ok {
		t.Fatalf("HAProxy TLS Secret is missing combined PEM key %q", haproxyCertFileName)
	}
	haproxyCertBlock, _ := pem.Decode(haproxyPEM)
	if haproxyCertBlock == nil {
		t.Fatalf("HAProxy TLS Secret PEM does not contain a certificate")
	}
	haproxyCert, err := x509.ParseCertificate(haproxyCertBlock.Bytes)
	if err != nil {
		t.Fatalf("parse HAProxy certificate: %v", err)
	}
	if err := haproxyCert.VerifyHostname(haproxyServiceName + "." + oauthSessionAffinityNamespace + ".svc.cluster.local"); err != nil {
		t.Fatalf("HAProxy certificate does not trust the load balancer hostname: %v", err)
	}
	if tls := resources.Cluster.Spec.HAProxy.Tls; tls == nil || !tls.Enabled || tls.SecretName != haproxyCertificate || tls.CertFileName != haproxyCertFileName {
		t.Fatalf("HAProxy TLS configuration = %#v, want frontend TLS termination enabled", resources.Cluster.Spec.HAProxy.Tls)
	}
	if resources.Client.Namespace != oauthSessionAffinityNamespace {
		t.Fatalf("client namespace = %q, want %q", resources.Client.Namespace, oauthSessionAffinityNamespace)
	}
	if resources.Client.Spec.Volumes[0].Secret == nil || resources.Client.Spec.Volumes[0].Secret.SecretName != resources.TLS.CASecret.Name {
		t.Fatalf("client CA Secret = %#v, want %q", resources.Client.Spec.Volumes[0].Secret, resources.TLS.CASecret.Name)
	}
	if resources.RedirectURI != callbackURI {
		t.Fatalf("redirect URI = %q, want %q", resources.RedirectURI, callbackURI)
	}
	if realm := resources.Keycloak.RealmConfigMap.Data["realm-marklogic-oauth.json"]; !strings.Contains(realm, callbackURI) {
		t.Fatalf("Keycloak realm does not contain redirect URI %q: %s", callbackURI, realm)
	}
	if !strings.Contains(resources.Keycloak.Deployment.Spec.Template.Spec.Containers[0].Args[len(resources.Keycloak.Deployment.Spec.Template.Spec.Containers[0].Args)-1], "keycloak."+oauthSessionAffinityNamespace+".svc.cluster.local:8443") {
		t.Fatalf("Keycloak hostname argument = %#v, want cluster-local HTTPS issuer", resources.Keycloak.Deployment.Spec.Template.Spec.Containers[0].Args)
	}
}

func TestBuildInfrastructureRequiresNamespace(t *testing.T) {
	if _, err := BuildInfrastructure(InfrastructureConfig{RedirectURI: "https://oauth.example.test/callback"}); err == nil {
		t.Fatal("BuildInfrastructure accepted an empty namespace")
	}
}

func TestBuildInfrastructureRequiresHTTPSRedirectURI(t *testing.T) {
	for _, redirectURI := range []string{"http://oauth.example.test/callback", "https:///callback"} {
		t.Run(redirectURI, func(t *testing.T) {
			if _, err := BuildInfrastructure(InfrastructureConfig{Namespace: oauthSessionAffinityNamespace, RedirectURI: redirectURI}); err == nil {
				t.Fatalf("BuildInfrastructure accepted invalid redirect URI %q", redirectURI)
			}
		})
	}
}

func TestBuildInfrastructureAllowsNoRedirectURI(t *testing.T) {
	resources, err := BuildInfrastructure(InfrastructureConfig{Namespace: oauthSessionAffinityNamespace})
	if err != nil {
		t.Fatalf("BuildInfrastructure rejected resource-server configuration without a redirect URI: %v", err)
	}
	if resources.RedirectURI != "" {
		t.Fatalf("redirect URI = %q, want empty", resources.RedirectURI)
	}
	if realm := resources.Keycloak.RealmConfigMap.Data["realm-marklogic-oauth.json"]; strings.Contains(realm, "redirectUris") {
		t.Fatalf("resource-server realm client unexpectedly includes redirect URIs: %s", realm)
	}
}

func TestRedirectURIFromEnvironment(t *testing.T) {
	const expected = "https://oauth.example.test/callback"
	t.Setenv(oauthRedirectURIEnvironment, expected)
	if redirectURI, ok := redirectURIFromEnvironment(); !ok || redirectURI != expected {
		t.Fatalf("redirectURIFromEnvironment() = (%q, %t), want (%q, true)", redirectURI, ok, expected)
	}

	t.Setenv(oauthRedirectURIEnvironment, "")
	if redirectURI, ok := redirectURIFromEnvironment(); ok || redirectURI != "" {
		t.Fatalf("redirectURIFromEnvironment() = (%q, %t), want (\"\", false)", redirectURI, ok)
	}
}

func TestResourceServerTestEnabledFromEnvironment(t *testing.T) {
	t.Setenv(oauthResourceServerTestEnvironment, "true")
	if !resourceServerTestEnabledFromEnvironment() {
		t.Fatal("resourceServerTestEnabledFromEnvironment() = false, want true")
	}

	t.Setenv(oauthResourceServerTestEnvironment, "false")
	if resourceServerTestEnabledFromEnvironment() {
		t.Fatal("resourceServerTestEnabledFromEnvironment() = true, want false")
	}

	t.Setenv(oauthResourceServerTestEnvironment, "not-a-bool")
	if resourceServerTestEnabledFromEnvironment() {
		t.Fatal("resourceServerTestEnabledFromEnvironment() = true for invalid value")
	}
}

func TestRetainNamespaceFromEnvironment(t *testing.T) {
	t.Setenv(oauthRetainNamespaceEnvironment, "true")
	if !retainNamespaceFromEnvironment() {
		t.Fatal("retainNamespaceFromEnvironment() = false, want true")
	}

	t.Setenv(oauthRetainNamespaceEnvironment, "false")
	if retainNamespaceFromEnvironment() {
		t.Fatal("retainNamespaceFromEnvironment() = true, want false")
	}

	t.Setenv(oauthRetainNamespaceEnvironment, "not-a-bool")
	if retainNamespaceFromEnvironment() {
		t.Fatal("retainNamespaceFromEnvironment() = true for invalid value")
	}
}

func TestParseOpenIDConfiguration(t *testing.T) {
	configuration, err := parseOpenIDConfiguration([]byte(`{
		"issuer":"https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth",
		"authorization_endpoint":"https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth/protocol/openid-connect/auth",
		"token_endpoint":"https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth/protocol/openid-connect/token",
		"jwks_uri":"https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth/protocol/openid-connect/certs"
	}`))
	if err != nil {
		t.Fatalf("parseOpenIDConfiguration returned an error: %v", err)
	}
	if configuration.TokenEndpoint != "https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth/protocol/openid-connect/token" {
		t.Fatalf("token endpoint = %q", configuration.TokenEndpoint)
	}

	if _, err := parseOpenIDConfiguration([]byte(`{"issuer":"http://keycloak.example.test","authorization_endpoint":"https://keycloak.example.test/auth","token_endpoint":"https://keycloak.example.test/token","jwks_uri":"https://keycloak.example.test/certs"}`)); err == nil {
		t.Fatal("parseOpenIDConfiguration accepted an insecure issuer")
	}
}

func TestResourceServerExternalSecurityConfigUsesDiscoveryEndpoints(t *testing.T) {
	document := openIDConfiguration{
		Issuer:  "https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth",
		JWKSURI: "https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth/protocol/openid-connect/certs",
	}
	config := resourceServerExternalSecurityConfig(document)
	if config.FlowType != "Resource server" || config.TokenType != "JSON Web Tokens" {
		t.Fatalf("resource-server configuration = %#v", config)
	}
	if config.Authorization != "internal" || config.Vendor != "Other" || config.CacheTimeout != 300 {
		t.Fatalf("live-verified resource-server configuration = %#v", config)
	}
	if config.JWTIssuerURI != document.Issuer || config.JWKSURI != document.JWKSURI {
		t.Fatalf("discovery URLs were not propagated: %#v", config)
	}
}

func TestResourceServerAppServerConfigUsesDedicatedOAuthPort(t *testing.T) {
	config := resourceServerAppServerConfig()
	if config.Name != oauthAppServerName || config.Port != oauthAppServerPort {
		t.Fatalf("OAuth App Server configuration = %#v", config)
	}
	if config.Group != "Default" || config.Root != "/" || config.ContentDatabase != "Documents" || config.ExternalSecurityName != oauthExternalSecurityName || config.TLSCertificateTemplate != "defaultTemplate" {
		t.Fatalf("OAuth App Server configuration = %#v", config)
	}
}

func TestKeycloakDiscoveryURL(t *testing.T) {
	const namespace = "oauth-test"
	baseURL := "https://keycloak.oauth-test.svc.cluster.local:8443"
	if got := keycloakBaseURL(namespace); got != baseURL {
		t.Fatalf("keycloakBaseURL(%q) = %q, want %q", namespace, got, baseURL)
	}
	want := "https://keycloak.oauth-test.svc.cluster.local:8443/realms/marklogic-oauth/.well-known/openid-configuration"
	if got := keycloakDiscoveryURL(namespace); got != want {
		t.Fatalf("keycloakDiscoveryURL(%q) = %q, want %q", namespace, got, want)
	}
}

func TestMarklogicManagementURL(t *testing.T) {
	const namespace = "oauth-test"
	want := "https://marklogic-0.marklogic.oauth-test.svc.cluster.local:8002"
	if got := marklogicManagementURL("marklogic", namespace); got != want {
		t.Fatalf("marklogicManagementURL() = %q, want %q", got, want)
	}
}

func TestInfrastructureObjectsReturnsAllRequiredResources(t *testing.T) {
	resources, err := BuildInfrastructure(InfrastructureConfig{
		Namespace:   oauthSessionAffinityNamespace,
		RedirectURI: "https://oauth.example.test/callback",
	})
	if err != nil {
		t.Fatalf("BuildInfrastructure returned an error: %v", err)
	}
	objects := resources.Objects()
	if len(objects) != 11 {
		t.Fatalf("object count = %d, want 11", len(objects))
	}
	for index, object := range objects {
		if isNilRuntimeObject(object) {
			t.Fatalf("object %d is nil", index)
		}
	}
}

func isNilRuntimeObject(object runtime.Object) bool {
	return object == nil
}

func marklogicClusterName(resources Infrastructure) string {
	return resources.Cluster.Name
}

func decodeCertificate(t *testing.T, secret *corev1.Secret) *x509.Certificate {
	t.Helper()
	certificateBlock, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
	if certificateBlock == nil {
		t.Fatalf("TLS Secret %q does not contain tls.crt", secret.Name)
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		t.Fatalf("parse TLS certificate %q: %v", secret.Name, err)
	}
	return certificate
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
