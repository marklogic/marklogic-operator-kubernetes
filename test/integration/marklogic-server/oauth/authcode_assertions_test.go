// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import "fmt"

// Validate only sanitized stage markers: raw authentication responses can contain
// authorization codes, state, session cookies, and tokens.
func validateAuthCodeStart(result map[string]string) error {
	if result["STEP1_STATUS"] != "302" && result["STEP1_STATUS"] != "303" {
		return fmt.Errorf("initial request must return HTTP 302 or 303")
	}
	for _, key := range []string{"STEP1_SESSIONID_PRESENT", "STEP1_PKCE_PRESENT", "STEP1_IDP_MATCH"} {
		if result[key] != "yes" {
			return fmt.Errorf("initial redirect check %s failed", key)
		}
	}
	return nil
}

func validateAuthCodeCallback(result map[string]string) error {
	if err := validateAuthCodeStart(result); err != nil {
		return err
	}
	if result["RESULT"] != "DONE" {
		return fmt.Errorf("Authorization Code flow did not complete the required stages")
	}
	for _, key := range []string{"STEP2_ACTION_PRESENT", "STEP3_CODE_PRESENT", "STEP3_STATE_PRESENT", "STEP3_CALLBACK_MATCH"} {
		if result[key] != "yes" {
			return fmt.Errorf("Authorization Code stage check %s failed", key)
		}
	}
	return nil
}

func validateAuthCodeSuccess(result map[string]string) error {
	if err := validateAuthCodeCallback(result); err != nil {
		return err
	}
	switch result["STEP4_STATUS"] {
	case "200", "302", "303":
	default:
		return fmt.Errorf("callback must return HTTP 200, 302, or 303")
	}
	if result["STEP4_RESTARTED"] != "no" {
		return fmt.Errorf("callback restarted authentication or did not report its redirect state")
	}
	if result["STEP5_STATUS"] != "200" || result["STEP5_IDENTITY_MATCH"] != "yes" {
		return fmt.Errorf("protected request after callback must return HTTP 200 and the expected authenticated identity")
	}
	return nil
}

func validateAuthCodeRejection(result map[string]string) error {
	if err := validateAuthCodeCallback(result); err != nil {
		return err
	}
	switch result["STEP4_STATUS"] {
	case "400", "401", "403", "500":
	default:
		return fmt.Errorf("cross-node callback must return an authentication error status")
	}
	if result["STEP4_ERROR"] != "NOVEL_OAUTH_STATE" {
		return fmt.Errorf("cross-node callback must report XDMP-OAUTH Novel OAuth state / potential CSRF attack; unrelated errors are not expected rejections")
	}
	return nil
}
