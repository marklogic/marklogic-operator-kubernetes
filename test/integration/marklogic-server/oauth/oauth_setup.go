// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/keycloak"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/marklogiccluster"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/oauthclient"
	tlsfixture "github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/tls"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	oauthCASecretName                  = "oauth-test-ca"
	marklogicCertificate0              = "marklogic-0-tls"
	marklogicCertificate1              = "marklogic-1-tls"
	keycloakCertificate                = "keycloak-tls"
	haproxyCertificate                 = "marklogic-haproxy-tls"
	haproxyCertFileName                = "tls.pem"
	marklogicAdminUsername             = "admin"
	marklogicAdminPassword             = "Admin@8001"
	haproxyServiceName                 = "marklogic-haproxy"
	oauthRedirectURIEnvironment        = "MARKLOGIC_OAUTH_REDIRECT_URI"
	oauthResourceServerTestEnvironment = "MARKLOGIC_OAUTH_RESOURCE_SERVER"
	oauthAuthCodeTestEnvironment       = "MARKLOGIC_OAUTH_AUTHORIZATION_CODE"
	oauthRetainNamespaceEnvironment    = "MARKLOGIC_OAUTH_RETAIN_NAMESPACE"
	marklogicImageEnvironment          = "MARKLOGIC_IMAGE"
	oauthExternalSecurityName          = "keycloak-resource-server"
	oauthAppServerName                 = "oauth-resource-server"
	oauthAppServerPort                 = 8013
	authCodeExternalSecurityName       = "keycloak-authorization-code"
	authCodeAppServerName              = "oauth-authorization-code"
	authCodeCertificateTemplate        = "oauthAuthCodeTemplate"
	authCodeCallbackPath               = "/oauth/callback"
)

type Infrastructure struct {
	RedirectURI string
	TLS         tlsfixture.ClusterResources
	Keycloak    keycloak.Resources
	Cluster     *marklogicv1.MarklogicCluster
	Client      *corev1.Pod
}

type InfrastructureConfig struct {
	Namespace   string
	RedirectURI string
	Image       string
}

type openIDConfiguration struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func (infrastructure Infrastructure) Objects() []runtime.Object {
	objects := make([]runtime.Object, 0, 6+len(infrastructure.TLS.TLSSecrets))
	objects = append(objects, infrastructure.TLS.CASecret)
	for _, secret := range infrastructure.TLS.TLSSecrets {
		objects = append(objects, secret)
	}
	objects = append(objects,
		infrastructure.Keycloak.AdminSecret,
		infrastructure.Keycloak.RealmConfigMap,
		infrastructure.Keycloak.Deployment,
		infrastructure.Keycloak.Service,
	)
	objects = append(objects, infrastructure.Cluster, infrastructure.Client)
	return objects
}

func DeployInfrastructure(t *testing.T, config InfrastructureConfig) Infrastructure {
	t.Helper()
	infrastructure, err := BuildInfrastructure(config)
	if err != nil {
		t.Fatal(err)
	}
	testutil.EnsureNamespace(t, config.Namespace)
	testutil.ApplyObjects(t, infrastructure.Objects()...)
	return infrastructure
}

