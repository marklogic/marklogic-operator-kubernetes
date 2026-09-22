// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import "testing"

func completedAuthCodeMarkers() map[string]string {
	return map[string]string{
		"STEP1_STATUS": "302", "STEP1_COOKIE_PATH": "yes", "STEP1_COOKIE_HTTPONLY": "yes", "STEP1_BACKEND": "node-0", "STEP4_BACKEND": "node-0", "STEP1_AFFINITY": "affinity", "STEP4_AFFINITY": "affinity", "STEP4_SESSION_PRESERVED": "yes", "STEP1_SESSIONID_PRESENT": "yes", "STEP1_IDP_MATCH": "yes", "STEP1_PKCE_PRESENT": "yes",
		"STEP2_ACTION_PRESENT": "yes", "STEP3_CODE_PRESENT": "yes", "STEP3_STATE_PRESENT": "yes", "STEP3_CALLBACK_MATCH": "yes",
		"STEP4_STATUS": "302", "STEP4_RESTARTED": "no", "STEP5_STATUS": "200", "STEP5_BACKEND": "node-0", "STEP5_SESSION_PRESERVED": "yes", "STEP5_IDENTITY_MATCH": "yes", "RESULT": "DONE",
	}
}

func TestAuthCodeSuccessRequiresAuthenticatedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
		valid            bool
	}{
		{"completed", "STEP4_STATUS", "302", true}, {"callback 200", "STEP4_STATUS", "200", true}, {"callback 303", "STEP4_STATUS", "303", true},
		{"forbidden callback", "STEP4_STATUS", "403", false}, {"unrelated 500", "STEP4_STATUS", "500", false},
		{"empty callback status", "STEP4_STATUS", "", false}, {"substring status", "STEP4_STATUS", "not-302", false},
		{"final request lost cookie", "STEP5_SESSION_PRESERVED", "no", false},
		{"final request changed backend", "STEP5_BACKEND", "node-1", false},
		{"wrong identity", "STEP5_IDENTITY_MATCH", "no", false}, {"missing identity", "STEP5_IDENTITY_MATCH", "", false},
		{"identity endpoint redirects", "STEP5_STATUS", "302", false}, {"identity endpoint forbidden", "STEP5_STATUS", "403", false},
		{"restarted login", "STEP4_RESTARTED", "yes", false}, {"missing restart marker", "STEP4_RESTARTED", "", false},
		{"no code", "STEP3_CODE_PRESENT", "no", false}, {"no state", "STEP3_STATE_PRESENT", "no", false},
		{"wrong callback", "STEP3_CALLBACK_MATCH", "no", false}, {"untrusted IdP", "STEP1_IDP_MATCH", "no", false},
		{"missing session", "STEP1_SESSIONID_PRESENT", "no", false}, {"missing PKCE", "STEP1_PKCE_PRESENT", "no", false},
		{"initial 303 redirect", "STEP1_STATUS", "303", true},
		{"initial 200 is not a redirect", "STEP1_STATUS", "200", false},
		{"initial 307 is not accepted", "STEP1_STATUS", "307", false},
		{"initial 308 is not accepted", "STEP1_STATUS", "308", false},
		{"missing initial status", "STEP1_STATUS", "", false},
		{"different backend", "STEP4_BACKEND", "node-1", false},
		{"unobserved backend", "STEP1_BACKEND", "unknown", false},
		{"missing cookie proof", "STEP4_SESSION_PRESERVED", "", false},
		{"wrong mode", "STEP4_AFFINITY", "no-affinity", false},
		{"initial unauthorized", "STEP1_STATUS", "401", false}, {"incomplete", "RESULT", "NO_CODE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := completedAuthCodeMarkers()
			result[tc.key] = tc.value
			if err := validateAuthCodeSuccess(result); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestAuthCodeRejectionRequiresStateSpecificError(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		valid                bool
	}{
		{"known error", "500", "NOVEL_OAUTH_STATE", true}, {"authentication error", "401", "NOVEL_OAUTH_STATE", true},
		{"generic 500", "500", "OTHER", false}, {"generic 401", "401", "OTHER", false},
		{"forbidden", "403", "OTHER", false}, {"restart redirect", "302", "OTHER", false},
		{"success body contains error", "200", "NOVEL_OAUTH_STATE", false}, {"gateway failure", "502", "NOVEL_OAUTH_STATE", false},
		{"empty status", "", "NOVEL_OAUTH_STATE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := completedAuthCodeMarkers()
			result["STEP4_STATUS"] = tc.status
			result["STEP4_ERROR"] = tc.reason
			result["STEP1_AFFINITY"] = "no-affinity"
			result["STEP4_AFFINITY"] = "no-affinity"
			result["STEP4_BACKEND"] = "node-1"
			if err := validateAuthCodeRejection(result); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestAuthCodeRejectionRequiresObservedCrossNodeAndPreservedCookie(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"STEP1_BACKEND", ""}, {"STEP4_BACKEND", "unknown"},
		{"STEP4_BACKEND", "node-0"}, {"STEP4_SESSION_PRESERVED", "no"},
		{"STEP4_SESSION_PRESERVED", ""}, {"STEP1_AFFINITY", "affinity"},
		{"STEP4_AFFINITY", "affinity"},
	} {
		t.Run(tc.key+"_"+tc.value, func(t *testing.T) {
			result := completedAuthCodeMarkers()
			result["STEP1_AFFINITY"], result["STEP4_AFFINITY"] = "no-affinity", "no-affinity"
			result["STEP4_BACKEND"] = "node-1"
			result["STEP4_STATUS"], result["STEP4_ERROR"] = "500", "NOVEL_OAUTH_STATE"
			result[tc.key] = tc.value
			if validateAuthCodeRejection(result) == nil {
				t.Fatal("unproven negative case passed")
			}
		})
	}
}
