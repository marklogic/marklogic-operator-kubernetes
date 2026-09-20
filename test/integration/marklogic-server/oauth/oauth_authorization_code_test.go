// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/keycloak"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/oauthclient"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
)

const oauthAuthCodeNamespace = "ml-oauth-authorization-code"

// TestOAuthAuthorizationCodeInfrastructure exercises the OAuth 2.0 Authorization
// Code flow behind the operator-managed HAProxy load balancer on MarkLogic 12.1+,
// where the flow is supported. It configures an Authorization Code external
// security and OAuth App Server, then runs the load-balancer session-affinity
// cases from the original release requirement (see README.md):
//
//	TC1 - the OAuth App Server sets a SessionID cookie and redirects to the IdP
//	      before authentication.
//	TC2 - with HAProxy SessionID affinity, the callback returns to the initiating
//	      node and the flow completes.
//	TC3 - a callback delivered to a different node fails because the in-flight
//	      PKCE/state is node-local.
func TestOAuthAuthorizationCodeInfrastructure(t *testing.T) {
	if !authCodeTestEnabledFromEnvironment() {
		t.Skipf("set %s=true to run the OAuth Authorization Code infrastructure test", oauthAuthCodeTestEnvironment)
	}
	image := marklogicImageFromEnvironment()
	if image == "" {
		t.Fatalf("set %s to a MarkLogic 12.1+ image; the Authorization Code flow is deprecated/rejected on 12.0.x", marklogicImageEnvironment)
	}
	run := testutil.NewRun(t, "ml-oauth-authorization-code", true)
	oauthAuthCodeNamespace := run.Namespace
	redirectURI := authCodeRedirectURI(oauthAuthCodeNamespace)

	infrastructure := DeployInfrastructure(t, run, InfrastructureConfig{
		Namespace:   oauthAuthCodeNamespace,
		RedirectURI: redirectURI,
		Image:       image,
	})
	testutil.WaitForStatefulSetReady(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name, 15*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthAuthCodeNamespace, haproxyServiceName, 5*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthAuthCodeNamespace, keycloak.Name, 5*time.Minute)
	testutil.WaitForPodReady(t, oauthAuthCodeNamespace, oauthclient.DefaultName, 2*time.Minute)
	run.LogImages(t)

	document := discoverKeycloak(t, oauthAuthCodeNamespace)
	if document.AuthorizationEndpoint == "" {
		t.Fatalf("Keycloak discovery did not advertise an authorization endpoint")
	}
	importCertificateAuthority(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name)

	// Configure the Authorization Code external security and OAuth App Server.
	externalSecurityPayload, err := mlmanage.BuildOAuthExternalSecurityPayload(authCodeExternalSecurityConfig(document, redirectURI))
	if err != nil {
		t.Fatalf("Build Authorization Code external-security payload: %v", err)
	}
	postManagementJSON(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name, "/manage/v2/external-security", externalSecurityPayload)
	probeRoot := installOAuthIdentityProbe(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name)
	appConfig := authCodeAppServerConfig()
	appConfig.Root = probeRoot
	postManagementJSON(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name, "/manage/v2/servers?group-id=Default&server-type=http", mlmanage.BuildOAuthAppServerPayload(appConfig))

	// Grant the disposable Keycloak identity a mapped MarkLogic role so a
	// completed handshake yields an authorized 200 rather than only a 403.
	assignExternalNameToAdmin(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name, keycloak.TestUsername)

	haproxyBase := fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", haproxyServiceName, oauthAuthCodeNamespace, oauthAppServerPort)
	node0Base := fmt.Sprintf("https://%s-0.%s.%s.svc.cluster.local:%d", infrastructure.Cluster.Name, infrastructure.Cluster.Name, oauthAuthCodeNamespace, oauthAppServerPort)
	node1Base := fmt.Sprintf("https://%s-1.%s.%s.svc.cluster.local:%d", infrastructure.Cluster.Name, infrastructure.Cluster.Name, oauthAuthCodeNamespace, oauthAppServerPort)

	// Wait for HAProxy's OAuth backend to pass its periodic health check before
	// exercising the flow; the backend briefly reports 503 <NOSRV> right after the
	// OAuth App Server is created, which would otherwise race the assertions.
	waitForOAuthAppServerThroughHAProxy(t, oauthAuthCodeNamespace, haproxyBase, 2*time.Minute)

	// TC1: the OAuth App Server must redirect to Keycloak and set SessionID
	// before authentication, when reached through the HAProxy load balancer.
	t.Run("TC1_SessionID_before_authentication", func(t *testing.T) {
		result := runAuthCodeFlow(t, oauthAuthCodeNamespace, authCodeFlowConfig{
			StartOnly:             true,
			StartURL:              haproxyBase + "/identity.xqy",
			CallbackBase:          haproxyBase,
			CarrySession:          true,
			AuthorizationEndpoint: document.AuthorizationEndpoint,
			RedirectURI:           redirectURI,
		})
		if err := validateAuthCodeStart(result); err != nil {
			t.Fatal(err)
		}
	})

	// TC2: with HAProxy SessionID affinity the callback returns to the initiating
	// node and the Authorization Code flow completes successfully.
	t.Run("TC2_affinity_completes_flow", func(t *testing.T) {
		result := runAuthCodeFlow(t, oauthAuthCodeNamespace, authCodeFlowConfig{
			StartURL:              haproxyBase + "/identity.xqy",
			CallbackBase:          haproxyBase,
			CarrySession:          true,
			AuthorizationEndpoint: document.AuthorizationEndpoint,
			RedirectURI:           redirectURI,
		})
		if err := validateAuthCodeSuccess(result); err != nil {
			t.Fatal(err)
		}
	})

	// TC3: a callback delivered to a different node than the one that started the
	// flow fails, because the PKCE verifier and state are node-local.
	t.Run("TC3_cross_node_callback_fails", func(t *testing.T) {
		result := runAuthCodeFlow(t, oauthAuthCodeNamespace, authCodeFlowConfig{
			StartURL:              node0Base + "/identity.xqy",
			CallbackBase:          node1Base,
			CarrySession:          false,
			AuthorizationEndpoint: document.AuthorizationEndpoint,
			RedirectURI:           redirectURI,
		})
		if err := validateAuthCodeRejection(result); err != nil {
			t.Fatal(err)
		}
	})

	if retainNamespaceFromEnvironment() {
		t.Logf("Authorization Code infrastructure retained in namespace %s", oauthAuthCodeNamespace)
	}
}