func BuildInfrastructure(config InfrastructureConfig) (Infrastructure, error) {
	if config.Namespace == "" {
		return Infrastructure{}, fmt.Errorf("namespace is required")
	}
	redirectURI := strings.TrimSpace(config.RedirectURI)
	clusterName := marklogiccluster.DefaultName
	marklogic0DNSNames := marklogicServerDNSNames(clusterName, config.Namespace, 0)
	marklogic1DNSNames := marklogicServerDNSNames(clusterName, config.Namespace, 1)
	if redirectURI != "" {
		callbackHostname, err := callbackHostname(redirectURI)
		if err != nil {
			return Infrastructure{}, err
		}
		marklogic0DNSNames = append(marklogic0DNSNames, callbackHostname)
		marklogic1DNSNames = append(marklogic1DNSNames, callbackHostname)
	}

	tlsResources, err := tlsfixture.BuildClusterResources(tlsfixture.ClusterConfig{
		Namespace:    config.Namespace,
		CASecretName: oauthCASecretName,
		Servers: []tlsfixture.ServerConfig{
			{TLSSecretName: marklogicCertificate0, DNSNames: marklogic0DNSNames},
			{TLSSecretName: marklogicCertificate1, DNSNames: marklogic1DNSNames},
			{TLSSecretName: keycloakCertificate, DNSNames: keycloakDNSNames(config.Namespace)},
			{TLSSecretName: haproxyCertificate, DNSNames: haproxyDNSNames(config.Namespace), PEMFileName: haproxyCertFileName},
		},
	})
	if err != nil {
		return Infrastructure{}, err
	}
	keycloakResources, err := keycloak.BuildResources(keycloak.Config{
		Namespace:     config.Namespace,
		RedirectURI:   redirectURI,
		IssuerURL:     keycloakBaseURL(config.Namespace),
		TLSSecretName: keycloakCertificate,
	})
	if err != nil {
		return Infrastructure{}, err
	}
	cluster, err := marklogiccluster.Build(marklogiccluster.Config{
		Namespace:              config.Namespace,
		Name:                   clusterName,
		Image:                  config.Image,
		AdminUsername:          marklogicAdminUsername,
		AdminPassword:          marklogicAdminPassword,
		CASecretName:           tlsResources.CASecret.Name,
		CertificateSecretNames: []string{marklogicCertificate0, marklogicCertificate1},
	})
	if err != nil {
		return Infrastructure{}, err
	}
	cluster.Spec.HAProxy.AppServers = append(cluster.Spec.HAProxy.AppServers, marklogicv1.AppServers{
		Name: oauthAppServerName,
		Port: oauthAppServerPort,
	})
	// Terminate TLS at the HAProxy frontend so it can run in HTTP mode and apply
	// SessionID cookie affinity while still re-encrypting to the HTTPS MarkLogic
	// App Servers. Without frontend TLS the OAuth App Server port would receive a
	// raw TLS handshake on a plaintext HTTP listener and reject it as a bad request.
	cluster.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{
		Enabled:      true,
		SecretName:   haproxyCertificate,
		CertFileName: haproxyCertFileName,
	}
	client, err := oauthclient.Build(oauthclient.Config{Namespace: config.Namespace, CASecretName: tlsResources.CASecret.Name})
	if err != nil {
		return Infrastructure{}, err
	}
	return Infrastructure{
		RedirectURI: redirectURI,
		TLS:         tlsResources,
		Keycloak:    keycloakResources,
		Cluster:     cluster,
		Client:      client,
	}, nil
}

func callbackHostname(redirectURI string) (string, error) {
	parsed, err := url.ParseRequestURI(redirectURI)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", fmt.Errorf("redirect URI must be an HTTPS URL with a hostname")
	}
	if net.ParseIP(parsed.Hostname()) != nil {
		return "", fmt.Errorf("redirect URI hostname must be a DNS name")
	}
	return parsed.Hostname(), nil
}

func redirectURIFromEnvironment() (string, bool) {
	redirectURI := strings.TrimSpace(os.Getenv(oauthRedirectURIEnvironment))
	return redirectURI, redirectURI != ""
}

func resourceServerTestEnabledFromEnvironment() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(oauthResourceServerTestEnvironment)))
	return err == nil && enabled
}

func authCodeTestEnabledFromEnvironment() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(oauthAuthCodeTestEnvironment)))
	return err == nil && enabled
}

func marklogicImageFromEnvironment() string {
	return strings.TrimSpace(os.Getenv(marklogicImageEnvironment))
}

func retainNamespaceFromEnvironment() bool {
	retain, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(oauthRetainNamespaceEnvironment)))
	return err == nil && retain
}

func parseOpenIDConfiguration(body []byte) (openIDConfiguration, error) {
	var configuration openIDConfiguration
	if err := json.Unmarshal(body, &configuration); err != nil {
		return openIDConfiguration{}, fmt.Errorf("decode OpenID discovery document: %w", err)
	}
	for field, value := range map[string]string{
		"issuer":                 configuration.Issuer,
		"authorization endpoint": configuration.AuthorizationEndpoint,
		"token endpoint":         configuration.TokenEndpoint,
		"JWKS URI":               configuration.JWKSURI,
	} {
		parsed, err := url.ParseRequestURI(value)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
			return openIDConfiguration{}, fmt.Errorf("OpenID discovery %s must be an HTTPS URL", field)
		}
	}
	return configuration, nil
}

