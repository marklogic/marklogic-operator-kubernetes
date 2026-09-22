// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// These tests run against a real MarkLogic Management API rather than a stub, and
// are skipped unless ML_MANAGE_ENDPOINT is set. They live in this package so they
// can reuse the client's digest authentication for the verification reads; the
// Manage app server rejects basic auth.
//
//	ML_MANAGE_ENDPOINT=localhost:18002 \
//	ML_MANAGE_USERNAME=admin ML_MANAGE_PASSWORD=admin123 \
//	go test ./pkg/mlmanage/... -run TestLive -count=1 -v
func liveClient(t *testing.T) *managementClient {
	t.Helper()

	endpoint := strings.TrimSpace(os.Getenv("ML_MANAGE_ENDPOINT"))
	if endpoint == "" {
		t.Skip("ML_MANAGE_ENDPOINT is not set; skipping live Management API test")
	}

	username := os.Getenv("ML_MANAGE_USERNAME")
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("ML_MANAGE_PASSWORD")
	if password == "" {
		password = "admin"
	}

	return &managementClient{
		baseURL:    buildBaseURL(endpoint, false),
		username:   username,
		password:   password,
		httpClient: buildHTTPClient(ClientOptions{}),
	}
}

func readLiveCredentials(t *testing.T, client *managementClient, provider string) map[string]any {
	t.Helper()

	query := url.Values{}
	query.Set("type", provider)
	query.Set("format", "json")

	data, _, err := client.doJSON(context.Background(), http.MethodGet, credentialsPropertiesPath, query, nil, http.StatusOK)
	if err != nil {
		t.Fatalf("failed to read %s credentials: %v", provider, err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("credentials response is not JSON: %v", err)
	}

	entry, _ := payload[provider].(map[string]any)
	return entry
}

// clearLiveCredentials removes a provider's credential set. DELETE requires a
// Content-Type header even though it carries no meaningful body, so an empty
// JSON object is sent.
func clearLiveCredentials(t *testing.T, client *managementClient, provider string) {
	t.Helper()

	query := url.Values{}
	query.Set("type", provider)

	_, status, err := client.doJSON(context.Background(), http.MethodDelete, credentialsPropertiesPath, query, map[string]any{}, http.StatusNoContent)
	if err != nil {
		t.Logf("cleanup of %s credentials returned status %d: %v", provider, status, err)
	}
}

func TestLiveEnsureCredentialsAppliesAndPersists(t *testing.T) {
	client := liveClient(t)

	t.Cleanup(func() {
		clearLiveCredentials(t, client, "aws")
		clearLiveCredentials(t, client, "azure")
	})
	clearLiveCredentials(t, client, "aws")
	clearLiveCredentials(t, client, "azure")

	const (
		accessKey      = "AKIAINTEGRATION01"
		secretKey      = "integrationSecretKeyValue123"
		storageAccount = "integrationacct"
		storageKey     = "aW50ZWdyYXRpb25rZXk="
	)

	if entry := readLiveCredentials(t, client, "aws"); entry != nil {
		t.Fatalf("expected AWS credentials to be unset before the test, got %v", entry)
	}

	if err := client.EnsureAWSCredentials(context.Background(), AWSCredentials{AccessKey: accessKey, SecretKey: secretKey}); err != nil {
		t.Fatalf("EnsureAWSCredentials failed: %v", err)
	}

	awsEntry := readLiveCredentials(t, client, "aws")
	if awsEntry == nil {
		t.Fatal("expected AWS credentials to be configured")
	}
	if got := awsEntry["access-key"]; got != accessKey {
		t.Fatalf("expected access-key %q, got %v", accessKey, got)
	}
	// MarkLogic stores the secret encrypted; a plaintext match would mean the
	// server is echoing the material back verbatim.
	if stored, _ := awsEntry["secret-key"].(string); stored == secretKey {
		t.Fatal("secret-key was returned in plaintext")
	}
	// session-token is never sent by this client.
	if token, present := awsEntry["session-token"]; present && token != "" {
		t.Fatalf("unexpected session-token in stored credentials: %v", token)
	}

	if err := client.EnsureAzureCredentials(context.Background(), AzureCredentials{StorageAccount: storageAccount, StorageKey: storageKey}); err != nil {
		t.Fatalf("EnsureAzureCredentials failed: %v", err)
	}

	azureEntry := readLiveCredentials(t, client, "azure")
	if azureEntry == nil {
		t.Fatal("expected Azure credentials to be configured")
	}
	if got := azureEntry["storage-account"]; got != storageAccount {
		t.Fatalf("expected storage-account %q, got %v", storageAccount, got)
	}
	if stored, _ := azureEntry["storage-key"].(string); stored == storageKey {
		t.Fatal("storage-key was returned in plaintext")
	}

	// Configuring Azure must not disturb AWS.
	if entry := readLiveCredentials(t, client, "aws"); entry == nil || entry["access-key"] != accessKey {
		t.Fatalf("AWS credentials were affected by the Azure write: %v", entry)
	}
}

// Re-applying identical material must be accepted, which is what makes the
// operator's skip-if-unchanged check an optimisation rather than a requirement.
func TestLiveEnsureCredentialsIsIdempotent(t *testing.T) {
	client := liveClient(t)
	t.Cleanup(func() { clearLiveCredentials(t, client, "aws") })

	credentials := AWSCredentials{AccessKey: "AKIAIDEMPOTENT01", SecretKey: "idempotentSecretKey123"}

	for attempt := 1; attempt <= 3; attempt++ {
		if err := client.EnsureAWSCredentials(context.Background(), credentials); err != nil {
			t.Fatalf("attempt %d failed: %v", attempt, err)
		}
	}

	entry := readLiveCredentials(t, client, "aws")
	if entry == nil || entry["access-key"] != credentials.AccessKey {
		t.Fatalf("unexpected stored credentials after repeated applies: %v", entry)
	}
}

// Rotation must be visible in the stored configuration.
func TestLiveEnsureCredentialsRotates(t *testing.T) {
	client := liveClient(t)
	t.Cleanup(func() { clearLiveCredentials(t, client, "aws") })

	first := AWSCredentials{AccessKey: "AKIAROTATEBEFORE", SecretKey: "beforeSecretKey123"}
	second := AWSCredentials{AccessKey: "AKIAROTATEAFTER0", SecretKey: "afterSecretKey456"}

	if err := client.EnsureAWSCredentials(context.Background(), first); err != nil {
		t.Fatalf("initial apply failed: %v", err)
	}
	if err := client.EnsureAWSCredentials(context.Background(), second); err != nil {
		t.Fatalf("rotation apply failed: %v", err)
	}

	entry := readLiveCredentials(t, client, "aws")
	if entry == nil || entry["access-key"] != second.AccessKey {
		t.Fatalf("expected rotated access-key %q, got %v", second.AccessKey, entry)
	}
}

// The insufficient-privilege path is the most likely misconfiguration, so confirm
// the live server really answers 403 and that it maps to a CredentialsError.
func TestLiveEnsureCredentialsRejectsBadPassword(t *testing.T) {
	client := liveClient(t)
	unauthorized := &managementClient{
		baseURL:    client.baseURL,
		username:   client.username,
		password:   client.password + "-wrong",
		httpClient: buildHTTPClient(ClientOptions{}),
	}

	err := unauthorized.EnsureAWSCredentials(context.Background(), AWSCredentials{
		AccessKey: "AKIAUNAUTHORIZED",
		SecretKey: "unauthorizedSecret123",
	})
	if err == nil {
		t.Fatal("expected an authentication failure")
	}

	var credentialsErr *CredentialsError
	if !errors.As(err, &credentialsErr) {
		t.Fatalf("expected *CredentialsError, got %T: %v", err, err)
	}
	if credentialsErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an invalid password, got %d", credentialsErr.StatusCode)
	}
}
