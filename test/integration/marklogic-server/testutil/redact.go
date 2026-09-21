// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|cookie|credential|private.?key|client.?secret|session.?id|^state$|^code$)`)
var authHeader = regexp.MustCompile(`(?im)(authorization|proxy-authorization|set-cookie|cookie)\s*:\s*[^\r\n]+`)
var quotedCredential = regexp.MustCompile(`(?i)("(?:[^"\n]*(?:password|secret|token|credential|cookie|session)[^"\n]*|code|state)"\s*:\s*)"(?:\\.|[^"\\])*"`)
var parameterCredential = regexp.MustCompile(`(?i)((?:password|passwd|client_secret|access_token|refresh_token|id_token|token|sessionid|session_code|code|state)\s*[=:]\s*)[^\s&;,"'<>]+`)
var jwtToken = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var authValue = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/-]+=*`)
var oauthStateError = regexp.MustCompile(`(?i)(Novel OAuth state)[^\r\n]*(potential CSRF attack)`)
var privateKey = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
var urlCredentials = regexp.MustCompile(`(https?://)[^/\s@]+:[^/\s@]+@`)

type redactor struct {
	mu    sync.Mutex
	known map[string]bool
}

func newRedactor() *redactor { return &redactor{known: map[string]bool{}} }
func (r *redactor) add(value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known[value] = true
	r.known[base64.StdEncoding.EncodeToString([]byte(value))] = true
}
func (r *redactor) text(value string) string {
	r.mu.Lock()
	known := make([]string, 0, len(r.known))
	for s := range r.known {
		known = append(known, s)
	}
	r.mu.Unlock()
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, s := range known {
		value = strings.ReplaceAll(value, s, "[REDACTED]")
	}
	value = privateKey.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	value = authHeader.ReplaceAllString(value, "$1: [REDACTED]")
	value = quotedCredential.ReplaceAllString(value, "${1}\"[REDACTED]\"")
	value = parameterCredential.ReplaceAllString(value, "${1}[REDACTED]")
	value = jwtToken.ReplaceAllString(value, "[REDACTED JWT]")
	value = authValue.ReplaceAllString(value, "${1} [REDACTED]")
	value = oauthStateError.ReplaceAllString(value, "${1} [REDACTED]; ${2}")
	return urlCredentials.ReplaceAllString(value, "${1}[REDACTED]@")
}

// Register credentials from typed fixture data and JSON embedded in ConfigMaps.
// Full manifests, annotations, container arguments and env values are never saved.
func (r *redactor) discover(value any) {
	switch value := value.(type) {
	case map[string]any:
		if kind, ok := value["type"].(string); ok && sensitiveKey.MatchString(kind) {
			if secret, ok := value["value"].(string); ok {
				r.add(secret)
			}
		}
		if name, ok := value["name"].(string); ok && sensitiveKey.MatchString(name) {
			if secret, ok := value["value"].(string); ok {
				r.add(secret)
			}
		}
		for key, item := range value {
			if sensitiveKey.MatchString(key) {
				if secret, ok := item.(string); ok {
					r.add(secret)
				}
			}
			r.discover(item)
		}
	case []any:
		for _, item := range value {
			r.discover(item)
		}
	case string:
		var nested any
		if json.Unmarshal([]byte(value), &nested) == nil {
			switch nested.(type) {
			case map[string]any, []any:
				r.discover(nested)
			}
		}
	}
}

func trimOutput(value []byte) string { return strings.TrimSpace(string(value)) }