func resourceServerExternalSecurityConfig(document openIDConfiguration) mlmanage.OAuthExternalSecurityConfig {
	return mlmanage.OAuthExternalSecurityConfig{
		Name:              oauthExternalSecurityName,
		Authentication:    "oauth",
		Authorization:     "internal",
		CacheTimeout:      300,
		FlowType:          "Resource server",
		Vendor:            "Other",
		ClientID:          keycloak.ClientID,
		TokenType:         "JSON Web Tokens",
		UsernameAttribute: "preferred_username",
		RoleAttribute:     "roles",
		JWTIssuerURI:      document.Issuer,
		JWTAlgorithm:      "RS256",
		JWKSURI:           document.JWKSURI,
	}
}

func resourceServerAppServerConfig() mlmanage.OAuthAppServerConfig {
	return mlmanage.OAuthAppServerConfig{
		Name:                   oauthAppServerName,
		Group:                  "Default",
		Root:                   "/",
		Port:                   oauthAppServerPort,
		ContentDatabase:        "Documents",
		ExternalSecurityName:   oauthExternalSecurityName,
		TLSCertificateTemplate: "defaultTemplate",
	}
}

func authCodeExternalSecurityConfig(document openIDConfiguration, redirectURI string) mlmanage.OAuthExternalSecurityConfig {
	return mlmanage.OAuthExternalSecurityConfig{
		Name:                   authCodeExternalSecurityName,
		Authentication:         "oauth",
		Authorization:          "internal",
		CacheTimeout:           300,
		FlowType:               "Authorization code",
		Vendor:                 "Other",
		ClientID:               keycloak.AuthCodeClientID,
		ClientSecret:           keycloak.AuthCodeClientSecret,
		RedirectURI:            redirectURI,
		AuthorizationServerURI: document.AuthorizationEndpoint,
		TokenServerURI:         document.TokenEndpoint,
		TokenType:              "JSON Web Tokens",
		UsernameAttribute:      "preferred_username",
		RoleAttribute:          "roles",
		JWTIssuerURI:           document.Issuer,
		JWTAlgorithm:           "RS256",
		JWKSURI:                document.JWKSURI,
	}
}

func authCodeAppServerConfig() mlmanage.OAuthAppServerConfig {
	return mlmanage.OAuthAppServerConfig{
		Name:                   authCodeAppServerName,
		Group:                  "Default",
		Root:                   "/",
		Port:                   oauthAppServerPort,
		ContentDatabase:        "Documents",
		ExternalSecurityName:   authCodeExternalSecurityName,
		TLSCertificateTemplate: "defaultTemplate",
	}
}

// authCodeRedirectURI returns the HTTPS callback URL, routed through the operator
// HAProxy service on the OAuth App Server port, that MarkLogic advertises to the
// authorization server and that Keycloak registers as a valid redirect URI.
func authCodeRedirectURI(namespace string) string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d%s", haproxyServiceName, namespace, oauthAppServerPort, authCodeCallbackPath)
}

func marklogicServerDNSNames(clusterName, namespace string, ordinal int) []string {
	podDNSName := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", clusterName, ordinal, clusterName, namespace)
	return []string{
		podDNSName,
		haproxyServiceName,
		fmt.Sprintf("%s.%s", haproxyServiceName, namespace),
		fmt.Sprintf("%s.%s.svc", haproxyServiceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", haproxyServiceName, namespace),
	}
}

func marklogicManagementURL(clusterName, namespace string) string {
	return "https://" + marklogicServerDNSNames(clusterName, namespace, 0)[0] + ":8002"
}

func keycloakIssuerURL(namespace string) string {
	return keycloakBaseURL(namespace) + "/realms/" + keycloak.Realm
}

func keycloakBaseURL(namespace string) string {
	return "https://" + keycloakDNSNames(namespace)[3] + ":8443"
}

func keycloakDiscoveryURL(namespace string) string {
	return keycloakIssuerURL(namespace) + "/.well-known/openid-configuration"
}

func haproxyDNSNames(namespace string) []string {
	return []string{
		haproxyServiceName,
		fmt.Sprintf("%s.%s", haproxyServiceName, namespace),
		fmt.Sprintf("%s.%s.svc", haproxyServiceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", haproxyServiceName, namespace),
	}
}

func keycloakDNSNames(namespace string) []string {
	return []string{
		keycloak.Name,
		fmt.Sprintf("%s.%s", keycloak.Name, namespace),
		fmt.Sprintf("%s.%s.svc", keycloak.Name, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", keycloak.Name, namespace),
	}
}
