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
// test cases from docs/test/OAuth Test Spec.md:
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
	redirectURI := authCodeRedirectURI(oauthAuthCodeNamespace)

	t.Cleanup(func() {
		if t.Failed() {
			testutil.CollectKubernetesDiagnostics(t, oauthAuthCodeNamespace)
		}
		if retainNamespaceFromEnvironment() {
			t.Logf("Retaining namespace %s because %s=true", oauthAuthCodeNamespace, oauthRetainNamespaceEnvironment)
			return
		}
		testutil.DeleteNamespace(t, oauthAuthCodeNamespace)
	})

	infrastructure := DeployInfrastructure(t, InfrastructureConfig{
		Namespace:   oauthAuthCodeNamespace,
		RedirectURI: redirectURI,
		Image:       image,
	})
	testutil.WaitForStatefulSetReady(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name, 15*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthAuthCodeNamespace, haproxyServiceName, 5*time.Minute)
	testutil.WaitForDeploymentAvailable(t, oauthAuthCodeNamespace, keycloak.Name, 5*time.Minute)
	testutil.WaitForPodReady(t, oauthAuthCodeNamespace, oauthclient.DefaultName, 2*time.Minute)

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
	postManagementJSON(t, oauthAuthCodeNamespace, infrastructure.Cluster.Name, "/manage/v2/servers?group-id=Default&server-type=http", mlmanage.BuildOAuthAppServerPayload(authCodeAppServerConfig()))

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
			StartURL:     haproxyBase + "/",
			CallbackBase: haproxyBase,
			CarrySession: true,
		})
		if !strings.Contains(result["STEP1_STATUS"], "303") && !strings.Contains(result["STEP1_STATUS"], "302") {
			t.Fatalf("expected a redirect (302/303) from the OAuth App Server, got %q\n%s", result["STEP1_STATUS"], result["_raw"])
		}
		if result["STEP1_SESSIONID"] == "" {
			t.Fatalf("expected a SessionID cookie before authentication\n%s", result["_raw"])
		}
		if !strings.Contains(result["STEP1_LOCATION"], "code_challenge") || !strings.Contains(result["STEP1_LOCATION"], "response_type=code") {
			t.Fatalf("expected an Authorization Code (PKCE) redirect to the IdP, got %q", result["STEP1_LOCATION"])
		}
	})

	// TC2: with HAProxy SessionID affinity the callback returns to the initiating
	// node and the Authorization Code flow completes successfully.
	t.Run("TC2_affinity_completes_flow", func(t *testing.T) {
		result := runAuthCodeFlow(t, oauthAuthCodeNamespace, authCodeFlowConfig{
			StartURL:     haproxyBase + "/",
			CallbackBase: haproxyBase,
			CarrySession: true,
		})
		assertHandshakeCompleted(t, result)
	})

	// TC3: a callback delivered to a different node than the one that started the
	// flow fails, because the PKCE verifier and state are node-local.
	t.Run("TC3_cross_node_callback_fails", func(t *testing.T) {
		result := runAuthCodeFlow(t, oauthAuthCodeNamespace, authCodeFlowConfig{
			StartURL:     node0Base + "/",
			CallbackBase: node1Base,
			CarrySession: false,
		})
		assertHandshakeRejected(t, result)
	})

	if retainNamespaceFromEnvironment() {
		t.Logf("Authorization Code infrastructure retained in namespace %s", oauthAuthCodeNamespace)
	}
}

type authCodeFlowConfig struct {
	StartURL     string
	CallbackBase string
	CarrySession bool
}

// runAuthCodeFlow drives a complete browser-style Authorization Code + PKCE login
// through the OAuth App Server and Keycloak from inside the in-cluster curl client,
// returning the parsed key=value markers the script emits.
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
	)
	result := parseMarkers(output)
	result["_raw"] = output
	t.Logf("Authorization Code flow markers:\n%s", output)
	return result
}

func assertHandshakeCompleted(t *testing.T, result map[string]string) {
	t.Helper()
	if result["RESULT"] != "DONE" {
		t.Fatalf("Authorization Code flow did not reach the callback: %s\n%s", result["RESULT"], result["_raw"])
	}
	status := result["STEP4_STATUS"]
	if strings.Contains(status, "401") || strings.Contains(status, "400") || strings.Contains(status, "500") {
		t.Fatalf("callback on the initiating node should complete, got %q\n%s", status, result["_raw"])
	}
	if strings.Contains(result["STEP4_LOCATION"], "/protocol/openid-connect/auth") {
		t.Fatalf("callback should not restart the OAuth flow when affinity holds\n%s", result["_raw"])
	}
}

