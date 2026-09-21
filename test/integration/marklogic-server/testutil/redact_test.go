// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRedactorRemovesCredentialsAndPreservesDiagnostics(t *testing.T) {
	r := newRedactor()
	r.add("fixture-password")
	r.discover(map[string]any{"realm.json": `{"clients":[{"secret":"client-value"}],"users":[{"credentials":[{"type":"password","value":"keycloak-password"}]}]}`, "adminPassword": "admin-value", "env": []any{map[string]any{"name": "DB_PASSWORD", "value": "env-password"}}})
	for _, tc := range []struct{ name, input, secret string }{
		{"known secret", "failure: fixture-password", "fixture-password"},
		{"encoded secret", base64.StdEncoding.EncodeToString([]byte("fixture-password")), base64.StdEncoding.EncodeToString([]byte("fixture-password"))},
		{"client secret", "client-value", "client-value"},
		{"admin password", "admin-value", "admin-value"},
		{"environment password", "env-password", "env-password"},
		{"Keycloak credential", "keycloak-password", "keycloak-password"},
		{"bearer header", "Authorization: Bearer dynamic-token", "dynamic-token"},
		{"standalone bearer", "received Bearer opaque-token", "opaque-token"},
		{"state error", "XDMP-OAUTH: Novel OAuth state unseen-state; potential CSRF attack", "unseen-state"},
		{"cookie header", "Set-Cookie: SessionID=dynamic-cookie; Secure", "dynamic-cookie"},
		{"token JSON", `{"access_token":"dynamic-token", "refresh_token":"refresh-value"}`, "dynamic-token"},
		{"password with spaces", `{"password":"a secret with spaces"}`, "a secret with spaces"},
		{"callback URL", "https://example.test/callback?code=auth-code&state=oauth-state", "auth-code"},
		{"state URL", "https://example.test/callback?code=auth-code&state=oauth-state", "oauth-state"},
		{"JWT", "error eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.signature failed", "eyJhbGci"},
		{"userinfo", "https://alice:credential@example.test", "credential"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nprivate-material\n-----END RSA PRIVATE KEY-----", "private-material"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.text(tc.input); strings.Contains(got, tc.secret) {
				t.Errorf("redaction failed for %s", tc.name)
			}
		})
	}
	const safe = "pod marklogic-0: CrashLoopBackOff; HTTP 503; exit code 1"
	if got := r.text(safe); got != safe {
		t.Errorf("lost useful diagnostics: %q", got)
	}
}
