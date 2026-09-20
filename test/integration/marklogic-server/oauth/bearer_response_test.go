// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import "testing"

func TestValidateBearerResponse(t *testing.T) {
	for _, tc := range []struct {
		name, output, status, body string
		valid                      bool
	}{
		{"identity", "oauth-user:alice\n200", "200", "oauth-user:alice", true},
		{"newline", "oauth-user:alice\n\n200", "200", "oauth-user:alice", true},
		{"unauthorized", "error\nwith details\n401", "401", "", true},
		{"wrong identity", "oauth-user:bob\n200", "200", "oauth-user:alice", false},
		{"forbidden", "oauth-user:alice\n403", "200", "oauth-user:alice", false},
		{"redirect", "\n302", "200", "oauth-user:alice", false},
		{"gateway error", "\n503", "401", "", false},
		{"missing status", "200", "200", "", false},
		{"status in body", "200\n500", "200", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBearerResponse(tc.output, tc.status, tc.body)
			if (err == nil) != tc.valid {
				t.Fatalf("validation error = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestValidateBearerRejection(t *testing.T) {
	const missing = "XDMP-OAUTH: Access token provided is empty."
	const invalid = "XDMP-INTERNAL: Internal error: invalid token supplied"
	for _, tc := range []struct {
		name, output, expected string
		valid                  bool
	}{
		{"unauthorized", "denied\n401", missing, true},
		{"missing token", "<dt>" + missing + "</dt>\n500", missing, true},
		{"invalid token", "<dt>" + invalid + "</dt>\n500", invalid, true},
		{"header lost", "<dt>" + missing + "</dt>\n500", invalid, false},
		{"unrelated internal error", "<dt>XDMP-INTERNAL: unrelated failure</dt>\n500", invalid, false},
		{"generic 500", "internal server error\n500", missing, false},
		{"gateway error", "bad gateway\n502", missing, false},
		{"login redirect", "\n302", missing, false},
		{"unexpected access", "oauth-user:alice\n200", missing, false},
		{"missing status", "401", missing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBearerRejection(tc.output, tc.expected)
			if (err == nil) != tc.valid {
				t.Fatalf("validation error = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}
