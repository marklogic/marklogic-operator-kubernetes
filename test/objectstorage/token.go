// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

const credentialReadLimit = 64 * 1024

// managementIdentity must never be formatted with its username/password.
type managementIdentity struct{ username, password string }

func (managementIdentity) String() string   { return "<redacted management identity>" }
func (managementIdentity) GoString() string { return "<redacted management identity>" }

func tokenClearingAvailable(f *fixture) bool {
	return f != nil && strings.TrimSpace(f.AWS.Initial["sessionToken"]) != "" && strings.TrimSpace(f.AWS.Rotated["sessionToken"]) == ""
}

func (s *scenario) tokenClearingScenarios(t *testing.T, f *fixture) bool {
	return t.Run("OS-T02_session_token_clearing", func(t *testing.T) {
		if !tokenClearingAvailable(f) {
			t.Skip("BLOCKED: token clearing requires AWS initial temporary credentials with a nonempty sessionToken and rotated static credentials without a token")
		}
		// Reads use a distinct identity. The controller itself still has no credential GET/DELETE.
		verifier := s.createCredentialVerifier(t)
		for _, variant := range []string{"absent_key", "empty_value"} {
			if !t.Run(variant, func(t *testing.T) {
				awsName := s.name + "-aws"
				azureBefore := s.waitReady(t, testutils.ObjectStorageAzure)
				// Re-establish the known token before EACH clearing variant, including after a
				// previous successful clear. Temporary/static keys come from dedicated fixtures.
				s.updateSecret(t, awsName, func(secret *corev1.Secret) { secret.Data = secretData(f.AWS.Initial) })
				before := s.waitReady(t, testutils.ObjectStorageAWS)
				s.assertStoredToken(t, verifier, strings.TrimSpace(f.AWS.Initial["sessionToken"]))
				values := secretData(f.AWS.Rotated)
				delete(values, "sessionToken")
				if variant == "empty_value" {
					values["sessionToken"] = []byte("")
				}
				s.updateSecret(t, awsName, func(secret *corev1.Secret) { secret.Data = values })
				after := s.waitReady(t, testutils.ObjectStorageAWS)
				if before.AppliedFingerprint == after.AppliedFingerprint || before.AppliedSecret.ResourceVersion == after.AppliedSecret.ResourceVersion {
					t.Fatal("temporary-to-static rotation did not record fresh changed material")
				}
				s.assertStoredToken(t, verifier, "")
				assertSameSuccess(t, azureBefore, s.waitReady(t, testutils.ObjectStorageAzure))
			}) {
				return
			}
		}
	})
}

func (s *scenario) createCredentialVerifier(t *testing.T) managementIdentity {
	t.Helper()
	adminSecret := &corev1.Secret{}
	if s.client.Get(s.ctx, s.key(s.admin), adminSecret) != nil {
		t.Fatal("cannot resolve bootstrap identity for verifier setup")
	}
	admin := managementIdentity{username: string(adminSecret.Data["username"]), password: string(adminSecret.Data["password"])}
	if admin.username == "" || admin.password == "" {
		t.Fatal("bootstrap identity is incomplete")
	}
	var password [24]byte
	if _, err := rand.Read(password[:]); err != nil {
		t.Fatal("cannot generate verifier identity")
	}
	verifier := managementIdentity{username: s.name + "-reader", password: hex.EncodeToString(password[:])}
	s.values = append(s.values, verifier.password)
	role := s.name + "-credential-read"
	roleBody, _ := json.Marshal(map[string]any{
		"role-name": role,
		"role":      []string{"manage"},
		"privilege": []map[string]string{{"privilege-name": "credentials-get-aws", "action": "http://marklogic.com/xdmp/privileges/credentials-get-aws", "kind": "execute"}},
	})
	// Register cleanup before each POST: a lost response can leave a successfully
	// created identity behind. These are security-resource operations, never a
	// credential PUT or DELETE. LIFO cleanup removes the user before its role.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.managementRequest(ctx, admin, http.MethodDelete, "/manage/v2/roles/"+url.PathEscape(role), nil, http.StatusNoContent); err != nil {
			t.Errorf("verifier role cleanup failed (details suppressed); disposable cluster cleanup remains required")
		}
	})
	if _, err := s.managementRequest(s.ctx, admin, http.MethodPost, "/manage/v2/roles?format=json", roleBody, http.StatusCreated); err != nil {
		t.Fatalf("create verifier role: %v", err)
	}
	userBody, _ := json.Marshal(map[string]any{"user-name": verifier.username, "password": verifier.password, "role": []string{role}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.managementRequest(ctx, admin, http.MethodDelete, "/manage/v2/users/"+url.PathEscape(verifier.username), nil, http.StatusNoContent); err != nil {
			t.Errorf("verifier user cleanup failed (details suppressed); disposable cluster cleanup remains required")
		}
	})
	if _, err := s.managementRequest(s.ctx, admin, http.MethodPost, "/manage/v2/users?format=json", userBody, http.StatusCreated); err != nil {
		t.Fatalf("create verifier user: %v", err)
	}
	return verifier
}

func (s *scenario) assertStoredToken(t *testing.T, identity managementIdentity, expected string) {
	t.Helper()
	body, err := s.managementRequest(s.ctx, identity, http.MethodGet, "/manage/v2/credentials/properties?type=aws&format=json", nil, http.StatusOK)
	if err != nil {
		t.Fatalf("credential readback: %v", err)
	}
	token, err := parseAWSToken(body)
	if err != nil {
		t.Fatal(err)
	}
	if token != expected {
		t.Fatal("stored session-token did not match the expected presence/value (all values suppressed)")
	}
}

