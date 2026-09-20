// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import "testing"

func completedAuthCodeMarkers() map[string]string {
	return map[string]string{
		"STEP1_STATUS": "302", "STEP1_SESSIONID_PRESENT": "yes", "STEP1_IDP_MATCH": "yes", "STEP1_PKCE_PRESENT": "yes",
		"STEP2_ACTION_PRESENT": "yes", "STEP3_CODE_PRESENT": "yes", "STEP3_STATE_PRESENT": "yes", "STEP3_CALLBACK_MATCH": "yes",
		"STEP4_STATUS": "302", "STEP4_RESTARTED": "no", "STEP5_STATUS": "200", "STEP5_IDENTITY_MATCH": "yes", "RESULT": "DONE",
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
		{"wrong identity", "STEP5_IDENTITY_MATCH", "no", false}, {"missing identity", "STEP5_IDENTITY_MATCH", "", false},
		{"identity endpoint redirects", "STEP5_STATUS", "302", false}, {"identity endpoint forbidden", "STEP5_STATUS", "403", false},
		{"restarted login", "STEP4_RESTARTED", "yes", false}, {"missing restart marker", "STEP4_RESTARTED", "", false},
		{"no code", "STEP3_CODE_PRESENT", "no", false}, {"no state", "STEP3_STATE_PRESENT", "no", false},
		{"wrong callback", "STEP3_CALLBACK_MATCH", "no", false}, {"untrusted IdP", "STEP1_IDP_MATCH", "no", false},
		{"missing session", "STEP1_SESSIONID_PRESENT", "no", false}, {"missing PKCE", "STEP1_PKCE_PRESENT", "no", false},
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
			if err := validateAuthCodeRejection(result); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
