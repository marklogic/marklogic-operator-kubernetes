// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/mlmanage"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/keycloak"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/oauthclient"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
)

const oauthSessionAffinityNamespace = "ml-oauth-session-affinity"

func TestOAuthResourceServerInfrastructure(t *testing.T) {
	if !resourceServerTestEnabledFromEnvironment() {
		t.Skipf("set %s=true to run OAuth resource-server infrastructure setup", oauthResourceServerTestEnvironment)
	}
	redirectURI, _ := redirectURIFromEnvironment()

	t.Cleanup(func() {
		if t.Failed() {
			testutil.CollectKubernetesDiagnostics(t, oauthSessionAffinityNamespace)
		}
		if retainNamespaceFromEnvironment() {
			t.Logf("Retaining namespace %s because %s=true", oauthSessionAffinityNamespace, oauthRetainNamespaceEnvironment)
			return
		}
		testutil.DeleteNamespace(t, oauthSessionAffinityNamespace)
	})

	infrastructure := DeployInfrastructure(t, InfrastructureConfig{
		Namespace:   oauthSessionAffinityNamespace,
		RedirectURI: redirectURI,
		Image:       marklogicImageFromEnvironment(),
	})
	testutil.WaitForStatefulSetReady(t, oauthSessionAffinityNamespace, infrastructure.Cluster.Name, 15*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthSessionAffinityNamespace, haproxyServiceName, 5*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthSessionAffinityNamespace, keycloak.Name, 5*time.Minute)
	testutil.WaitForPodReady(t, oauthSessionAffinityNamespace, oauthclient.DefaultName, 2*time.Minute)
	discovery := testutil.ExecuteInPod(
		t,
		oauthSessionAffinityNamespace,
		oauthclient.DefaultName,
		"curl",
		"curl",
		"--fail",
		"--silent",
		"--show-error",
		keycloakDiscoveryURL(oauthSessionAffinityNamespace),
	)
	document, err := parseOpenIDConfiguration([]byte(discovery))
	if err != nil {
		t.Fatalf("Parse Keycloak OpenID discovery document: %v\n%s", err, discovery)
	}
	if document.Issuer != keycloakIssuerURL(oauthSessionAffinityNamespace) {
		t.Fatalf("Keycloak discovery issuer = %q, want %q", document.Issuer, keycloakIssuerURL(oauthSessionAffinityNamespace))
	}
	tokenResponse := testutil.ExecuteInPod(
		t,
		oauthSessionAffinityNamespace,
		oauthclient.DefaultName,
		"curl",
		"curl",
		"--fail",
		"--silent",
		"--show-error",
		"--request", "POST",
		"--data-urlencode", "grant_type=password",
		"--data-urlencode", "client_id="+keycloak.ClientID,
		"--data-urlencode", "username="+keycloak.TestUsername,
		"--data-urlencode", "password="+keycloak.TestUserPassword,
		document.TokenEndpoint,
	)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(tokenResponse), &token); err != nil {
		t.Fatalf("Decode Keycloak access token response: %v\n%s", err, tokenResponse)
	}
	if token.AccessToken == "" {
		t.Fatalf("Keycloak access token response did not contain an access token: %s", tokenResponse)
	}
	testutil.ExecuteInPod(
		t,
		oauthSessionAffinityNamespace,
		oauthclient.DefaultName,
		"curl",
		"curl",
		"--fail",
		"--silent",
		"--show-error",
		"--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		"--request", "POST",
		"--header", "Content-Type: text/plain",
		"--data-binary", "@"+oauthclient.CAPath,
		marklogicManagementURL(infrastructure.Cluster.Name, oauthSessionAffinityNamespace)+"/manage/v2/certificate-authorities",
	)
	externalSecurityPayload, err := mlmanage.BuildOAuthExternalSecurityPayload(resourceServerExternalSecurityConfig(document))
	if err != nil {
		t.Fatalf("Build OAuth external-security payload: %v", err)
	}
	payloadBytes, err := json.Marshal(externalSecurityPayload)
	if err != nil {
		t.Fatalf("Marshal OAuth external-security payload: %v", err)
	}
	testutil.ExecuteInPod(
		t,
		oauthSessionAffinityNamespace,
		oauthclient.DefaultName,
		"curl",
		"curl",
		"--fail",
		"--silent",
		"--show-error",
		"--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		"--request", "POST",
		"--header", "Accept: application/json",
		"--header", "Content-Type: application/json",
		"--data-binary", string(payloadBytes),
		marklogicManagementURL(infrastructure.Cluster.Name, oauthSessionAffinityNamespace)+"/manage/v2/external-security",
	)
	appServerPayload, err := json.Marshal(mlmanage.BuildOAuthAppServerPayload(resourceServerAppServerConfig()))
	if err != nil {
		t.Fatalf("Marshal OAuth App Server payload: %v", err)
	}
	testutil.ExecuteInPod(
		t,
		oauthSessionAffinityNamespace,
		oauthclient.DefaultName,
		"curl",
		"curl",
		"--fail",
		"--silent",
		"--show-error",
		"--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		"--request", "POST",
		"--header", "Accept: application/json",
		"--header", "Content-Type: application/json",
		"--data-binary", string(appServerPayload),
		marklogicManagementURL(infrastructure.Cluster.Name, oauthSessionAffinityNamespace)+"/manage/v2/servers?group-id=Default&server-type=http",
	)
	appServerProperties := testutil.ExecuteInPod(
		t,
		oauthSessionAffinityNamespace,
		oauthclient.DefaultName,
		"curl",
		"curl",
		"--fail",
		"--silent",
		"--show-error",
		"--digest",
		"--user", marklogicAdminUsername+":"+marklogicAdminPassword,
		marklogicManagementURL(infrastructure.Cluster.Name, oauthSessionAffinityNamespace)+"/manage/v2/servers/"+oauthAppServerName+"/properties?group-id=Default&format=json",
	)
	var appServer map[string]any
	if err := json.Unmarshal([]byte(appServerProperties), &appServer); err != nil {
		t.Fatalf("Decode OAuth App Server properties: %v\n%s", err, appServerProperties)
	}
	if appServer["port"] != float64(oauthAppServerPort) || appServer["authentication"] != "oauth" || appServer["internal-security"] != false {
		t.Fatalf("OAuth App Server properties = %#v", appServer)
	}
	externalSecurity, ok := appServer["external-security"].([]any)
	if !ok || len(externalSecurity) != 1 || externalSecurity[0] != oauthExternalSecurityName {
		t.Fatalf("OAuth App Server external security = %#v", appServer["external-security"])
	}

	t.Log("OAuth external security and a direct OAuth App Server were created; HAProxy routing and bearer-token assertions are not implemented yet; see docs/test/oauth-load-balancer-affinity-test.md")
}
