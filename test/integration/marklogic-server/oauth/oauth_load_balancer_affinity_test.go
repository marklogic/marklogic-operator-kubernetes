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

const oauthSessionAffinityNamespace = "ml-oauth-session-affinity"

func TestOAuthResourceServerInfrastructure(t *testing.T) {
	if !resourceServerTestEnabledFromEnvironment() {
		t.Skipf("set %s=true to run OAuth resource-server infrastructure setup", oauthResourceServerTestEnvironment)
	}
	run := testutil.NewRun(t, "ml-oauth-resource-server", true)
	oauthSessionAffinityNamespace := run.Namespace
	redirectURI, _ := redirectURIFromEnvironment()

	infrastructure := DeployInfrastructure(t, run, InfrastructureConfig{
		Namespace:   oauthSessionAffinityNamespace,
		RedirectURI: redirectURI,
		Image:       marklogicImageFromEnvironment(),
	})
	testutil.WaitForStatefulSetReady(t, oauthSessionAffinityNamespace, infrastructure.Cluster.Name, 15*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthSessionAffinityNamespace, haproxyServiceName, 5*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthSessionAffinityNamespace, keycloak.Name, 5*time.Minute)
	testutil.WaitForPodReady(t, oauthSessionAffinityNamespace, oauthclient.DefaultName, 2*time.Minute)
	run.LogImages(t)
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
	// Install the same protected identity probe on both nodes. A plain HTTP
	// App Server does not automatically provide the REST API endpoints.
	const probeRoot = "/tmp/oauth-resource-server/"
	const probe = `xquery version "1.0-ml";
xdmp:set-response-content-type("text/plain"),
fn:concat("oauth-user:", xdmp:get-current-user())`
	for node := 0; node < 2; node++ {
		testutil.ExecuteInPod(t, oauthSessionAffinityNamespace,
			fmt.Sprintf("%s-%d", infrastructure.Cluster.Name, node), "marklogic-server",
			"sh", "-c", `mkdir -p "$1" && printf '%s' "$2" > "$1/identity.xqy"`,
			"install-oauth-probe", probeRoot, probe)
	}
	// Match the disposable identity mapping used by the Authorization Code
	// scenario. This grants admin only inside this disposable test cluster.
	assignExternalNameToAdmin(t, oauthSessionAffinityNamespace, infrastructure.Cluster.Name, keycloak.TestUsername)
	appConfig := resourceServerAppServerConfig()
	appConfig.Root = probeRoot
	appServerPayload, err := json.Marshal(mlmanage.BuildOAuthAppServerPayload(appConfig))
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

	baseURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", haproxyServiceName, oauthSessionAffinityNamespace, oauthAppServerPort)
	waitForOAuthAppServerThroughHAProxy(t, oauthSessionAffinityNamespace, baseURL, 2*time.Minute)
	// Acquire the token after setup and backend readiness so its lifetime is
	// available for requests rather than consumed by cluster configuration.
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
	for _, tc := range []struct {
		name, bearer, status, body, rejection string
	}{
		{"valid_token", token.AccessToken, "200", "oauth-user:" + keycloak.TestUsername, ""},
		{"missing_token", "", "", "", "XDMP-OAUTH: Access token provided is empty."},
		{"invalid_token", "not-a-valid-jwt", "", "", "XDMP-INTERNAL: Internal error: invalid token supplied"},
		// A fresh request must still succeed after the negative cases.
		{"valid_token_after_rejection", token.AccessToken, "200", "oauth-user:" + keycloak.TestUsername, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No cookie jar or redirects: each request independently tests bearer
			// authentication, and a redirect cannot masquerade as success.
			args := []string{"curl", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30",
				"--write-out", "\n%{http_code}"}
			if tc.bearer != "" {
				args = append(args, "--header", "Authorization: Bearer "+tc.bearer)
			}
			args = append(args, baseURL+"/identity.xqy")
			output := testutil.ExecuteInPod(t, oauthSessionAffinityNamespace, oauthclient.DefaultName, "curl", args...)
			var err error
			if tc.rejection != "" {
				err = validateBearerRejection(output, tc.rejection)
			} else {
				err = validateBearerResponse(output, tc.status, tc.body)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// validateBearerResponse requires an exact HTTP status and, for successful
// requests, the identity produced by our protected module. Do not log response
// bodies here: authentication error responses can contain token details.
func validateBearerResponse(output, wantStatus, wantBody string) error {
	body, status, err := parseBearerResponse(output)
	if err != nil {
		return err
	}
	if status != wantStatus {
		return fmt.Errorf("bearer request HTTP status = %q, want %s", status, wantStatus)
	}
	if wantBody != "" && strings.TrimSpace(body) != wantBody {
		return fmt.Errorf("bearer request did not return the expected authenticated identity")
	}
	return nil
}

// MarkLogic 12.0.3 reports these authentication failures as HTTP 500. Accept
// only the specific error for the supplied input; an empty-token error for a
// malformed token must fail because it indicates a lost Authorization header.
func validateBearerRejection(output, expectedError string) error {
	body, status, err := parseBearerResponse(output)
	if err != nil {
		return err
	}
	if status == "401" {
		return nil
	}
	if status == "500" && expectedError != "" && strings.Contains(body, "<dt>"+expectedError+"</dt>") {
		return nil
	}
	return fmt.Errorf("bearer rejection HTTP status = %q; expected 401 or the specific MarkLogic authentication error %q", status, expectedError)
}

func parseBearerResponse(output string) (string, string, error) {
	output = strings.TrimSuffix(output, "\n")
	index := strings.LastIndex(output, "\n")
	if index < 0 {
		return "", "", fmt.Errorf("bearer response is missing the HTTP status delimiter")
	}
	return output[:index], output[index+1:], nil
}