// managementRequest captures raw responses only in bounded memory. No shell, local files, raw
// transport errors, curl stderr, or credential arguments are exposed. --config - receives the
// identity and POST body over stdin; --disable prevents a pod's curlrc from enabling verbose logs.
func (s *scenario) managementRequest(parent context.Context, identity managementIdentity, method, path string, body []byte, expected int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	config, err := curlConfig(identity, body)
	if err != nil {
		return nil, err
	}
	command := []string{"curl", "--disable", "--silent", "--show-error", "--digest", "--noproxy", "*", "--connect-timeout", "5", "--max-time", "30", "--max-filesize", strconv.Itoa(credentialReadLimit), "--config", "-", "--request", method, "--url", "http://127.0.0.1:8002" + path, "--write-out", "\n%{http_code}"}
	req := s.kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(s.opts.Namespace).Name(s.group+"-0").SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: "marklogic-server", Command: command, Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(s.restConfig, http.MethodPost, req.URL())
	if err != nil {
		return nil, errors.New("management verification transport could not be initialized")
	}
	stdout := &boundedCredentialOutput{limit: credentialReadLimit + 4}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: strings.NewReader(config), Stdout: stdout, Stderr: io.Discard})
	if err != nil || stdout.overflow {
		return nil, errors.New("management verification transport failed or response exceeded its size limit")
	}
	return credentialResponse(stdout.Bytes(), expected)
}

func curlConfig(identity managementIdentity, body []byte) (string, error) {
	// curl config escaping is deliberately limited to generated/simple authentication material.
	// Request JSON may contain arbitrary escaped strings; escape it as a single config value.
	quote := func(value string) (string, error) {
		if strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New("verification input contains unsupported control characters")
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`, nil
	}
	if strings.Contains(identity.username, ":") {
		return "", errors.New("verification username contains an unsupported delimiter")
	}
	user, err := quote(identity.username + ":" + identity.password)
	if err != nil {
		return "", err
	}
	config := "user = " + user + "\nheader = \"Accept: application/json\"\n"
	if body != nil {
		data, err := quote(string(body))
		if err != nil {
			return "", err
		}
		config += "header = \"Content-Type: application/json\"\ndata = " + data + "\n"
	}
	return config, nil
}

type boundedCredentialOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedCredentialOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.overflow = true
		return 0, errors.New("verification output exceeded its size limit")
	}
	return b.buffer.Write(p)
}
func (b *boundedCredentialOutput) Bytes() []byte  { return b.buffer.Bytes() }
func (*boundedCredentialOutput) String() string   { return "<redacted verification output>" }
func (*boundedCredentialOutput) GoString() string { return "<redacted verification output>" }

func credentialResponse(output []byte, expected int) ([]byte, error) {
	index := bytes.LastIndexByte(output, '\n')
	if index < 0 || len(output)-index != 4 {
		return nil, errors.New("management verification response has no valid HTTP status")
	}
	status, err := strconv.Atoi(string(output[index+1:]))
	if err != nil || status < 100 || status > 599 {
		return nil, errors.New("management verification response has no valid HTTP status")
	}
	if status != expected {
		return nil, fmt.Errorf("management verification returned HTTP %d (response suppressed)", status)
	}
	if index > credentialReadLimit {
		return nil, errors.New("management verification response exceeded its size limit")
	}
	return output[:index], nil
}

// parseAWSToken accepts only explicit AWS objects, never an arbitrary body missing a token.
// MarkLogic documents AWS fields but not one concrete JSON envelope; accept a flat type=aws
// object or an explicit aws object, and fail closed on any other shape until live validation.
func parseAWSToken(data []byte) (string, error) {
	invalid := errors.New("credential readback has an unrecognized or incomplete AWS response (body suppressed)")
	if len(data) > credentialReadLimit {
		return "", invalid
	}
	root, err := uniqueJSONObject(data)
	if err != nil {
		return "", invalid
	}
	isAWS := func(raw json.RawMessage) bool {
		var kind string
		return json.Unmarshal(raw, &kind) == nil && kind == "aws"
	}
	var properties map[string]json.RawMessage
	if aws, ok := root["aws"]; ok {
		for key := range root {
			if key != "aws" && key != "type" {
				return "", invalid
			}
		}
		if kind, exists := root["type"]; exists && !isAWS(kind) {
			return "", invalid
		}
		properties, err = uniqueJSONObject(aws)
		if err != nil {
			return "", invalid
		}
	} else {
		if !isAWS(root["type"]) {
			return "", invalid
		}
		properties = root
	}
	if kind, exists := properties["type"]; exists && !isAWS(kind) {
		return "", invalid
	}
	for key := range properties {
		if key != "type" && key != "access-key" && key != "secret-key" && key != "session-token" {
			return "", invalid
		}
	}
	for _, key := range []string{"access-key", "secret-key"} {
		var value string
		if raw, ok := properties[key]; !ok || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
			return "", invalid
		}
	}
	var token string
	if raw, ok := properties["session-token"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &token) != nil {
			return "", invalid
		}
	}
	return token, nil
}

func uniqueJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid JSON object")
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid JSON field")
		}
		if _, exists := result[key]; exists {
			return nil, errors.New("duplicate JSON field")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid JSON value")
		}
		result[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid JSON object ending")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return result, nil
}
