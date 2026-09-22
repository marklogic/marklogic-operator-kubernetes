// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"errors"
	"fmt"
)

// Validate only sanitized stage markers and collect independent failures.
// RFC 6749 section 1.7 allows redirect mechanisms beyond its 302 examples;
// MarkLogic uses 303 when starting Authorization Code authentication.
func validateAuthCodeStart(result map[string]string) error {
	var failures []error
	if result["STEP1_STATUS"] != "302" && result["STEP1_STATUS"] != "303" {
		failures = append(failures, fmt.Errorf("initial OAuth redirect must return HTTP 302 or 303 (observed %s)", result["STEP1_STATUS"]))
	}
	for _, key := range []string{"STEP1_SESSIONID_PRESENT", "STEP1_PKCE_PRESENT", "STEP1_IDP_MATCH", "STEP1_COOKIE_PATH", "STEP1_COOKIE_HTTPONLY"} {
		if result[key] != "yes" {
			failures = append(failures, fmt.Errorf("initial redirect check %s failed", key))
		}
	}
	if !knownAuthCodeBackend(result["STEP1_BACKEND"]) {
		failures = append(failures, fmt.Errorf("initial backend identity is unverified"))
	}
	return errors.Join(failures...)
}

func knownAuthCodeBackend(value string) bool { return value == "node-0" || value == "node-1" }

func validateAuthCodeCallback(result map[string]string) error {
	failures := []error{validateAuthCodeStart(result)}
	if result["RESULT"] != "DONE" {
		failures = append(failures, fmt.Errorf("Authorization Code flow did not complete the required stages"))
	}
	for _, key := range []string{"STEP2_ACTION_PRESENT", "STEP3_CODE_PRESENT", "STEP3_STATE_PRESENT", "STEP3_CALLBACK_MATCH", "STEP4_SESSION_PRESERVED"} {
		if result[key] != "yes" {
			failures = append(failures, fmt.Errorf("Authorization Code stage check %s failed", key))
		}
	}
	return errors.Join(failures...)
}

func validateAuthCodeSuccess(result map[string]string) error {
	failures := []error{validateAuthCodeCallback(result)}
	if result["STEP1_AFFINITY"] != "affinity" || result["STEP4_AFFINITY"] != "affinity" || !knownAuthCodeBackend(result["STEP4_BACKEND"]) || result["STEP1_BACKEND"] != result["STEP4_BACKEND"] {
		failures = append(failures, fmt.Errorf("affinity callback must demonstrably return to the initiating backend"))
	}
	switch result["STEP4_STATUS"] {
	case "200", "302", "303":
	default:
		failures = append(failures, fmt.Errorf("callback must return HTTP 200, 302, or 303"))
	}
	if result["STEP4_RESTARTED"] != "no" {
		failures = append(failures, fmt.Errorf("callback restarted authentication or did not report its redirect state"))
	}
	if result["STEP5_SESSION_PRESERVED"] != "yes" || result["STEP5_BACKEND"] != result["STEP1_BACKEND"] {
		failures = append(failures, fmt.Errorf("final identity request must retain the session and return to the initiating backend"))
	}
	if result["STEP5_STATUS"] != "200" || result["STEP5_IDENTITY_MATCH"] != "yes" {
		failures = append(failures, fmt.Errorf("protected request after callback must return HTTP 200 and the expected authenticated identity"))
	}
	return errors.Join(failures...)
}

func validateAuthCodeRejection(result map[string]string) error {
	failures := []error{validateAuthCodeCallback(result)}
	if result["STEP1_AFFINITY"] != "no-affinity" || result["STEP4_AFFINITY"] != "no-affinity" || !knownAuthCodeBackend(result["STEP4_BACKEND"]) || result["STEP1_BACKEND"] == result["STEP4_BACKEND"] {
		failures = append(failures, fmt.Errorf("negative case requires verified different backends through the no-affinity proxy; cross-node delivery is unverified"))
	}
	switch result["STEP4_STATUS"] {
	case "400", "401", "403", "500":
	default:
		failures = append(failures, fmt.Errorf("cross-node callback must return an authentication error status"))
	}
	if result["STEP4_ERROR"] != "NOVEL_OAUTH_STATE" {
		failures = append(failures, fmt.Errorf("cross-node callback must report XDMP-OAUTH Novel OAuth state / potential CSRF attack; unrelated errors are not expected rejections"))
	}
	return errors.Join(failures...)
}