func assertHandshakeRejected(t *testing.T, result map[string]string) {
	t.Helper()
	if result["RESULT"] != "DONE" {
		t.Fatalf("cross-node negative case did not reach the callback: %s\n%s", result["RESULT"], result["_raw"])
	}
	status := result["STEP4_STATUS"]
	restarted := strings.Contains(result["STEP4_LOCATION"], "/protocol/openid-connect/auth")
	rejected := strings.Contains(status, "401") || strings.Contains(status, "400") || strings.Contains(status, "403") || strings.Contains(status, "500") || restarted
	if !rejected {
		t.Fatalf("cross-node callback should fail without the initiating node's OAuth state, got %q\n%s", status, result["_raw"])
	}
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

// authCodeFlowScript performs a full Authorization Code + PKCE + form_post login
// from the in-cluster curl client. MarkLogic generates the PKCE verifier and
// state, so the script only follows MarkLogic's redirect, submits the Keycloak
// login form, and posts the callback. Positional args:
//
//	$1 START_URL     initial OAuth App Server URL
//	$2 CALLBACK_BASE scheme://host[:port] to send the callback to
//	$3 USERNAME
//	$4 PASSWORD
//	$5 CARRY_SESSION "yes" to send the initiating SessionID cookie on the callback
const authCodeFlowScript = `
START_URL="$1"; CALLBACK_BASE="$2"; USER="$3"; PASS="$4"; CARRY="$5"
JAR=$(mktemp); KCJAR=$(mktemp)

H1=$(curl -sS --fail -c "$JAR" -D - -o /dev/null "$START_URL")
echo "STEP1_STATUS=$(printf '%s' "$H1" | head -1 | tr -d '\r')"
LOC=$(printf '%s' "$H1" | tr -d '\r' | awk 'tolower($1)=="location:"{print $2; exit}')
SID=$(awk '$6=="SessionID"{v=$7} END{print v}' "$JAR")
echo "STEP1_SESSIONID=$SID"
echo "STEP1_LOCATION=$LOC"
if [ -z "$LOC" ]; then echo "RESULT=NO_REDIRECT"; exit 0; fi

LOGIN=$(curl -sk -c "$KCJAR" -b "$KCJAR" "$LOC")
ACTION=$(printf '%s' "$LOGIN" | grep -io 'action="[^"]*login-actions/authenticate[^"]*"' | head -1 | sed -e 's/^[aA][cC][tT][iI][oO][nN]="//' -e 's/"$//' -e 's/&amp;/\&/g')
if [ -z "$ACTION" ]; then ACTION=$(printf '%s' "$LOGIN" | grep -io 'action="[^"]*"' | head -1 | sed -e 's/^[aA][cC][tT][iI][oO][nN]="//' -e 's/"$//' -e 's/&amp;/\&/g'); fi
echo "STEP2_ACTION_PRESENT=$([ -n "$ACTION" ] && echo yes || echo no)"
if [ -z "$ACTION" ]; then echo "RESULT=NO_LOGIN_FORM"; printf '%s' "$LOGIN" | head -40; exit 0; fi

# Keycloak's form_post response emits UPPERCASE HTML (<INPUT NAME="code" VALUE="..."/>)
# while the login page is lowercase, so all attribute parsing must be case-insensitive.
POST=$(curl -sk -c "$KCJAR" -b "$KCJAR" --data-urlencode "username=$USER" --data-urlencode "password=$PASS" --data-urlencode "credentialId=" "$ACTION")
CODE=$(printf '%s' "$POST" | grep -io 'name="code"[^>]*' | head -1 | grep -io 'value="[^"]*"' | head -1 | sed -e 's/^[vV][aA][lL][uU][eE]="//' -e 's/"$//')
STATE=$(printf '%s' "$POST" | grep -io 'name="state"[^>]*' | head -1 | grep -io 'value="[^"]*"' | head -1 | sed -e 's/^[vV][aA][lL][uU][eE]="//' -e 's/"$//')
CBACT=$(printf '%s' "$POST" | grep -io 'action="[^"]*"' | head -1 | sed -e 's/^[aA][cC][tT][iI][oO][nN]="//' -e 's/"$//' -e 's/&amp;/\&/g')
echo "STEP3_CODE_PRESENT=$([ -n "$CODE" ] && echo yes || echo no)"
echo "STEP3_STATE_PRESENT=$([ -n "$STATE" ] && echo yes || echo no)"
if [ -z "$CODE" ] || [ -z "$STATE" ]; then echo "RESULT=NO_CODE"; printf '%s' "$POST" | head -60; exit 0; fi

PATHQ=$(printf '%s' "$CBACT" | sed -e 's#^https\{0,1\}://[^/]*##')
if [ -z "$PATHQ" ]; then PATHQ="/oauth/callback"; fi
CBURL="${CALLBACK_BASE}${PATHQ}"
echo "CALLBACK_URL=$CBURL"

B4=$(mktemp)
if [ "$CARRY" = "yes" ] && [ -n "$SID" ]; then
  H4=$(curl -sk -b "SessionID=$SID" -D - -o "$B4" --data-urlencode "code=$CODE" --data-urlencode "state=$STATE" "$CBURL")
else
  H4=$(curl -sk -D - -o "$B4" --data-urlencode "code=$CODE" --data-urlencode "state=$STATE" "$CBURL")
fi
echo "STEP4_STATUS=$(printf '%s' "$H4" | head -1 | tr -d '\r')"
echo "STEP4_LOCATION=$(printf '%s' "$H4" | tr -d '\r' | awk 'tolower($1)=="location:"{print $2; exit}')"
printf '%s' "$H4" | tr -d '\r' | grep -iE '^(www-authenticate|set-cookie):' | head -5
echo "STEP4_BODY_BEGIN"
head -40 "$B4"
echo "STEP4_BODY_END"
echo "RESULT=DONE"
`
