// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTokenReadbackRejectsAmbiguousResponses(t *testing.T) {
	for _, body := range []string{
		``, `null`, `[]`, `{}`, `{"type":"aws"}`, `{"type":"azure","access-key":"a","secret-key":"s"}`,
		`{"type":"aws","access-key":"a","secret-key":"s","session-token":null}`,
		`{"type":"aws","access-key":"a","secret-key":"s","session-token":123}`,
		`{"type":"aws","access-key":"a","secret-key":"s","session-token":"SECRET-sentinel","session-token":""}`,
		`{"type":"aws","access-key":"a","secret-key":"s","access-key":""}`,
		`{"type":"aws","access-key":"a","secret-key":"s"} {}`,
		`{"aws":{"access-key":"a","secret-key":"s"},"azure":{}}`,
		`{"aws":{"type":"azure","access-key":"a","secret-key":"s"}}`,
		`{"aws":{"access-key":"a","secret-key":"s","session-token":"SECRET-sentinel","session-token":""}}`,
		`{"credential-properties":{"type":"aws","access-key":"a","secret-key":"s"}}`,
		`{"type":"aws","access-key":"a","secret-key":" "}`,
		`{"type":"aws","access-key":"a","secret-key":null}`,
		`{"type":"aws","access-key":"a","secret-key":"s","unknown":"SECRET-sentinel"}`,
		strings.Repeat("x", credentialReadLimit+1),
	} {
		if _, err := parseAWSToken([]byte(body)); err == nil {
			t.Fatal("unrecognized response falsely passed token readback (body suppressed)")
		} else if strings.Contains(err.Error(), "SECRET-sentinel") {
			t.Fatal("parser error exposed response material")
		}
	}
}

func TestTokenReadbackRecognizesPresenceAndClearing(t *testing.T) {
	for _, tc := range []struct{ body, expected string }{
		{`{"type":"aws","access-key":"a","secret-key":"s","session-token":"secret-token"}`, "secret-token"},
		{`{"type":"aws","access-key":"a","secret-key":"s","session-token":""}`, ""},
		{`{"type":"aws","access-key":"a","secret-key":"s"}`, ""},
		{`{"aws":{"access-key":"a","secret-key":"s","session-token":"secret-token"}}`, "secret-token"},
		{`{"type":"aws","aws":{"access-key":"a","secret-key":"s"}}`, ""},
	} {
		got, err := parseAWSToken([]byte(tc.body))
		if err != nil || got != tc.expected {
			t.Fatal("valid AWS token state was not recognized (values suppressed)")
		}
	}
}

func TestCredentialOutputIsBoundedThroughIOCopy(t *testing.T) {
	output := &boundedCredentialOutput{limit: 8}
	if _, ok := any(output).(io.ReaderFrom); ok {
		t.Fatal("output exposes ReaderFrom, bypassing the write limit")
	}
	_, err := io.Copy(output, struct{ io.Reader }{strings.NewReader("secret-overflow")})
	if err == nil || !output.overflow || len(output.Bytes()) > 8 {
		t.Fatal("stream capture did not enforce its byte limit")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", output, output), "secret-overflow") {
		t.Fatal("output formatting disclosed content")
	}
}

func TestCredentialHTTPResponseErrorsAreSanitized(t *testing.T) {
	for _, output := range []string{"SECRET-body\n401", "SECRET-body\n500", "SECRET-body\n000", "SECRET-body\nxx1", "SECRET-body", "SECRET-body\n200\n"} {
		if _, err := credentialResponse([]byte(output), 200); err == nil {
			t.Fatal("failed or malformed response accepted")
		} else if strings.Contains(err.Error(), "SECRET-body") {
			t.Fatal("HTTP error leaked response body")
		}
	}
	if data, err := credentialResponse([]byte("{}\n200"), 200); err != nil || string(data) != "{}" {
		t.Fatal("valid response status rejected")
	}
	if _, err := credentialResponse([]byte(strings.Repeat("x", credentialReadLimit+1)+"\n200"), 200); err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestCurlConfigurationKeepsSensitiveMaterialOnStdin(t *testing.T) {
	identity := managementIdentity{username: "reader", password: `secret-"slash\sentinel`}
	body, _ := json.Marshal(map[string]string{"password": identity.password})
	config, err := curlConfig(identity, body)
	if err != nil {
		t.Fatal("valid config rejected")
	}
	// Four complete lines, with a quoted JSON body; quoting cannot inject curl options.
	if strings.Count(config, "\n") != 4 || !strings.Contains(config, `user = "reader:secret-\"slash\\sentinel"`) {
		t.Fatal("curl config escaping failed (contents suppressed)")
	}
	for _, bad := range []managementIdentity{{"reader", "secret\nverbose"}, {"reader", "secret\rtrace"}, {"reader:extra", "secret"}, {"reader", "secret\x00"}} {
		if _, err := curlConfig(bad, nil); err == nil {
			t.Fatal("unsafe config input accepted")
		}
	}
	if strings.Contains(fmt.Sprintf("%v %#v", identity, identity), "sentinel") {
		t.Fatal("identity formatting leaked material")
	}
}

func TestTokenClearingPrerequisitesDoNotTreatStaticAsTemporary(t *testing.T) {
	f := fixtureExample()
	if !tokenClearingAvailable(&f) {
		t.Fatal("temporary-to-static fixture was not admitted")
	}
	f.AWS.Initial["sessionToken"] = " "
	if tokenClearingAvailable(&f) {
		t.Fatal("empty initial token cannot prove clearing")
	}
	f.AWS.Initial["sessionToken"] = "token"
	f.AWS.Rotated["sessionToken"] = "other"
	if tokenClearingAvailable(&f) {
		t.Fatal("temporary-to-temporary rotation cannot prove clearing")
	}
	if tokenClearingAvailable(nil) {
		t.Fatal("absent fixture cannot prove clearing")
	}
}