type authCodeFlowConfig struct {
	StartURL              string
	CallbackBase          string
	StartOnly             bool
	CarrySession          bool
	AuthorizationEndpoint string
	RedirectURI           string
}

// waitForOAuthAppServerThroughHAProxy blocks until the HAProxy OAuth backend has a
// healthy server. HAProxy's server health check runs periodically, so for a short
// window after the OAuth App Server is created the 8013 backend reports 503 with
// <NOSRV>; running the session-affinity cases before then would race the load
// balancer rather than the flow.
func waitForOAuthAppServerThroughHAProxy(t *testing.T, namespace, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus string
	for {
		lastStatus = strings.TrimSpace(testutil.ExecuteInPod(
			t, namespace, oauthclient.DefaultName, "curl",
			"curl", "--silent", "--output", "/dev/null", "--write-out", "%{http_code}",
			"--head", baseURL+"/",
		))
		if lastStatus != "503" && lastStatus != "000" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("HAProxy OAuth backend %s did not become healthy within %s (last status %q)", baseURL, timeout, lastStatus)
		}
		time.Sleep(3 * time.Second)
	}
}

func runAuthCodeFlow(t *testing.T, namespace string, config authCodeFlowConfig) map[string]string {
	t.Helper()
	carry := "no"
	if config.CarrySession {
		carry = "yes"
	}
	mode := "full"
	if config.StartOnly {
		mode = "start"
	}
	output := testutil.ExecuteInPod(
		t,
		namespace,
		oauthclient.DefaultName,
		"curl",
		"sh", "-c", authCodeFlowScript, "authcode-flow",
		config.StartURL,
		config.CallbackBase,
		keycloak.TestUsername,
		keycloak.TestUserPassword,
		carry,
		oauthclient.CAPath,
		config.AuthorizationEndpoint,
		config.RedirectURI,
		mode,
	)
	result := parseMarkers(output)
	t.Logf("Authorization Code stages: start=%s callback=%s identity=%s result=%s", result["STEP1_STATUS"], result["STEP4_STATUS"], result["STEP5_STATUS"], result["RESULT"])
	return result
}

func discoverKeycloak(t *testing.T, namespace string) openIDConfiguration {
	t.Helper()
	discovery := testutil.ExecuteInPod(
		t, namespace, oauthclient.DefaultName, "curl",
		"curl", "--fail", "--silent", "--show-error", keycloakDiscoveryURL(namespace),
	)
	document, err := parseOpenIDConfiguration([]byte(discovery))
	if err != nil {
		t.Fatalf("Parse Keycloak OpenID discovery document: %v\n%s", err, discovery)
	}
	if document.Issuer != keycloakIssuerURL(namespace) {
		t.Fatalf("Keycloak discovery issuer = %q, want %q", document.Issuer, keycloakIssuerURL(namespace))
	}
	return document
}

func importCertificateAuthority(t *testing.T, namespace, clusterName string) {
	t.Helper()
	testutil.ExecuteInPod(
		t, namespace, oauthclient.DefaultName, "curl",
		"curl", "--fail", "--silent", "--show-error", "--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		"--request", "POST", "--header", "Content-Type: text/plain",
		"--data-binary", "@"+oauthclient.CAPath,
		marklogicManagementURL(clusterName, namespace)+"/manage/v2/certificate-authorities",
	)
}

func postManagementJSON(t *testing.T, namespace, clusterName, path string, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal management payload for %s: %v", path, err)
	}
	testutil.ExecuteInPod(
		t, namespace, oauthclient.DefaultName, "curl",
		"curl", "--fail", "--silent", "--show-error", "--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		"--request", "POST", "--header", "Accept: application/json", "--header", "Content-Type: application/json",
		"--data-binary", string(body),
		marklogicManagementURL(clusterName, namespace)+path,
	)
}

// assignExternalNameToAdmin maps the Keycloak identity to the built-in admin role
// so a completed Authorization Code handshake is authorized (Section 2.4 of the spec).
func assignExternalNameToAdmin(t *testing.T, namespace, clusterName, externalName string) {
	t.Helper()
	payload := map[string]any{"external-name": []string{externalName}}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal admin role external-name payload: %v", err)
	}
	testutil.ExecuteInPod(
		t, namespace, oauthclient.DefaultName, "curl",
		"curl", "--fail", "--silent", "--show-error", "--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		"--request", "PUT", "--header", "Content-Type: application/json",
		"--data-binary", string(body),
		marklogicManagementURL(clusterName, namespace)+"/manage/v2/roles/admin/properties",
	)
}

func parseMarkers(output string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if _, exists := result[key]; !exists {
			result[key] = value
		}
	}
	return result
}
